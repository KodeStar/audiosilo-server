package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Sync v2 (Track W1, capability listening_sync): POST /me/listening/sync takes
// a device's outbox of listening events and answers a result per event, the
// server's time and the progress of every book touched. The transport checks
// the envelope and resolves each (library_id, path) the batch names, all reads
// before the one write; catalog.SyncListening decides everything else.

// listeningSyncRequest is the batch's envelope. Events are decoded one by one
// (catalog.DecodeSyncEvents), so a malformed event is that event's "invalid",
// never the batch's 400.
type listeningSyncRequest struct {
	DeviceID    string            `json:"device_id"`
	SentAt      *string           `json:"sent_at"`
	ClockOffset *float64          `json:"clock_offset"`
	Events      []json.RawMessage `json:"events"`
}

// syncPathReasons are the scope check's answers that reject an event rather
// than the batch.
var syncPathReasons = map[int]string{
	http.StatusBadRequest: catalog.SyncInvalid,
	http.StatusForbidden:  catalog.SyncNoAccess,
	http.StatusNotFound:   catalog.SyncNotFound,
}

// handleListeningSync applies one batch of listening events: 413 code
// too_large for a body over 1 MiB (the player halves its batch and sends
// again), 400 for a bad envelope (JSON or an unknown field; no device id, one
// over 200 bytes or an Audible account's ("audible:", the Bridge's route's
// alone); neither or both of sent_at and clock_offset, an unparseable sent_at
// or an offset beyond 1e9 s; code too_many_events past 500 events).
func (a *API) handleListeningSync(w http.ResponseWriter, r *http.Request) {
	var in listeningSyncRequest
	if !decodeSyncEnvelope(w, r, &in) {
		return
	}
	switch {
	case in.DeviceID == "":
		writeError(w, http.StatusBadRequest, "device_id is required")
		return
	case len(in.DeviceID) > catalog.MaxDeviceIDBytes:
		writeError(w, http.StatusBadRequest, "device_id is too long")
		return
	case strings.HasPrefix(in.DeviceID, catalog.AudibleDevicePrefix):
		writeError(w, http.StatusBadRequest, "device_id is reserved")
		return
	}
	var sentAt *time.Time
	switch {
	case (in.SentAt == nil) == (in.ClockOffset == nil),
		in.ClockOffset != nil && !(math.Abs(*in.ClockOffset) <= catalog.MaxClockOffset):
		writeError(w, http.StatusBadRequest, "send sent_at or clock_offset")
		return
	case in.SentAt != nil:
		t, err := time.Parse(time.RFC3339, *in.SentAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "send sent_at or clock_offset")
			return
		}
		sentAt = &t
	}
	if tooManySyncEvents(w, in.Events) {
		return
	}
	events := catalog.DecodeSyncEvents(in.Events)
	scopes, ok := a.callerScopes(w, r, "could not sync listening")
	if !ok {
		return
	}
	cred := credentialFrom(r.Context())
	resolve := func(libraryID int64, path string) (*catalog.Library, string, catalog.Scope, int, string) {
		return a.scopedPathIn(r, scopes, libraryID, path)
	}
	now := time.Now()
	transcoding := func(libraryID int64, key string) bool {
		return a.streams.Transcoding(cred.DeviceID, libraryID, key, now)
	}
	targets, ok := a.syncTargets(w, r, events, resolve, transcoding, nil)
	if !ok {
		return
	}
	u := userFrom(r.Context())
	// TokenID is the token the request acts for (a cast token's sender's,
	// auth.Credential.DeviceID), never the client's device_id.
	answer, err := a.cat.SyncListening(r.Context(), catalog.SyncBatch{
		UserID: u.ID, DeviceID: in.DeviceID, TokenID: cred.DeviceID, DeviceName: cred.DeviceName,
		Client: catalog.Client(cred.Client), SentAt: sentAt, ClockOffset: in.ClockOffset,
		Events: events, Targets: targets,
	})
	if err != nil {
		a.writeCatalogError(w, err, "listening sync failed", "could not sync listening", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// decodeSyncEnvelope decodes a Sync v2 batch's envelope into dst (a pointer):
// 413 code too_large for a body over 1 MiB, 400 for bad JSON or an unknown
// field, written here (false).
func decodeSyncEnvelope(w http.ResponseWriter, r *http.Request, dst any) bool {
	err := decodeJSON(r, dst, 0)
	if err == nil {
		return true
	}
	if tooBig := (*http.MaxBytesError)(nil); errors.As(err, &tooBig) {
		writeErrorCode(w, http.StatusRequestEntityTooLarge, codeTooLarge, "a batch is at most 1 MiB")
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid request")
	return false
}

// tooManySyncEvents answers 400 too_many_events for a batch past
// catalog.MaxSyncEvents (true).
func tooManySyncEvents(w http.ResponseWriter, events []json.RawMessage) bool {
	if len(events) <= catalog.MaxSyncEvents {
		return false
	}
	writeErrorCode(w, http.StatusBadRequest, "too_many_events", "a batch carries at most 500 events")
	return true
}

// syncResolver checks one event's (library_id, path) against the scopes of the
// user a batch is for, as scopedPathIn does: the library, the cleaned path and
// the scope, or the status and message of a refusal.
type syncResolver func(libraryID int64, path string) (*catalog.Library, string, catalog.Scope, int, string)

// syncTargets resolves every distinct (library_id, path) the events name, as
// PUT .../progress resolves its path: the scope check (resolve; 403 is the
// events' no_access, 404 their not_found, a path that cleans to nothing or is
// over catalog.MaxPathBytes their invalid), the progress key (progressPath: a
// file or part path keys its book), and the indexed book there with its files'
// offsets. allowed, when not nil, is every progress key the batch may write
// (an Audible account's synced books, catalog.AudibleSyncBooks), each with ""
// or the reason its events are rejected (duration_mismatch): any other key is
// not_synced. An event with
// no library or path is left out (catalog checks it invalid). A failed lookup
// is the batch's error, written here (false).
func (a *API) syncTargets(w http.ResponseWriter, r *http.Request, events []catalog.SyncEvent, resolve syncResolver,
	transcoding func(libraryID int64, key string) bool, allowed map[catalog.Ref]string) (map[catalog.Ref]catalog.SyncTarget, bool) {
	ctx := r.Context()
	targets := map[catalog.Ref]catalog.SyncTarget{}
	books := map[catalog.Ref]*catalog.SyncBook{} // by key: several sent paths may key one book
	for _, ev := range events {
		sent := catalog.Ref{LibraryID: ev.LibraryID, Path: ev.Path}
		if _, done := targets[sent]; done || ev.LibraryID <= 0 || ev.Path == "" {
			continue
		}
		lib, rel, scope, status, msg := resolve(ev.LibraryID, ev.Path)
		if reason, ok := syncPathReasons[status]; ok {
			targets[sent] = catalog.SyncTarget{Reason: reason}
			continue
		}
		if status != 0 {
			writeError(w, status, msg)
			return nil, false
		}
		key, err := a.progressPath(ctx, lib, scope, rel)
		if err != nil {
			a.writeCatalogError(w, err, "resolve progress path failed", "could not sync listening",
				"library", lib.ID, "path", rel)
			return nil, false
		}
		ref := catalog.Ref{LibraryID: lib.ID, Path: key}
		if allowed != nil {
			reason, ok := allowed[ref]
			if !ok {
				reason = catalog.SyncNotSynced
			}
			if reason != "" {
				targets[sent] = catalog.SyncTarget{Reason: reason}
				continue
			}
		}
		book, done := books[ref]
		if !done {
			if book, err = a.cat.SyncBookAt(ctx, lib.ID, key); err != nil {
				a.writeCatalogError(w, err, "read book for sync failed", "could not sync listening",
					"library", lib.ID, "path", key)
				return nil, false
			}
			books[ref] = book
		}
		targets[sent] = catalog.SyncTarget{
			Key:         ref,
			Exact:       book != nil && rel == key,
			Book:        book,
			Transcoding: transcoding != nil && transcoding(lib.ID, key),
		}
	}
	return targets, true
}

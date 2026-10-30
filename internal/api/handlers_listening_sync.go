package api

import (
	"encoding/json"
	"math"
	"net/http"
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

// maxSyncDeviceID is the longest device id a batch may carry (as a reading
// save's).
const maxSyncDeviceID = 200

// handleListeningSync applies one batch of listening events: 400 for a bad
// envelope (JSON, an unknown field or a body over 1 MiB; no device id or one
// over 200 bytes; neither or both of sent_at and clock_offset, an unparseable
// sent_at or an offset beyond 1e9 s; code too_many_events past 500 events).
func (a *API) handleListeningSync(w http.ResponseWriter, r *http.Request) {
	var in listeningSyncRequest
	if err := decodeJSON(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	switch {
	case in.DeviceID == "":
		writeError(w, http.StatusBadRequest, "device_id is required")
		return
	case len(in.DeviceID) > maxSyncDeviceID:
		writeError(w, http.StatusBadRequest, "device_id is too long")
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
	if len(in.Events) > catalog.MaxSyncEvents {
		writeErrorCode(w, http.StatusBadRequest, "too_many_events", "a batch carries at most 500 events")
		return
	}
	events, decoded := catalog.DecodeSyncEvents(in.Events)
	scopes, ok := a.callerScopes(w, r, "could not sync listening")
	if !ok {
		return
	}
	targets, ok := a.syncTargets(w, r, scopes, events)
	if !ok {
		return
	}
	u, cred := userFrom(r.Context()), credentialFrom(r.Context())
	answer, err := a.cat.SyncListening(r.Context(), catalog.SyncBatch{
		UserID: u.ID, DeviceID: in.DeviceID, TokenID: cred.DeviceID, DeviceName: cred.DeviceName,
		Client: catalog.Client(cred.Client), SentAt: sentAt, ClockOffset: in.ClockOffset,
		Events: events, Decoded: decoded, Targets: targets,
	})
	if err != nil {
		a.writeCatalogError(w, err, "listening sync failed", "could not sync listening", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// syncTargets resolves every distinct (library_id, path) the events name, as
// PUT .../progress resolves its path: the scope check (403 is the events'
// no_access, 404 their not_found, a path that cleans to nothing their invalid),
// the progress key (progressPath: a file or part path keys its book), and the
// indexed book there with its files' offsets. An event with no library or path
// is left out (catalog checks it invalid). A failed lookup is the batch's
// error, written here (false).
func (a *API) syncTargets(w http.ResponseWriter, r *http.Request, scopes []catalog.Scope, events []catalog.SyncEvent) (map[catalog.Ref]catalog.SyncTarget, bool) {
	ctx := r.Context()
	cred := credentialFrom(ctx)
	now := time.Now()
	targets := map[catalog.Ref]catalog.SyncTarget{}
	for _, ev := range events {
		sent := catalog.Ref{LibraryID: ev.LibraryID, Path: ev.Path}
		if _, done := targets[sent]; done || ev.LibraryID <= 0 || ev.Path == "" {
			continue
		}
		lib, rel, scope, status, msg := a.scopedPathIn(r, scopes, ev.LibraryID, ev.Path)
		switch status {
		case 0:
		case http.StatusBadRequest:
			targets[sent] = catalog.SyncTarget{Reason: catalog.SyncInvalid}
			continue
		case http.StatusForbidden:
			targets[sent] = catalog.SyncTarget{Reason: catalog.SyncNoAccess}
			continue
		case http.StatusNotFound:
			targets[sent] = catalog.SyncTarget{Reason: catalog.SyncNotFound}
			continue
		default:
			writeError(w, status, msg)
			return nil, false
		}
		key, err := a.progressPath(ctx, lib, scope, rel)
		if err != nil {
			a.writeCatalogError(w, err, "resolve progress path failed", "could not sync listening",
				"library", lib.ID, "path", rel)
			return nil, false
		}
		book, err := a.cat.SyncBookAt(ctx, lib.ID, key)
		if err != nil {
			a.writeCatalogError(w, err, "read book for sync failed", "could not sync listening",
				"library", lib.ID, "path", key)
			return nil, false
		}
		targets[sent] = catalog.SyncTarget{
			Key:         catalog.Ref{LibraryID: lib.ID, Path: key},
			Exact:       book != nil && rel == key,
			Book:        book,
			Transcoding: a.streams.Transcoding(cred.DeviceID, lib.ID, key, now),
		}
	}
	return targets, true
}

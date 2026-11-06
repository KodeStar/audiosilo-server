package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Bridge Phase 4c: an Audible account's places, both ways
// (catalog/audible_sync.go). The Bridge posts the account's places on Audible
// for its person (furthest only: never a move back) and reads the person's
// places here to push to Audible. Both act for the account's person, never for
// the paired admin: that person's scopes, their progress rows only, and the
// books of the account's synced titles only. A bridge token, header only, like
// the Bridge's other Audible routes; while the audible module or the account's
// sync is off, or the account is paused (its library deleted), both answer 403
// sync_off.

// audibleListeningRequest is a batch of an account's places: the Bridge's
// clock when it sent them and the events (listen spans of no length at
// Audible's place, finishes), each decoded on its own as a player's are.
type audibleListeningRequest struct {
	SentAt string            `json:"sent_at"`
	Events []json.RawMessage `json:"events"`
}

// writeAudibleSyncError answers a sync call's refusal: 404 unknown_account, 403
// sync_off (the module or the account's sync off, or the account paused), 403
// user_unavailable (the account's person is disabled, a kid or a demo account
// since it was mapped).
func (a *API) writeAudibleSyncError(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, catalog.ErrAudibleSyncOff):
		writeErrorCode(w, http.StatusForbidden, "sync_off", "sync with Audible is off for this account")
	case errors.Is(err, catalog.ErrAudibleSyncUser):
		writeErrorCode(w, http.StatusForbidden, "user_unavailable",
			"the account's person is disabled, a kid or a demo account: map the account to someone else")
	default:
		a.writeAudibleError(w, err, op)
	}
}

// audibleSyncFor is the account a sync call names, while the audible module
// and the account's sync are on and its person may be synced for, with that
// person's current scopes; otherwise it answers (writeAudibleSyncError) and
// reports false.
func (a *API) audibleSyncFor(w http.ResponseWriter, r *http.Request, account, op string) (*catalog.AudibleSyncAccount, []catalog.Scope, bool) {
	if !a.moduleOn(bridge.ModuleAudible) {
		a.writeAudibleSyncError(w, catalog.ErrAudibleSyncOff, op)
		return nil, nil, false
	}
	acct, err := a.cat.AudibleSyncFor(r.Context(), account)
	if err != nil {
		a.writeAudibleSyncError(w, err, op)
		return nil, nil, false
	}
	scopes, err := a.cat.UserScopes(r.Context(), acct.UserID, acct.Admin)
	if err != nil {
		a.writeCatalogError(w, err, op, "could not read the account's access", "account", account)
		return nil, nil, false
	}
	return acct, scopes, true
}

// userPathResolver is scopedPathIn for a person who is not the caller (an
// Audible account's): their scope in the library (none, or no such library, is
// 403), then pathInScope.
func (a *API) userPathResolver(r *http.Request, scopes []catalog.Scope) syncResolver {
	return func(libraryID int64, rawPath string) (*catalog.Library, string, catalog.Scope, int, string) {
		scope, ok := scopeIn(scopes, libraryID)
		if !ok || (!scope.AllowAll && len(scope.Paths) == 0) {
			return nil, "", scope, http.StatusForbidden, "no access to this library"
		}
		lib, err := a.cat.GetLibrary(r.Context(), libraryID)
		if errors.Is(err, catalog.ErrNotFound) {
			return nil, "", scope, http.StatusNotFound, "library not found"
		}
		if err != nil {
			return nil, "", scope, http.StatusInternalServerError, "could not load library"
		}
		return pathInScope(lib, scope, rawPath)
	}
}

// handleBridgeAudibleListening serves POST /bridge/audible/accounts/{account}/
// listening {sent_at, events}: the account's places on Audible for its person,
// as a furthest-only batch (catalog.SyncBatch.FurthestOnly: a place not ahead
// of the stored one is not_applied "behind") under device "audible:<account>"
// named "Audible (<label>)", recording no listening and nothing against the
// bridge token. An event on a book that is none of the account's synced titles
// is rejected not_synced, one whose titles' Audible runtimes all disagree with
// the indexed book's duration duration_mismatch (catalog.AudibleSyncBooks), one
// outside the person's access no_access. 200 the
// Sync v2 answer {server_time, results, progress}; 400 a bad envelope (as POST
// /me/listening/sync's, sent_at required); 413 too_large; 403 sync_off,
// user_unavailable; 404 unknown_account.
func (a *API) handleBridgeAudibleListening(w http.ResponseWriter, r *http.Request) {
	id, ok := audibleAccountID(w, r)
	if !ok {
		return
	}
	const op = "audible listening failed"
	acct, scopes, ok := a.audibleSyncFor(w, r, id, op)
	if !ok {
		return
	}
	var in audibleListeningRequest
	if !decodeSyncEnvelope(w, r, &in) {
		return
	}
	sentAt, err := time.Parse(time.RFC3339, in.SentAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "sent_at is required")
		return
	}
	if tooManySyncEvents(w, in.Events) {
		return
	}
	allowed, err := a.cat.AudibleSyncBooks(r.Context(), id)
	if err != nil {
		a.writeCatalogError(w, err, op, "could not sync listening", "account", id)
		return
	}
	events := catalog.DecodeSyncEvents(in.Events)
	targets, ok := a.syncTargets(w, r, events, a.userPathResolver(r, scopes), nil, allowed)
	if !ok {
		return
	}
	// FurthestOnly and TokenID 0 are this route's alone: no request body can
	// set either.
	answer, err := a.cat.SyncListening(r.Context(), catalog.SyncBatch{
		UserID: acct.UserID, DeviceID: acct.DeviceID(), TokenID: 0, DeviceName: acct.DeviceName(),
		SentAt: &sentAt, Events: events, Targets: targets, FurthestOnly: true,
	})
	if err != nil {
		a.writeCatalogError(w, err, op, "could not sync listening", "account", id)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// handleBridgeProgress serves GET /bridge/progress?account=&since=&limit=: a
// page of the places of the account's person that the Bridge may push to
// Audible (catalog.AudibleProgress: the books of the account's synced titles
// the person can still reach, rows the account itself wrote left out, oldest
// change first, after the cursor since). 200 {rows, next, more}: next is the
// cursor to send as since next time (kept between polls), more whether to ask
// again at once. 400 invalid_request (no account, a bad limit or cursor); 403
// sync_off, user_unavailable; 404 unknown_account.
func (a *API) handleBridgeProgress(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("account")
	if id == "" {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "account is required")
		return
	}
	if !bridge.AudibleAccountPattern.MatchString(id) {
		writeErrorCode(w, http.StatusNotFound, "unknown_account", "no such Audible account")
		return
	}
	limit, ok := positiveLimit(w, r)
	if !ok {
		return
	}
	const op = "audible progress failed"
	acct, scopes, ok := a.audibleSyncFor(w, r, id, op)
	if !ok {
		return
	}
	rows, next, more, err := a.cat.AudibleProgress(r.Context(), acct, scopes, q.Get("since"), limit)
	if err != nil {
		a.writeAudibleError(w, err, op)
		return
	}
	writeNoStore(w, http.StatusOK, map[string]any{"rows": rows, "next": next, "more": more})
}

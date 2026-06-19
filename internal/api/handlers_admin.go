package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

func (a *API) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.auth.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (a *API) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	u, err := a.auth.CreateUser(r.Context(), req.Username, req.Password, req.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (a *API) handleDisableUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var req struct {
		Disabled bool `json:"disabled"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.auth.SetDisabled(r.Context(), id, req.Disabled); err != nil {
		writeError(w, http.StatusInternalServerError, "could not update user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Invite-friendly defaults: an auth code minted from the admin console is meant
// to onboard a single person and may travel in a copy-invite URL, so bound the
// blast radius if it leaks. Callers that pass explicit max_uses/ttl_days keep
// full control (the first-run bootstrap code in cmd/audiosilo stays unbounded).
const (
	defaultAuthCodeMaxUses = 1
	defaultAuthCodeTTLDays = 7
)

// handleCreateAuthCode mints a redeemable auth code for a user. The code is
// returned once and only its hash is stored. The response also carries an
// invite_url that drops the recipient straight onto the connect/QR screen with
// the code pre-filled (the code rides in the URL fragment, so it never reaches
// the server or its logs).
func (a *API) handleCreateAuthCode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var req struct {
		Label   string `json:"label"`
		MaxUses int    `json:"max_uses"`
		TTLDays int    `json:"ttl_days"`
	}
	_ = decodeJSON(r, &req, 0)
	if req.MaxUses <= 0 {
		req.MaxUses = defaultAuthCodeMaxUses
	}
	if req.TTLDays <= 0 {
		req.TTLDays = defaultAuthCodeTTLDays
	}
	ttl := time.Duration(req.TTLDays) * 24 * time.Hour
	code, err := a.auth.CreateAuthCode(r.Context(), id, req.Label, req.MaxUses, ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create auth code")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"auth_code":  code,
		"invite_url": a.inviteURL(r, code),
	})
}

func (a *API) handleAdminListLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := a.cat.ListLibraries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list libraries")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": libs})
}

func (a *API) handleCreateLibrary(w http.ResponseWriter, r *http.Request) {
	var lib catalog.Library
	if err := decodeJSON(r, &lib, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if lib.Name == "" || lib.Root == "" {
		writeError(w, http.StatusBadRequest, "name and root are required")
		return
	}
	created, err := a.cat.CreateLibrary(r.Context(), lib)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Kick off an initial scan in the background; browsing works immediately.
	go a.backgroundScan(*created)
	writeJSON(w, http.StatusCreated, created)
}

// handleUpdateLibrary edits a library's mutable fields (name/root/layout/
// default_view). Because changing the layout or root invalidates the index, it
// kicks off a background rescan; browsing still works immediately.
func (a *API) handleUpdateLibrary(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	var in catalog.Library
	if err := decodeJSON(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	updated, err := a.cat.UpdateLibrary(r.Context(), id, in)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	go a.backgroundScan(*updated)
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteLibrary removes a library and everything indexed under it. The
// audio files on disk are not touched.
func (a *API) handleDeleteLibrary(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	if err := a.cat.DeleteLibrary(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete library")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleScanLibrary(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	lib, err := a.cat.GetLibrary(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	go a.backgroundScan(*lib)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "scan started"})
}

// handleScanStatus reports progress of the (possibly running) scan for a library
// so the admin UI can show a counter.
func (a *API) handleScanStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	writeJSON(w, http.StatusOK, a.scanner.Progress(id))
}

// backgroundScan runs a library scan detached from the request lifecycle.
func (a *API) backgroundScan(lib catalog.Library) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if _, err := a.scanner.Scan(ctx, lib); err != nil {
		a.log.Warn("background scan failed", "library", lib.Name, "err", err)
	}
}

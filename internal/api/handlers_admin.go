package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
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
		Password string `json:"password"` // optional for non-admins (auth-code pairing only)
		Role     string `json:"role"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// CreateUser enforces the password rules (required for admins, optional
	// otherwise), so the transport layer stays a thin pass-through.
	u, err := a.auth.CreateUser(r.Context(), req.Username, req.Password, req.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

// handleGetUserDetail returns one account plus everything the admin console
// needs to manage it: the libraries it can reach, the shares granted to it, and
// its issued auth codes (metadata only — the plaintext codes are never stored).
func (a *API) handleGetUserDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	u, err := a.auth.GetUser(r.Context(), id)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load user")
		return
	}
	isAdmin := u.Role == auth.RoleAdmin
	libs, err := a.cat.AccessibleLibraries(r.Context(), id, isAdmin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load access")
		return
	}
	shares, err := a.cat.UserShares(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load shares")
		return
	}
	codes, err := a.auth.ListAuthCodes(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load auth codes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":                 u,
		"accessible_libraries": libs,
		"shares":               shares,
		"auth_codes":           codes,
	})
}

// handleUpdateUser patches an account in place: role, password and/or disabled
// state (any subset). It replaces the old delete-and-recreate dance and the
// separate disable endpoint. Apply password before role so promoting a
// password-less account to admin in one request passes the admin-password guard.
func (a *API) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var req struct {
		Role     *string `json:"role"`
		Password *string `json:"password"`
		Disabled *bool   `json:"disabled"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Password != nil {
		if err := a.auth.SetPassword(r.Context(), id, *req.Password); err != nil {
			writeUserError(w, err)
			return
		}
	}
	if req.Role != nil {
		if err := a.auth.SetRole(r.Context(), id, *req.Role); err != nil {
			writeUserError(w, err)
			return
		}
	}
	if req.Disabled != nil {
		if err := a.auth.SetDisabled(r.Context(), id, *req.Disabled); err != nil {
			writeUserError(w, err)
			return
		}
	}
	u, err := a.auth.GetUser(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load user")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// writeUserError maps auth account-management errors to HTTP statuses.
func writeUserError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, auth.ErrLastAdmin):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrAdminNeedsPassword):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, auth.ErrPasswordTooShort):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "could not update user")
	}
}

// handleRevokeAuthCode deletes an issued auth code, immediately invalidating it.
func (a *API) handleRevokeAuthCode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid auth code id")
		return
	}
	if err := a.auth.RevokeAuthCode(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not revoke auth code")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Invite-friendly defaults: applied only when the request omits the field, so a
// caller (or the admin console) can still ask for unlimited uses / no expiry with
// an explicit 0. Bounded by default to limit the blast radius if a link leaks; the
// first-run bootstrap code in cmd/audiosilo calls CreateAuthCode directly and is
// unaffected.
const (
	defaultAuthCodeMaxUses = 5
	defaultAuthCodeTTLDays = 1
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
	// Pointers distinguish "omitted" (apply the default) from an explicit 0
	// (max_uses 0 = unlimited, ttl_days 0 = never expires) — both of which
	// CreateAuthCode supports.
	var req struct {
		Label   string `json:"label"`
		MaxUses *int   `json:"max_uses"`
		TTLDays *int   `json:"ttl_days"`
	}
	_ = decodeJSON(r, &req, 0)
	maxUses := defaultAuthCodeMaxUses
	if req.MaxUses != nil {
		maxUses = *req.MaxUses
	}
	if maxUses < 0 {
		maxUses = 0
	}
	ttlDays := defaultAuthCodeTTLDays
	if req.TTLDays != nil {
		ttlDays = *req.TTLDays
	}
	var ttl time.Duration
	if ttlDays > 0 {
		ttl = time.Duration(ttlDays) * 24 * time.Hour
	}
	code, err := a.auth.CreateAuthCode(r.Context(), id, req.Label, maxUses, ttl)
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

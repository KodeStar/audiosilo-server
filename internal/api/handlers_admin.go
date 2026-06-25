package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
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
	maxUses, ttl := resolveAuthCodeLifetime(req.MaxUses, req.TTLDays)
	// One active invite per user: minting atomically supersedes the user's other
	// still-redeemable invites (used-up/expired ones stay as history).
	code, err := a.auth.CreateInvite(r.Context(), id, req.Label, maxUses, ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create auth code")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"auth_code":  code,
		"invite_url": a.inviteURL(r, code),
	})
}

// resolveAuthCodeLifetime applies the invite-friendly defaults when a field is
// omitted; an explicit negative max_uses is clamped to 0 (unlimited).
func resolveAuthCodeLifetime(maxUsesPtr, ttlDaysPtr *int) (maxUses int, ttl time.Duration) {
	maxUses = defaultAuthCodeMaxUses
	if maxUsesPtr != nil {
		maxUses = *maxUsesPtr
	}
	if maxUses < 0 {
		maxUses = 0
	}
	ttlDays := defaultAuthCodeTTLDays
	if ttlDaysPtr != nil {
		ttlDays = *ttlDaysPtr
	}
	if ttlDays > 0 {
		ttl = time.Duration(ttlDays) * 24 * time.Hour
	}
	return maxUses, ttl
}

// handleRotateAuthCode regenerates an existing invite's secret in place and
// returns the new code + invite link once. This is the admin "Resend": the old
// link dies, no new row is created, and the invite is pending again. The invite's
// max_uses and lifetime window are preserved (the expiry is renewed for the same
// duration), so resending never silently downgrades a custom invite.
func (a *API) handleRotateAuthCode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid auth code id")
		return
	}
	code, err := a.auth.RotateAuthCode(r.Context(), id)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, http.StatusNotFound, "invite not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not rotate auth code")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_code":  code,
		"invite_url": a.inviteURL(r, code),
	})
}

// handleAdminClearRecovery revokes a user's durable recovery code. Recovery codes
// are user-owned and never surfaced as listable invites, so this is the admin's
// only lever to kill a leaked/compromised one (disabling the account only pauses
// it). No-op if the user has none.
func (a *API) handleAdminClearRecovery(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if err := a.auth.ClearRecoveryCode(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not clear recovery code")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		a.writeCatalogError(w, err, "create library failed", "could not create library", "name", lib.Name)
		return
	}
	// Kick off an initial scan in the background; browsing works immediately.
	go a.backgroundScan(*created)
	writeJSON(w, http.StatusCreated, created)
}

// handleUpdateLibrary edits a library's mutable fields (name/root/default_view).
// Because changing the root invalidates the index, it kicks off a background
// rescan; browsing still works immediately.
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
		a.writeCatalogError(w, err, "update library failed", "could not update library", "library", id)
		return
	}
	go a.backgroundScan(*updated)
	writeJSON(w, http.StatusOK, updated)
}

// handleReorderLibraries sets the libraries' display order from an ordered list
// of ids (position 0 first). That order is also the tiebreaker when the same book
// appears in more than one library: the copy in the earlier library wins de-dup.
func (a *API) handleReorderLibraries(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.cat.ReorderLibraries(r.Context(), req.IDs); err != nil {
		writeError(w, http.StatusInternalServerError, "could not reorder libraries")
		return
	}
	libs, err := a.cat.ListLibraries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list libraries")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": libs})
}

// handleSetFolderOverride forces how a folder is classified by the auto book
// detector ("book" = one multi-file book, "collection" = one book per file),
// then rescans the library so the change takes effect.
func (a *API) handleSetFolderOverride(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	lib, err := a.cat.GetLibrary(r.Context(), id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load library")
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if _, err := library.SafeJoin(lib.Root, path); err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// The {book, collection} allowlist is enforced once in catalog.SetFolderOverride
	// (returning ErrInvalidOverrideMode); the mapper turns that into a 400 here.
	if err := a.cat.SetFolderOverride(r.Context(), id, path, req.Mode); err != nil {
		a.writeCatalogError(w, err, "set folder override failed", "could not set folder override", "library", id, "path", path)
		return
	}
	go a.backgroundScan(*lib)
	writeJSON(w, http.StatusOK, map[string]any{"status": "override set", "path": path, "mode": req.Mode})
}

// handleDeleteFolderOverride clears a folder's detection override (reverting it
// to auto-detection) and rescans the library.
func (a *API) handleDeleteFolderOverride(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	lib, err := a.cat.GetLibrary(r.Context(), id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load library")
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if err := a.cat.DeleteFolderOverride(r.Context(), id, path); err != nil {
		writeError(w, http.StatusInternalServerError, "could not clear override")
		return
	}
	go a.backgroundScan(*lib)
	writeJSON(w, http.StatusOK, map[string]any{"status": "override cleared", "path": path})
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
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	if err != nil {
		a.log.Warn("scan library: load failed", "library", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not load library")
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

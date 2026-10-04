package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
	if req.Username == "" {
		writeError(w, http.StatusBadRequest, "username is required")
		return
	}
	// CreateUser enforces the password rules (required for admins, optional
	// otherwise); writeUserError maps its typed errors to the right status and
	// never echoes a raw DB error (a duplicate username must not leak the SQLite
	// constraint string).
	u, err := a.auth.CreateUser(r.Context(), req.Username, req.Password, req.Role)
	if err != nil {
		a.writeUserError(w, err, "could not create user")
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

// handleGetUserDetail returns one account plus everything the admin console
// needs to manage it: the libraries it can reach, the shares granted to it, and
// its issued auth codes (metadata only - the plaintext codes are never stored).
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
			a.writeUserError(w, err, "could not update user")
			return
		}
	}
	if req.Role != nil {
		if err := a.auth.SetRole(r.Context(), id, *req.Role); err != nil {
			a.writeUserError(w, err, "could not update user")
			return
		}
	}
	if req.Disabled != nil {
		if err := a.auth.SetDisabled(r.Context(), id, *req.Disabled); err != nil {
			a.writeUserError(w, err, "could not update user")
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

// handleDeleteUser permanently removes an account and all of its durable state
// (sessions, auth codes, progress/bookmarks/notes/history, share grants) via the
// schema's cascade. Two guards: an admin cannot delete their own account (disable
// it instead - prevents self-lockout and fat-finger loss), and auth.DeleteUser
// refuses the last enabled admin (ErrLastAdmin → 409).
func (a *API) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if caller := userFrom(r.Context()); caller != nil && caller.ID == id {
		writeErrorCode(w, http.StatusBadRequest, codeCannotDeleteSelf, "you cannot delete your own account - disable it instead")
		return
	}
	if err := a.auth.DeleteUser(r.Context(), id); err != nil {
		a.writeUserError(w, err, "could not delete user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeUserError maps auth account-management errors to HTTP statuses - the
// auth twin of writeCatalogError, and like it centralized so a newly added
// sentinel is handled once rather than falling through to 500 in the handlers
// that forgot to special-case it. Anything unmapped is logged and returned as a
// generic 500 with the caller-supplied message.
func (a *API) writeUserError(w http.ResponseWriter, err error, genericMsg string) {
	switch {
	case errors.Is(err, auth.ErrNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, auth.ErrUsernameTaken):
		writeErrorCode(w, http.StatusConflict, codeUsernameTaken, "username already taken")
	case errors.Is(err, auth.ErrLastAdmin):
		writeErrorCode(w, http.StatusConflict, codeLastAdmin, err.Error())
	case errors.Is(err, auth.ErrAdminNeedsPassword):
		writeErrorCode(w, http.StatusBadRequest, codeAdminNeedsPassword, err.Error())
	case errors.Is(err, auth.ErrPasswordTooShort):
		writeErrorCode(w, http.StatusBadRequest, codePasswordTooShort, err.Error())
	default:
		a.log.Warn(genericMsg, "err", err)
		writeError(w, http.StatusInternalServerError, genericMsg)
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
	// (max_uses 0 = unlimited, ttl_days 0 = never expires) - both of which
	// CreateAuthCode supports.
	var req struct {
		Label   string `json:"label"`
		MaxUses *int   `json:"max_uses"`
		TTLDays *int   `json:"ttl_days"`
	}
	// An empty body is fine - mint an invite with the defaults (decodeJSONOptional
	// still rejects a malformed body).
	if err := decodeJSONOptional(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	maxUses, ttl := resolveAuthCodeLifetime(req.MaxUses, req.TTLDays)
	// One active invite per user: minting atomically supersedes the user's other
	// still-redeemable invites (used-up/expired ones stay as history).
	minted, err := a.auth.CreateInvite(r.Context(), id, req.Label, maxUses, ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create auth code")
		return
	}
	writeJSON(w, http.StatusCreated, a.mintedInvite(r, minted))
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
	minted, err := a.auth.RotateAuthCode(r.Context(), id)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, http.StatusNotFound, "invite not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not rotate auth code")
		return
	}
	writeJSON(w, http.StatusOK, a.mintedInvite(r, minted))
}

// mintedInvite is the create/rotate response: the code and its invite link
// (returned this once) plus the lifetime the invite was given, so the console
// shows the server's numbers rather than re-deriving them.
func (a *API) mintedInvite(r *http.Request, m auth.Minted) map[string]any {
	out := map[string]any{
		"auth_code":  m.Code,
		"invite_url": a.inviteURL(r, m.Code),
		"max_uses":   m.MaxUses,
	}
	if m.ExpiresAt != "" {
		out["expires_at"] = m.ExpiresAt
	}
	return out
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

// adminLibrary is a library as the admin console lists it: the stored fields
// plus its indexed book count, whether its root folder is reachable right now
// (see library.Scanner.RootAvailable) and its scan progress, so one poll of the
// list shows every library's state.
type adminLibrary struct {
	catalog.Library
	BookCount int                  `json:"book_count"`
	Available bool                 `json:"available"`
	Scan      library.ScanProgress `json:"scan"`
	// The admin-only settings (off the player's library wire) and, with a schedule,
	// when the next scheduled scan is due.
	ScanSchedule   string   `json:"scan_schedule"`
	IgnorePatterns []string `json:"ignore_patterns"`
	NextScanAt     string   `json:"next_scan_at,omitempty"`
}

func (a *API) handleAdminListLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := a.adminLibraries(r.Context())
	if err != nil {
		a.log.Warn("list libraries failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list libraries")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": libs})
}

// adminLibraries lists every library with its book count, root availability
// and scan progress.
func (a *API) adminLibraries(ctx context.Context) ([]adminLibrary, error) {
	libs, err := a.cat.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := a.cat.CountBooksByLibrary(ctx)
	if err != nil {
		return nil, err
	}
	available := a.scanner.RootsAvailable(libs, counts)
	next, err := a.scanner.NextScans(ctx, libs)
	if err != nil {
		return nil, err
	}
	out := make([]adminLibrary, len(libs))
	for i, l := range libs {
		out[i] = adminLibrary{
			Library:        l,
			BookCount:      counts[l.ID],
			Available:      available[l.ID],
			Scan:           a.scanner.Progress(l.ID),
			ScanSchedule:   l.ScanSchedule,
			IgnorePatterns: l.IgnorePatterns,
		}
		if at, ok := next[l.ID]; ok {
			out[i].NextScanAt = at.UTC().Format(time.RFC3339)
		}
	}
	return out, nil
}

// handleListInvites lists every account's invite codes (metadata only: the codes
// themselves are never stored) for the console's Invites page.
func (a *API) handleListInvites(w http.ResponseWriter, r *http.Request) {
	invites, err := a.auth.ListInvites(r.Context())
	if err != nil {
		a.log.Warn("list invites failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list invites")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": invites})
}

// maxDirListing caps one folder-picker listing.
const maxDirListing = 1000

// handleListDirs is the add-library folder picker: the subfolders of an absolute
// server path (the filesystem root when ?path= is empty). Folder names only; see
// library.ListDirs for the bounds.
func (a *API) handleListDirs(w http.ResponseWriter, r *http.Request) {
	listing, err := library.ListDirs(r.URL.Query().Get("path"), maxDirListing)
	switch {
	case errors.Is(err, library.ErrNotAbsolute):
		writeErrorCode(w, http.StatusBadRequest, codePathNotAbsolute, "path must be absolute")
	case err != nil:
		// Missing, not a folder, or not readable by the server: the same answer,
		// and the OS error (which can name the path) stays out of the body.
		writeErrorCode(w, http.StatusNotFound, codeFolderUnreadable, "folder not found or not readable")
	default:
		writeJSON(w, http.StatusOK, listing)
	}
}

// libraryRequest is the body of a library create or edit. The scan settings are
// pointers so an edit that leaves them out keeps them.
type libraryRequest struct {
	Name           string    `json:"name"`
	Root           string    `json:"root"`
	DefaultView    string    `json:"default_view"`
	ScanSchedule   *string   `json:"scan_schedule"`
	IgnorePatterns *[]string `json:"ignore_patterns"`
}

// patch is the request as a validated edit (library.ValidatePatch's errors).
func (req libraryRequest) patch() (catalog.LibraryPatch, error) {
	p := catalog.LibraryPatch{Name: req.Name, Root: req.Root, DefaultView: req.DefaultView,
		ScanSchedule: req.ScanSchedule, IgnorePatterns: req.IgnorePatterns}
	return p, library.ValidatePatch(&p)
}

func (a *API) handleCreateLibrary(w http.ResponseWriter, r *http.Request) {
	var req libraryRequest
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Name == "" || req.Root == "" {
		writeError(w, http.StatusBadRequest, "name and root are required")
		return
	}
	p, err := req.patch()
	if err != nil {
		a.writeCatalogError(w, err, "create library failed", "could not create library")
		return
	}
	var lib catalog.Library
	p.Apply(&lib)
	created, err := a.cat.CreateLibrary(r.Context(), lib)
	if err != nil {
		a.writeCatalogError(w, err, "create library failed", "could not create library", "name", lib.Name)
		return
	}
	// Kick off an initial scan in the background; browsing works immediately.
	a.startScan(r, *created, library.TriggerManual)
	writeJSON(w, http.StatusCreated, created)
}

// handleUpdateLibrary edits a library: name, root, default view, scan schedule and
// ignore rules. A new root or new ignore rules make the index stale, so either
// queues a rescan, returned as `job`; browsing still works immediately.
func (a *API) handleUpdateLibrary(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	var req libraryRequest
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	p, err := req.patch()
	if err != nil {
		a.writeCatalogError(w, err, "update library failed", "could not update library", "library", id)
		return
	}
	updated, stale, err := a.cat.UpdateLibrary(r.Context(), id, p)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "update library failed", "could not update library", "library", id)
		return
	}
	resp := struct {
		*catalog.Library
		Job *library.Job `json:"job,omitempty"`
	}{Library: updated}
	if stale {
		job := a.startScan(r, *updated, library.TriggerChange)
		resp.Job = &job
	}
	writeJSON(w, http.StatusOK, resp)
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
	libs, err := a.adminLibraries(r.Context())
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
	a.startScan(r, *lib, library.TriggerChange)
	writeJSON(w, http.StatusOK, map[string]any{"status": "override set", "path": path, "mode": req.Mode})
}

// handleSetEnrichment attaches path-keyed metadata (ASIN/ISBN) to a book. The
// manager calls this after matching an external source (e.g. an Audible library) to
// an indexed book, so a book scanned without an ASIN gains one - making future
// matches exact. The enrichment is durable and survives a re-scan (UpsertBook
// re-applies it); no file on disk is modified, so the network API stays
// non-destructive.
func (a *API) handleSetEnrichment(w http.ResponseWriter, r *http.Request) {
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
		ASIN string `json:"asin"`
		ISBN string `json:"isbn"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if strings.TrimSpace(req.ASIN) == "" && strings.TrimSpace(req.ISBN) == "" {
		writeError(w, http.StatusBadRequest, "asin or isbn is required")
		return
	}
	if err := a.cat.SetEnrichment(r.Context(), id, path, req.ASIN, req.ISBN); err != nil {
		a.writeCatalogError(w, err, "set enrichment failed", "could not set enrichment", "library", id, "path", path)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "enrichment set", "path": path})
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
	a.startScan(r, *lib, library.TriggerChange)
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
	job := a.startScan(r, *lib, library.TriggerManual)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "scan started", "job": job})
}

// handleScanAll queues a scan of every library (the console's "Check again" and
// "Rescan every library"); the queue runs them one at a time.
func (a *API) handleScanAll(w http.ResponseWriter, r *http.Request) {
	var by *int64
	if u := userFrom(r.Context()); u != nil {
		by = &u.ID
	}
	jobs, err := a.scanner.EnqueueAll(r.Context(), library.TriggerManual, by)
	if err != nil {
		a.writeCatalogError(w, err, "scan all failed", "could not queue the scans")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"jobs": jobs})
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

// startScan queues a scan of lib in the job queue on behalf of the request's admin;
// the library reads as queued (or running) when the request returns.
func (a *API) startScan(r *http.Request, lib catalog.Library, trigger string) library.Job {
	var by *int64
	if u := userFrom(r.Context()); u != nil {
		by = &u.ID
	}
	return a.scanner.Enqueue(lib, trigger, by)
}

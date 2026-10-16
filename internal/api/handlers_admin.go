package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
)

// adminUser is an account as the admin console shows it: the /me fields plus the
// household member fields only the admin (and the owner's own list) sees.
// ManagedBy is the one field it replaces: the embedded managed_by with the
// owner's own typed name added, which the player's managed_by never carries.
type adminUser struct {
	*auth.User
	ManagedBy     *adminOwner `json:"managed_by"`
	MemberAccess  *string     `json:"member_access"` // "all" | "collections", null for a non-member
	CanAddMembers bool        `json:"can_add_members"`
	MembersCount  int         `json:"members_count"`
	DisplayName   string      `json:"display_name"`
}

// adminOwner is a member's owner as the console names them (display_name, else
// the username).
type adminOwner struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

func toAdminUser(u *auth.User) adminUser {
	v := adminUser{User: u, MemberAccess: u.MemberAccess, CanAddMembers: u.CanAddMembers,
		MembersCount: u.MembersCount, DisplayName: u.DisplayName}
	if o := u.ManagedBy; o != nil {
		v.ManagedBy = &adminOwner{ID: o.ID, Username: o.Username, DisplayName: o.DisplayName}
	}
	return v
}

// adminMember is one of a person's household members on their console page:
// the owner's list's member (auth.Member) with whether the account is disabled.
type adminMember struct {
	auth.Member
	Disabled bool `json:"disabled"`
}

func (a *API) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.auth.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list users")
		return
	}
	out := make([]adminUser, len(users))
	for i := range users {
		out[i] = toAdminUser(&users[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
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
	a.audit(r, "user.create", u.Username, map[string]any{"role": u.Role, "password": req.Password != ""})
	// Re-read, so the answer is the admin's user object like every other admin
	// user answer (adminUser): the stored defaults (can_add_members) included.
	full, err := a.auth.GetUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load user")
		return
	}
	writeJSON(w, http.StatusCreated, toAdminUser(full))
}

// handleGetUserDetail returns one account plus everything the admin console
// needs to manage it: the libraries it can reach (a household member's derived
// access included), the shares granted to it, its issued auth codes (metadata
// only - the plaintext codes are never stored), its own household members and,
// for a member on "collections", the owner's collections shared with it. A
// member's detail also carries what their owner reaches (owner_libraries,
// owner_is_admin), so the console's Access tab needs no second detail.
func (a *API) handleGetUserDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	u := a.lookupUser(w, r, id)
	if u == nil {
		return
	}
	libs, err := a.cat.CallerLibraries(r.Context(), callerOf(u))
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
	members, err := a.auth.ListMembers(r.Context(), id)
	if err != nil {
		a.log.Warn("list members failed", "user", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not load household members")
		return
	}
	own := make([]adminMember, len(members))
	for i, m := range members {
		own[i] = adminMember{Member: m, Disabled: m.Disabled}
	}
	shared, err := a.memberCollectionsOf(r.Context(), u)
	if err != nil {
		a.log.Warn("member collections failed", "user", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not load household members")
		return
	}
	out := map[string]any{
		"user":                 toAdminUser(u),
		"accessible_libraries": libs,
		"shares":               shares,
		"auth_codes":           codes,
		"members":              own,
		"member_collections":   shared,
	}
	if u.IsMember() {
		ownerLibs, ownerAdmin := []catalog.Library{}, false
		if rule := u.MemberRule; rule != nil {
			ownerAdmin = rule.OwnerAdmin
			if ownerLibs, err = a.cat.CallerLibraries(r.Context(), catalog.Caller{ID: rule.Owner, Admin: ownerAdmin}); err != nil {
				a.log.Warn("owner libraries failed", "user", id, "err", err)
				writeError(w, http.StatusInternalServerError, "could not load access")
				return
			}
		}
		out["owner_libraries"], out["owner_is_admin"] = ownerLibs, ownerAdmin
	}
	writeJSON(w, http.StatusOK, out)
}

// memberCollectionsOf is the owner's collections shared with u when u is a
// member on "collections" (counted as the owner's current access sees them),
// else [].
func (a *API) memberCollectionsOf(ctx context.Context, u *auth.User) ([]catalog.MemberCollection, error) {
	out := []catalog.MemberCollection{}
	r := u.MemberRule
	if r == nil || r.Access != auth.MemberCollections {
		return out, nil
	}
	shared, err := a.cat.MemberCollections(ctx, r.Owner, r.OwnerAdmin, []int64{u.ID})
	if err != nil {
		return nil, err
	}
	if got := shared[u.ID]; got != nil {
		out = got
	}
	return out, nil
}

// userPatch is PATCH /admin/users/{id}'s body. ManagedBy is raw so an absent
// field ("leave it"), null ("Make independent") and an id ("Assign to someone")
// differ.
type userPatch struct {
	Role          *string         `json:"role"`
	Password      *string         `json:"password"`
	Disabled      *bool           `json:"disabled"`
	CanAddMembers *bool           `json:"can_add_members"`
	ManagedBy     json.RawMessage `json:"managed_by"`
	MemberKind    *string         `json:"member_kind"`
	MemberAccess  *string         `json:"member_access"`
}

// owner reads ManagedBy: present, and the owner's id (0 for null). ok is false
// for anything but null or a whole number.
func (p userPatch) owner() (present bool, id int64, ok bool) {
	if p.ManagedBy == nil {
		return false, 0, true
	}
	if string(p.ManagedBy) == "null" {
		return true, 0, true
	}
	if err := json.Unmarshal(p.ManagedBy, &id); err != nil || id <= 0 {
		return true, 0, false
	}
	return true, id, true
}

// handleUpdateUser patches an account in place: role, password, disabled state
// and the household member fields (any subset). It replaces the old
// delete-and-recreate dance and the separate disable endpoint. auth.UpdateUser
// applies the whole body in one transaction, checking all of it first, so a
// refused field changes nothing. The password counts before the role, so
// promoting a password-less account to admin in one request passes the
// admin-password guard, and the member fields before the role, so "Make
// independent" then admin works in one request. managed_by: <id> needs
// member_kind and member_access beside it, and they come only with it (400
// member_fields_required); a password for a kid, or beside assigning a kid, is
// 400 kid_no_password (clearing one is fine).
func (a *API) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var req userPatch
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	assign, owner, ok := req.owner()
	if !ok {
		writeError(w, http.StatusBadRequest, "managed_by must be a user id or null")
		return
	}
	toOwner := assign && owner > 0
	if toOwner && (req.MemberKind == nil || req.MemberAccess == nil) ||
		!toOwner && (req.MemberKind != nil || req.MemberAccess != nil) {
		writeErrorCode(w, http.StatusBadRequest, codeMemberFields,
			"managed_by needs member_kind and member_access, and they come only with it")
		return
	}
	change := auth.UserChanges{
		Password: req.Password, Role: req.Role, Disabled: req.Disabled, CanAddMembers: req.CanAddMembers,
	}
	if assign {
		change.Household = &auth.HouseholdChange{Owner: owner}
		if owner > 0 {
			change.Household.Kind, change.Household.Access = *req.MemberKind, *req.MemberAccess
		}
	}
	if err := a.auth.UpdateUser(r.Context(), id, change); err != nil {
		a.writeUserError(w, err, "could not update user")
		return
	}
	u, err := a.auth.GetUser(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load user")
		return
	}
	// The password itself never: only that it changed (or was cleared).
	details := map[string]any{}
	if req.Role != nil {
		details["role"] = u.Role
	}
	if req.Disabled != nil {
		details["disabled"] = u.Disabled
	}
	if req.Password != nil {
		details["password"] = map[bool]string{true: "set", false: "cleared"}[*req.Password != ""]
	}
	if req.CanAddMembers != nil {
		details["can_add_members"] = u.CanAddMembers
	}
	if assign {
		details["managed_by"], details["member_kind"], details["member_access"] = nil, nil, nil
		if u.ManagedBy != nil {
			details["managed_by"], details["member_kind"], details["member_access"] = u.ManagedBy.Username, *u.MemberKind, *u.MemberAccess
		}
	}
	a.audit(r, "user.update", u.Username, details)
	writeJSON(w, http.StatusOK, toAdminUser(u))
}

// handleDeleteUser permanently removes an account and all of its durable state
// (sessions, auth codes, progress/bookmarks/notes/history, share grants) via the
// schema's cascade, and its household members with it except the ids in
// ?keep_members=<id>,<id> (or the parameter repeated; kept as ordinary
// accounts; an id that isn't one of theirs is 400 invalid_keep_members). Two guards: an admin cannot delete their
// own account (disable it instead - prevents self-lockout and fat-finger loss),
// and auth.DeleteUser refuses the last enabled admin (ErrLastAdmin → 409).
func (a *API) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	// Every keep_members value counts (?keep_members=3,7 and the repeated
	// ?keep_members=3&keep_members=7 alike): reading only the first would delete
	// the members a client asked to keep.
	keep, ok := parseIDList(strings.Join(r.URL.Query()["keep_members"], ","))
	if !ok {
		writeErrorCode(w, http.StatusBadRequest, codeInvalidKeepMembers, "keep_members must be user ids separated by commas")
		return
	}
	if caller := userFrom(r.Context()); caller != nil && caller.ID == id {
		writeErrorCode(w, http.StatusBadRequest, codeCannotDeleteSelf, "you cannot delete your own account - disable it instead")
		return
	}
	target, err := a.auth.GetUser(r.Context(), id)
	if err != nil {
		a.writeUserError(w, err, "could not delete user")
		return
	}
	done, err := a.auth.DeleteUser(r.Context(), id, keep)
	if err != nil {
		a.writeUserError(w, err, "could not delete user")
		return
	}
	// The household facts only for someone who had members (no "0 deleted,
	// none kept" on every other deletion).
	var details map[string]any
	if done.MembersDeleted > 0 || len(done.MembersKept) > 0 {
		details = map[string]any{"members_deleted": done.MembersDeleted, "members_kept": done.MembersKept}
	}
	a.audit(r, "user.delete", target.Username, details)
	w.WriteHeader(http.StatusNoContent)
}

// parseIDList reads "3,7" as ids ("" is none); ok is false for anything else.
func parseIDList(s string) ([]int64, bool) {
	if s == "" {
		return nil, true
	}
	var out []int64
	for part := range strings.SplitSeq(s, ",") {
		id, ok := parseOptionalID(strings.TrimSpace(part))
		if !ok || id == 0 {
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
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
	// Household members (the admin's member fields, DeleteUser's keep list).
	case errors.Is(err, auth.ErrMemberNotAdmin):
		writeErrorCode(w, http.StatusConflict, codeMemberNotAdmin, err.Error())
	case errors.Is(err, auth.ErrOwnerIsMember):
		writeErrorCode(w, http.StatusConflict, codeOwnerIsMember, err.Error())
	case errors.Is(err, auth.ErrOwnerIsDemo):
		writeErrorCode(w, http.StatusConflict, codeOwnerIsDemo, err.Error())
	case errors.Is(err, auth.ErrSelfManaged):
		writeErrorCode(w, http.StatusConflict, codeSelfManaged, err.Error())
	case errors.Is(err, auth.ErrHasMembers):
		writeErrorCode(w, http.StatusConflict, codeHasMembers, err.Error())
	case errors.Is(err, auth.ErrMemberIsDemo):
		writeErrorCode(w, http.StatusConflict, codeMemberIsDemo, err.Error())
	case errors.Is(err, auth.ErrKidNoPassword):
		writeErrorCode(w, http.StatusBadRequest, codeKidNoPassword, err.Error())
	case errors.Is(err, auth.ErrInvalidKind), errors.Is(err, auth.ErrInvalidAccess):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, auth.ErrInvalidKeepMembers):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidKeepMembers, err.Error())
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
	owner := a.codeOwner(r, id)
	if err := a.auth.RevokeAuthCode(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not revoke auth code")
		return
	}
	a.audit(r, "invite.revoke", owner, nil)
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
	switch {
	case errors.Is(err, auth.ErrNotFound):
		writeError(w, http.StatusNotFound, "user not found")
		return
	case writeKidInviteRefusal(w, err):
		return
	case err != nil:
		a.log.Warn("create invite failed", "user", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not create auth code")
		return
	}
	a.audit(r, "invite.create", a.userName(r, id), inviteDetails(minted))
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
	if writeKidInviteRefusal(w, err) {
		return
	}
	if err != nil {
		a.log.Warn("rotate invite failed", "code", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not rotate auth code")
		return
	}
	a.audit(r, "invite.rotate", a.codeOwner(r, id), inviteDetails(minted))
	writeJSON(w, http.StatusOK, a.mintedInvite(r, minted))
}

// writeKidInviteRefusal answers 409 kid_account for an invite the admin mints or
// rotates for a kid's account (auth.ErrKidAccount): a kid holds no invite, only
// their owner sets up their devices. 409, not notKid's 403, since the admin is
// allowed the route and it is the account's state that refuses it. It reports
// whether it answered.
func writeKidInviteRefusal(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, auth.ErrKidAccount) {
		return false
	}
	writeErrorCode(w, http.StatusConflict, codeKidAccount, auth.ErrKidAccount.Error())
	return true
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
	a.audit(r, "user.recovery_clear", a.userName(r, id), nil)
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
	MetadataSource string   `json:"metadata_source"`
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
			MetadataSource: l.MetadataSource,
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
	MetadataSource *string   `json:"metadata_source"`
}

// changes are the fields an edit sent, for the audit log.
func (req libraryRequest) changes() map[string]any {
	d := map[string]any{}
	for k, v := range map[string]string{"name": req.Name, "root": req.Root, "default_view": req.DefaultView} {
		if v != "" {
			d[k] = v
		}
	}
	if req.ScanSchedule != nil {
		d["scan_schedule"] = *req.ScanSchedule
	}
	if req.IgnorePatterns != nil {
		d["ignore_patterns"] = len(*req.IgnorePatterns)
	}
	if req.MetadataSource != nil {
		d["metadata_source"] = *req.MetadataSource
	}
	return d
}

// patch is the request as a validated edit (library.ValidatePatch's errors).
func (req libraryRequest) patch() (catalog.LibraryPatch, error) {
	p := catalog.LibraryPatch{Name: req.Name, Root: req.Root, DefaultView: req.DefaultView,
		ScanSchedule: req.ScanSchedule, IgnorePatterns: req.IgnorePatterns, MetadataSource: req.MetadataSource}
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
	a.audit(r, "library.create", created.Name, map[string]any{"root": created.Root})
	// Kick off an initial scan in the background; browsing works immediately.
	a.startScan(r, *created, library.TriggerManual)
	writeJSON(w, http.StatusCreated, created)
}

// handleUpdateLibrary edits a library: name, root, default view, scan schedule,
// ignore rules and metadata source. A new root or new ignore rules make the index
// stale, so either queues a rescan, returned as `job`; browsing still works
// immediately. A new metadata source re-resolves the books before it answers.
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
	a.audit(r, "library.update", updated.Name, req.changes())
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
	a.audit(r, "library.reorder", "", nil)
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
	if _, err := library.ContentJoin(lib.Root, path); err != nil {
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
	a.audit(r, "library.folder_override", lib.Name+": "+path, map[string]any{"mode": req.Mode})
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
	if _, err := library.ContentJoin(lib.Root, path); err != nil {
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
	a.audit(r, "book.enrichment", lib.Name+": "+path, map[string]any{"asin": req.ASIN, "isbn": req.ISBN})
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
	a.audit(r, "library.folder_override", lib.Name+": "+path, map[string]any{"mode": "auto"})
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
	name := a.libraryName(r, id) // read before it's gone
	if err := a.cat.DeleteLibrary(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete library")
		return
	}
	// Its scans have nothing left to index into; don't let them hold the queue.
	a.scanner.CancelLibrary(id)
	a.audit(r, "library.delete", name, nil)
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

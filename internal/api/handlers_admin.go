package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/metadata"
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
	Email         string      `json:"email"`
	// SignInProvider names the single sign-on the person signs in with now
	// ("" for none): an identity from the provider the server runs, on an
	// account it signs in (never a household member's or a demo account's,
	// auth.ConsumeOIDC). Like a password, an admin's way into the console
	// (server#408).
	SignInProvider string `json:"sign_in_provider"`
}

// adminOwner is a member's owner as the console names them (display_name, else
// the username).
type adminOwner struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

// toAdminUser is u as the console shows it; sso says whether u holds an
// identity from the single sign-on the server runs (auth.SSOAccounts).
func (a *API) toAdminUser(u *auth.User, sso bool) adminUser {
	v := adminUser{User: u, MemberAccess: u.MemberAccess, CanAddMembers: u.CanAddMembers,
		MembersCount: u.MembersCount, DisplayName: u.DisplayName, Email: u.Email}
	if o := u.ManagedBy; o != nil {
		v.ManagedBy = &adminOwner{ID: o.ID, Username: o.Username, DisplayName: o.DisplayName}
	}
	if sso && a.ssoShown(u) {
		v.SignInProvider = a.oidc.name
	}
	return v
}

// ssoShown reports whether u's console object can name a single sign-on: one
// runs, and u is an account it signs in (auth.User.SSOAllowed).
func (a *API) ssoShown(u *auth.User) bool {
	return a.oidc != nil && u.SSOAllowed()
}

// adminUserOf is toAdminUser reading whether u signs in with the single
// sign-on (auth.HasSSO). A failed read is logged and says none: the console
// then asks for a password rather than trust a way in.
func (a *API) adminUserOf(ctx context.Context, u *auth.User) adminUser {
	if !a.ssoShown(u) {
		return a.toAdminUser(u, false)
	}
	sso, err := a.auth.HasSSO(ctx, u.ID)
	if err != nil {
		a.log.Warn("reading an account's single sign-on failed", "user", u.ID, "err", err)
	}
	return a.toAdminUser(u, sso)
}

// adminIdentity is one of a person's linked sign-ons on their console page:
// the identity with the configured provider's name when it is the single
// sign-on the server runs ("" otherwise: the console names it by the
// issuer's host), so the page needs no provider status read (server#408).
type adminIdentity struct {
	auth.Identity
	Provider string `json:"provider"`
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
	sso, err := a.auth.SSOAccounts(r.Context())
	if err != nil {
		// Without it no one reads as signing in with a provider: the list still
		// loads.
		a.log.Warn("reading the accounts' single sign-on failed", "err", err)
	}
	out := make([]adminUser, len(users))
	for i := range users {
		out[i] = a.toAdminUser(&users[i], sso[users[i].ID])
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (a *API) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"` // optional for non-admins (auth-code pairing only)
		Role     string `json:"role"`
		Email    string `json:"email"` // optional
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// CreateUser enforces the username rule (400 username_required) and the
	// password rules (required for admins, optional otherwise); writeUserError
	// maps its typed errors to the right status and never echoes a raw DB error
	// (a duplicate username must not leak the SQLite constraint string).
	u, err := a.auth.CreateUserWithEmail(r.Context(), req.Username, req.Password, req.Role, req.Email)
	if err != nil {
		a.writeUserError(w, err, "could not create user")
		return
	}
	a.audit(r, "user.create", u.Username, map[string]any{"role": u.Role, "password": req.Password != "", "email": u.Email != ""})
	// Re-read, so the answer is the admin's user object like every other admin
	// user answer (adminUser): the stored defaults (can_add_members) included.
	full, err := a.auth.GetUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load user")
		return
	}
	writeJSON(w, http.StatusCreated, a.adminUserOf(r.Context(), full))
}

// handleGetUserDetail returns one account plus everything the admin console
// needs to manage it: the libraries it can reach (a household member's derived
// access included), the shares granted to it, its issued auth codes (metadata
// only - the plaintext codes are never stored), its own household members,
// whether it has a calendar feed link (calendar_link: {active, created_at},
// never the secret) and, for a member on "collections", the owner's
// collections shared with it. A
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
	who := callerForUser(u)
	who.Podcasts = seesPodcasts(r.Context())
	libs, err := a.cat.CallerLibraries(r.Context(), who)
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
	identities, err := a.auth.ListIdentities(r.Context(), id)
	if err != nil {
		a.log.Warn("list identities failed", "user", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not load single sign-on identities")
		return
	}
	shared, err := a.memberCollectionsOf(r.Context(), u)
	if err != nil {
		a.log.Warn("member collections failed", "user", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not load household members")
		return
	}
	feed, err := a.auth.FeedTokenStatus(r.Context(), id)
	if err != nil {
		a.log.Warn("calendar link status failed", "user", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not load the calendar link")
		return
	}
	active, sso := a.oidcIssuer(), false
	ids := make([]adminIdentity, len(identities))
	for i, ident := range identities {
		ids[i] = adminIdentity{Identity: ident}
		if active != "" && ident.Issuer == active {
			ids[i].Provider, sso = a.oidc.name, true
		}
	}
	out := map[string]any{
		"user":                 a.toAdminUser(u, sso),
		"accessible_libraries": libs,
		"shares":               shares,
		"auth_codes":           codes,
		"members":              own,
		"member_collections":   shared,
		"identities":           ids,
		"calendar_link":        feed,
	}
	if u.IsMember() {
		ownerLibs, ownerAdmin := []catalog.Library{}, false
		if rule := u.MemberRule; rule != nil {
			ownerAdmin = rule.OwnerAdmin
			if ownerLibs, err = a.cat.CallerLibraries(r.Context(), catalog.Caller{ID: rule.Owner, Admin: ownerAdmin, Podcasts: who.Podcasts}); err != nil {
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
	Username      *string         `json:"username"`
	Role          *string         `json:"role"`
	Password      *string         `json:"password"`
	Email         *string         `json:"email"`
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

// handleUpdateUser patches an account in place: username, role, password,
// disabled state and the household member fields (any subset). A username is
// the rule a new account's is: not "" (400 username_required), not another
// account's (409 username_taken). It replaces the old
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
	// The name before a rename, for the audit.
	var was string
	if req.Username != nil {
		before := a.lookupUser(w, r, id)
		if before == nil {
			return
		}
		was = before.Username
	}
	// A reset of a password the person had ends their sessions, all but the
	// admin's own when it is their own account (auth.UpdateUser).
	change := auth.UserChanges{
		Username: req.Username, Password: req.Password, Email: req.Email, Role: req.Role, Disabled: req.Disabled,
		CanAddMembers: req.CanAddMembers,
		KeepToken:     credentialFrom(r.Context()).ID,
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
	if req.Username != nil && was != u.Username {
		details["username_was"] = was
	}
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
	if req.Email != nil {
		details["email"] = map[bool]string{true: "set", false: "cleared"}[u.Email != ""]
	}
	if assign {
		details["managed_by"], details["member_kind"], details["member_access"] = nil, nil, nil
		if u.ManagedBy != nil {
			details["managed_by"], details["member_kind"], details["member_access"] = u.ManagedBy.Username, *u.MemberKind, *u.MemberAccess
		}
	}
	a.audit(r, "user.update", u.Username, details)
	writeJSON(w, http.StatusOK, a.adminUserOf(r.Context(), u))
}

// handleDeleteUser permanently removes an account and all of its durable state
// (sessions, auth codes, progress/bookmarks/notes/history, share grants) via the
// schema's cascade, and its household members with it except the ids in
// ?keep_members=<id>,<id> (or the parameter repeated; kept as ordinary
// accounts; an id that isn't one of theirs is 400 invalid_keep_members). With
// ?delete_members=<id>,<id> (present, even empty: the console always sends it)
// only those members go and every other is kept, so a member added after the
// console read the list is never deleted unseen (server#222; an id that isn't
// one of theirs, or one keep_members names, is 400 invalid_delete_members).
// With detail=1 the reply includes counts of the members deleted and kept.
// Their Audible accounts go too, and those accounts' open jobs are cancelled.
// Two guards: an admin cannot delete their
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
	query := r.URL.Query()
	keep, ok := parseIDList(strings.Join(query["keep_members"], ","))
	if !ok {
		writeErrorCode(w, http.StatusBadRequest, codeInvalidKeepMembers, "keep_members must be user ids separated by commas")
		return
	}
	delParam, listed := query["delete_members"]
	del, ok := parseIDList(strings.Join(delParam, ","))
	if !ok {
		writeErrorCode(w, http.StatusBadRequest, codeInvalidDelMembers, "delete_members must be user ids separated by commas")
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
	var done auth.DeletedUser
	if listed {
		done, err = a.auth.DeleteUserListed(r.Context(), id, keep, del)
	} else {
		done, err = a.auth.DeleteUser(r.Context(), id, keep)
	}
	if err != nil {
		a.writeUserError(w, err, "could not delete user")
		return
	}
	a.cancelRemovedAudibleJobs(r, done.AudibleAccounts)
	// The household facts only for someone who had members (no "0 deleted,
	// none kept" on every other deletion).
	var details map[string]any
	if done.MembersDeleted > 0 || len(done.MembersKept) > 0 {
		details = map[string]any{"members_deleted": done.MembersDeleted, "members_kept": done.MembersKept}
	}
	a.audit(r, "user.delete", target.Username, details)
	if query.Get("detail") == "1" {
		writeJSON(w, http.StatusOK, map[string]int{"members_deleted": done.MembersDeleted, "members_kept": len(done.MembersKept)})
		return
	}
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
	case errors.Is(err, auth.ErrUsernameRequired):
		writeErrorCode(w, http.StatusBadRequest, codeUsernameRequired, err.Error())
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
	case errors.Is(err, auth.ErrInvalidEmail):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidEmail, err.Error())
	case errors.Is(err, auth.ErrIdentityLinked):
		writeErrorCode(w, http.StatusConflict, codeIdentityLinked, err.Error())
	case errors.Is(err, auth.ErrUserHasIdentity):
		writeErrorCode(w, http.StatusConflict, codeUserHasIdentity, err.Error())
	case errors.Is(err, auth.ErrNotLinkable):
		writeErrorCode(w, http.StatusConflict, codeNotLinkable, err.Error())
	case errors.Is(err, auth.ErrInvalidKeepMembers):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidKeepMembers, err.Error())
	case errors.Is(err, auth.ErrInvalidDeleteMembers):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidDelMembers, err.Error())
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
	if a.externalHTTPS(r) {
		out["external_https"] = true
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

// handleSignOutEverywhere signs a person out of every phone, tablet and browser
// (the console's "Sign out everywhere"): it ends all their sessions, and the cast
// tokens minted from them, keeping the admin's own when it is their own account,
// with their open pairing codes and recovery code, and their household members'
// sign-ins, API keys and calendar links made on those devices; their own API
// keys, calendar feed token and Bridge tokens stay (auth.endSessions). Answers
// {"signed_out": n}, the sessions it ended.
func (a *API) handleSignOutEverywhere(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	n, err := a.auth.SignOutEverywhere(r.Context(), id, credentialFrom(r.Context()).ID)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		a.log.Warn("sign out everywhere failed", "err", err, "user", id)
		writeError(w, http.StatusInternalServerError, "could not sign the person out")
		return
	}
	a.audit(r, "user.sign_out_everywhere", a.userName(r, id), map[string]any{"signed_out": n})
	writeJSON(w, http.StatusOK, map[string]int64{"signed_out": n})
}

// handleEndKeys ends a person's API keys and their calendar feed link (the
// console's "End API keys and calendar link" on People > a person > Devices,
// server#285): every live key revoked and the link deleted in one write, so
// neither works on the next request. Their sessions stay (Sign out everywhere
// is the other lever). Answers {"api_keys": n, "calendar_link": bool}, what it
// ended; audit user.end_keys with the same.
func (a *API) handleEndKeys(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	ended, err := a.auth.EndKeys(r.Context(), id)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		a.log.Warn("end keys failed", "err", err, "user", id)
		writeError(w, http.StatusInternalServerError, "could not end the keys")
		return
	}
	a.audit(r, "user.end_keys", a.userName(r, id),
		map[string]any{"api_keys": ended.APIKeys, "calendar_link": ended.CalendarLink})
	writeJSON(w, http.StatusOK, ended)
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
	FromConfig     bool     `json:"from_config"`
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
			FromConfig:     a.configLibrary(l.Name),
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

// maxDirListing caps one folder-picker listing; dirListSlots and dirListTimeout
// are the picker's library.DirLister (the setup preview's root check runs
// behind it too): reads at once per mount, and how long one is waited for (a
// hung mount answers 404 folder_unreadable then, the others listing as ever).
const (
	maxDirListing  = 1000
	dirListSlots   = 4
	dirListTimeout = 5 * time.Second
)

// handleListDirs is the add-library folder picker: the subfolders of an absolute
// server path (the filesystem root when ?path= is empty). Folder names only; see
// library.ListDirs for the bounds.
func (a *API) handleListDirs(w http.ResponseWriter, r *http.Request) {
	listing, err := a.dirs.List(r.Context(), r.URL.Query().Get("path"), maxDirListing)
	if err != nil {
		writeFolderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

// writeFolderError answers a typed server folder the folder picker or the setup
// preview refused: 400 path_not_absolute for a relative path, else 404
// folder_unreadable (missing, not a folder, not readable by the server, or not
// answering in time: the same answer, and the OS error, which can name the path,
// stays out of the body).
func writeFolderError(w http.ResponseWriter, err error) {
	if errors.Is(err, library.ErrNotAbsolute) {
		writeErrorCode(w, http.StatusBadRequest, codePathNotAbsolute, "path must be absolute")
		return
	}
	writeErrorCode(w, http.StatusNotFound, codeFolderUnreadable, "folder not found or not readable")
}

// libraryRequest is the body of a library create or edit. The scan settings are
// pointers so an edit that leaves them out keeps them.
type libraryRequest struct {
	Kind           *string   `json:"kind"`
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
	p := catalog.LibraryPatch{Kind: req.Kind, Name: req.Name, Root: req.Root, DefaultView: req.DefaultView,
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
	if req.Kind != nil && !catalog.KnownKind(*req.Kind) {
		writeErrorCode(w, 400, "invalid_kind", "Kind must be books or podcasts.")
		return
	}
	if a.takesConfigName(req.Name, req.Root) {
		writeConfigNameTaken(w)
		return
	}
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
// A library config.yaml declares keeps the name and folder declared there: every
// start upserts it by name (launcher.syncLibraries) and the server never writes
// a library to config.yaml, so a rename would come back as a second library and
// a new folder would be put back. Changing either is 409 config_library, and
// the same values resent write nothing (a respelled folder would be stored as
// typed and rescanned); its other settings stay editable. Another library
// renamed to a declared name with another folder is 409 name_taken, as the next
// start would give it the declared folder.
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
	// Only a server whose config declares libraries needs the current row.
	if (req.Name != "" || req.Root != "") && len(a.config().Libraries) > 0 {
		cur, err := a.cat.GetLibrary(r.Context(), id)
		if errors.Is(err, catalog.ErrNotFound) {
			writeError(w, http.StatusNotFound, "library not found")
			return
		}
		if err != nil {
			a.writeCatalogError(w, err, "update library failed", "could not update library", "library", id)
			return
		}
		switch {
		case a.configLibrary(cur.Name):
			if (req.Name != "" && req.Name != cur.Name) || (req.Root != "" && !sameRoot(req.Root, cur.Root)) {
				writeErrorCode(w, http.StatusConflict, codeConfigLibrary,
					"This library is declared in the server's config, so its name and folder are set there. Change them in config.yaml (or the app that runs the server) and restart.")
				return
			}
			p.Name, p.Root = "", ""
		case req.Name != "" && req.Name != cur.Name && a.takesConfigName(req.Name, cmp.Or(req.Root, cur.Root)):
			writeConfigNameTaken(w)
			return
		}
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
	resolving := false
	if req.MetadataSource != nil {
		resolving = !awaitResolve(r.Context(), a.resolveSources()) || a.resolvePending()
	}
	// Resolving: the books are still taking the new metadata source when this
	// answers (a large library finishes in the background, a run that failed
	// is run again later), so the console says so rather than "updated".
	resp := struct {
		*catalog.Library
		Job       *library.Job `json:"job,omitempty"`
		Resolving bool         `json:"resolving,omitempty"`
	}{Library: updated, Resolving: resolving}
	if stale {
		// The folders kept open under its old root are let go of now, not
		// when they have gone unused a moment: that root can be unmounted
		// once the reads still under way there end.
		metadata.CloseFolders()
		job := a.startScan(r, *updated, library.TriggerChange)
		resp.Job = &job
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveWait is how long a metadata source change waits for its books'
// re-resolve, at most, before it answers.
const resolveWait = 20 * time.Second

// awaitResolve waits for done (the re-resolve a metadata source change began),
// up to resolveWait and never closer than two seconds to the request's deadline:
// a library small enough answers with its books re-resolved, a large one at once,
// the rest going on in the background (server#213). It reports whether the
// re-resolve ended.
func awaitResolve(ctx context.Context, done <-chan struct{}) bool {
	wait := resolveWait
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline)-2*time.Second)
	}
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// configLibrary reports whether the config declares a library by this name
// (launcher.syncLibraries upserts each by name at start).
func (a *API) configLibrary(name string) bool {
	return slices.ContainsFunc(a.config().Libraries, func(l config.Library) bool { return l.Name == name })
}

// takesConfigName reports whether a library added or renamed with this name and
// folder would be given another folder at the next start: the config declares a
// library by the name at another folder. (The name reads as free only while the
// declared library is deleted; the next start puts it back, on that name.)
func (a *API) takesConfigName(name, root string) bool {
	for _, l := range a.config().Libraries {
		if l.Name == name {
			declared, err := filepath.Abs(l.Root)
			return err != nil || !sameRoot(root, declared)
		}
	}
	return false
}

// sameRoot reports whether a folder as sent names the one a library has: the
// same absolute path, whatever the spelling ("/a/b/" is "/a/b").
func sameRoot(sent, have string) bool {
	root, err := filepath.Abs(sent)
	return err == nil && root == filepath.Clean(have)
}

func writeConfigNameTaken(w http.ResponseWriter) {
	writeErrorCode(w, http.StatusConflict, codeNameTaken,
		"The server's config declares a library by this name, with another folder. Pick another name.")
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
	path := r.URL.Query().Get("path")
	if pathTooLong(w, path) {
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
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if _, err := library.ContentJoin(lib.Root, path); metadata.GaveUp(err) {
		writeShareNotAnswering(w) // the join's look a share failed: no answer about the path
		return
	} else if err != nil {
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

// handleDeleteFolderOverride clears a folder's detection override (reverting it
// to auto-detection) and rescans the library.
func (a *API) handleDeleteFolderOverride(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	path := r.URL.Query().Get("path")
	if pathTooLong(w, path) {
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
// audio files on disk are not touched. A library the config declares is 409
// config_library: the next start would add it back, empty, with everyone's
// progress, bookmarks and notes in it gone (server#176), so it is removed from
// config.yaml (or the app that runs the server) instead.
func (a *API) handleDeleteLibrary(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	var name string // read before it's gone
	switch lib, err := a.cat.GetLibrary(r.Context(), id); {
	case err == nil && a.configLibrary(lib.Name):
		writeErrorCode(w, http.StatusConflict, codeConfigLibrary,
			"This library is declared in the server's config, so deleting it here would only empty it until the next start. Remove it from config.yaml (or the app that runs the server) and restart.")
		return
	case err == nil:
		name = lib.Name
	case errors.Is(err, catalog.ErrNotFound):
		// Nothing to delete, so nothing to record (server#198).
		writeErrorCode(w, http.StatusNotFound, codeNotFound, "library not found")
		return
	default:
		a.writeCatalogError(w, err, "delete library failed", "could not delete library", "library", id)
		return
	}
	if err := a.cat.DeleteLibrary(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete library")
		return
	}
	// Its scans have nothing left to index into; don't let them hold the queue.
	a.scanner.CancelLibrary(id)
	// Nor is a folder of its kept open for a next read: its share can be
	// unmounted once the reads the stopped scan still has under way end.
	metadata.CloseFolders()
	// Its Bridge jobs went with it: so do the private feed URLs held for them.
	if a.bridge != nil {
		if err := a.bridge.PruneSecrets(r.Context()); err != nil {
			a.log.Warn("drop a deleted library's private feed URLs failed", "library", id, "err", err)
		}
	}
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

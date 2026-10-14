package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Household members (player redesign Phase 8b-1, capability household_members):
// the caller's own members under /me/household/members. The accounts and their
// rules live in auth (members.go), what a member may listen to in catalog (the
// member rule) and the collections shared with them in catalog too; this file is
// transport. Every route refuses an API key (denyAPIKey); every route but the
// list (which answers a demo or member caller 200 with allowed false and its
// reason) refuses a demo account and a member caller too. Adding is also refused
// while the server's or the person's switch is off or at the cap, while the
// other routes keep working on the caller's existing members (so an owner can
// always rename, sign out and delete them). Someone else's member is 404 on
// every route.

// Member route error codes.
const (
	codeDemoAccount        = "demo_account"
	codeMemberCannotManage = "member_cannot_manage"
	codeKidAccount         = "kid_account"
	codeFeatureOff         = "feature_off"
	codeNotAllowed         = "not_allowed"
	codeMemberLimit        = "member_limit"
	codeMemberNotAdmin     = "member_not_admin"
	codeInvalidName        = "invalid_name"
	codeInvalidCollections = "invalid_collections"
	codeInvalidKeepMembers = "invalid_keep_members"
	codeMemberFields       = "member_fields_required"
	codeOwnerIsMember      = "owner_is_member"
	codeOwnerIsDemo        = "owner_is_demo"
	codeSelfManaged        = "self_managed"
	codeHasMembers         = "has_members"
	codeMemberIsDemo       = "member_is_demo"
	codeKidNoPassword      = "kid_no_password"
	codeMemberDisabled     = "member_disabled"
)

// addReasons names auth.MayAddMembers' refusals as the list's reasons
// (householdMembers.Reason).
var addReasons = map[error]string{
	auth.ErrDemoAccount:        "demo",
	auth.ErrMemberCannotManage: "member",
	auth.ErrFeatureOff:         "feature_off",
	auth.ErrNotAllowed:         "not_allowed",
}

// householdMember is a member as their owner's list shows it: the account
// (auth.Member), the owner's collections shared with them (present, maybe empty,
// for access "collections"; absent for "all") and when they last listened (null
// when never).
type householdMember struct {
	auth.Member
	Collections    *[]catalog.MemberCollection `json:"collections,omitempty"`
	LastListenedAt *string                     `json:"last_listened_at"`
}

// householdMembers is GET /me/household/members' answer. Reason says why
// Allowed is false (being at the cap is not a reason: the client compares
// len(members) with max).
type householdMembers struct {
	Allowed bool              `json:"allowed"`
	Reason  string            `json:"reason,omitempty"`
	Max     int               `json:"max"`
	Members []householdMember `json:"members"`
}

// notKid wraps the routes a kid's account can't use (a password, API keys,
// pairing, a recovery code, house sharing, sharing collections; api.go): a kid
// is 403 kid_account before the handler runs. It reads nothing: the request's
// user carries member_kind from its token lookup.
func notKid(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := userFrom(r.Context()); u != nil && u.IsKid() {
			writeErrorCode(w, http.StatusForbidden, codeKidAccount, "not available for a kid's account")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeKidRefusal answers 403 kid_account, as notKid does, for a write that
// auth or catalog refused for a kid inside its own transaction (a request that
// passed notKid just before the owner's switch to kid committed), and reports
// whether it did.
func writeKidRefusal(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, auth.ErrKidAccount) && !errors.Is(err, catalog.ErrKidAccount) {
		return false
	}
	writeErrorCode(w, http.StatusForbidden, codeKidAccount, "not available for a kid's account")
	return true
}

// refuseMember answers 403 member_cannot_manage and returns true when u is a
// household member.
func refuseMember(w http.ResponseWriter, u *auth.User) bool {
	if u.IsMember() {
		writeErrorCode(w, http.StatusForbidden, codeMemberCannotManage, auth.ErrMemberCannotManage.Error())
		return true
	}
	return false
}

// memberOwner loads the caller who may manage their members: an API key, a demo
// account and a member are refused (403); with limited, the account limiter
// then applies (gateSelfService's: the routes that make an account or a
// credential), after the refusals, so a member's or demo's refused calls never
// spend the address's shared budget. nil when it has answered.
func (a *API) memberOwner(w http.ResponseWriter, r *http.Request, limited bool) *auth.User {
	if denyAPIKey(w, r) {
		return nil
	}
	u := a.nonDemoAccount(w, r)
	if u == nil || refuseMember(w, u) {
		return nil
	}
	if limited && !allowAttempt(w, a.accountLimiter.Acquire(clientIP(r))) {
		return nil
	}
	return u
}

// memberID reads {id}, answering 400 when it is not a number.
func memberID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	return pathID(w, r, "id", "invalid member id")
}

// writeMemberError maps the member routes' errors; anything else goes to
// writeCatalogError (a share cap, else a logged 500). auth.ErrNotFound is not
// mapped: what wasn't found differs per call (the caller's own account is a
// logged 500, a member's device 404 "device not found" at its one call site).
func (a *API) writeMemberError(w http.ResponseWriter, err error, op, generic string, logKV ...any) {
	switch {
	case errors.Is(err, auth.ErrNotMember):
		writeError(w, http.StatusNotFound, "member not found")
	case errors.Is(err, auth.ErrDemoAccount):
		writeErrorCode(w, http.StatusForbidden, codeDemoAccount, err.Error())
	case errors.Is(err, auth.ErrMemberCannotManage):
		writeErrorCode(w, http.StatusForbidden, codeMemberCannotManage, err.Error())
	case errors.Is(err, auth.ErrFeatureOff):
		writeErrorCode(w, http.StatusForbidden, codeFeatureOff, err.Error())
	case errors.Is(err, auth.ErrNotAllowed):
		writeErrorCode(w, http.StatusForbidden, codeNotAllowed, err.Error())
	case errors.Is(err, auth.ErrMemberLimit):
		writeErrorCode(w, http.StatusConflict, codeMemberLimit, err.Error())
	case errors.Is(err, auth.ErrMemberDisabled):
		writeErrorCode(w, http.StatusConflict, codeMemberDisabled, err.Error())
	case errors.Is(err, auth.ErrInvalidName):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidName, err.Error())
	case errors.Is(err, auth.ErrInvalidKind), errors.Is(err, auth.ErrInvalidAccess):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, catalog.ErrInvalidCollections):
		writeErrorCode(w, http.StatusBadRequest, codeInvalidCollections, err.Error())
	default:
		a.writeCatalogError(w, err, op, generic, logKV...)
	}
}

// shareWith is the auth.ShareFunc making ids the owner's collections shared with
// a member (nil when the request named none).
func (a *API) shareWith(ctx context.Context, owner int64, ids *[]int64) auth.ShareFunc {
	if ids == nil {
		return nil
	}
	return func(tx *sql.Tx, member int64) error {
		return a.cat.SetMemberCollections(ctx, tx, owner, member, *ids)
	}
}

// decorateMembers adds each member's shared collections (as the owner's current
// access counts them) and last listening.
func (a *API) decorateMembers(ctx context.Context, owner *auth.User, members []auth.Member) ([]householdMember, error) {
	out := make([]householdMember, len(members))
	if len(members) == 0 {
		return out, nil
	}
	ids := make([]int64, len(members))
	var coll []int64
	for i, m := range members {
		ids[i] = m.ID
		if m.Access == auth.MemberCollections {
			coll = append(coll, m.ID)
		}
	}
	shared, err := a.cat.MemberCollections(ctx, owner.ID, owner.Role == auth.RoleAdmin, coll)
	if err != nil {
		return nil, err
	}
	last, err := a.cat.LastListened(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i, m := range members {
		out[i].Member = m
		if m.Access == auth.MemberCollections {
			cols := shared[m.ID]
			if cols == nil {
				cols = []catalog.MemberCollection{}
			}
			out[i].Collections = &cols
		}
		if at, ok := last[m.ID]; ok {
			out[i].LastListenedAt = &at
		}
	}
	return out, nil
}

// decoratedMember is one member as the list shows them, for the create and PATCH
// answers.
func (a *API) decoratedMember(ctx context.Context, owner *auth.User, m auth.Member) (householdMember, error) {
	out, err := a.decorateMembers(ctx, owner, []auth.Member{m})
	if err != nil {
		return householdMember{}, err
	}
	return out[0], nil
}

// handleListMembers answers GET /me/household/members: the caller's members and
// whether they may add one. A demo or member caller gets 200 with allowed false,
// its reason and no members; an API key 403.
func (a *API) handleListMembers(w http.ResponseWriter, r *http.Request) {
	if denyAPIKey(w, r) {
		return
	}
	caller := userFrom(r.Context())
	u, err := a.auth.GetUser(r.Context(), caller.ID)
	if err != nil {
		a.writeMemberError(w, err, "load account failed", "could not load your household", "user", caller.ID)
		return
	}
	policy := a.config().Household
	out := householdMembers{Max: policy.MaxMembers, Members: []householdMember{}}
	refused := auth.MayAddMembers(u, auth.MemberPolicy{Enabled: policy.MembersEnabled, Max: policy.MaxMembers})
	out.Allowed, out.Reason = refused == nil, addReasons[refused]
	// A demo account or a member has no members to list; anyone else's list
	// shows even while adding is off.
	if !u.IsDemo && !u.IsMember() {
		members, err := a.auth.ListMembers(r.Context(), u.ID)
		if err == nil {
			out.Members, err = a.decorateMembers(r.Context(), u, members)
		}
		if err != nil {
			a.writeMemberError(w, err, "list members failed", "could not load your household", "user", u.ID)
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateMember answers POST /me/household/members {name, kind, access,
// sign_in_here?, collection_ids?} (strict): 201 {member, session?}. With
// sign_in_here the member is signed in on the caller's device (a session named
// as the caller's, recording it as created_on_token_id).
func (a *API) handleCreateMember(w http.ResponseWriter, r *http.Request) {
	owner := a.memberOwner(w, r, true)
	if owner == nil {
		return
	}
	var in struct {
		Name          string   `json:"name"`
		Kind          string   `json:"kind"`
		Access        string   `json:"access"`
		SignInHere    bool     `json:"sign_in_here"`
		CollectionIDs *[]int64 `json:"collection_ids"`
	}
	if err := decodeJSON(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	cred := credentialFrom(r.Context())
	policy := a.config().Household
	m, secret, signedIn, err := a.auth.CreateMember(r.Context(), owner.ID, auth.NewMember{
		Name: in.Name, Kind: in.Kind, Access: in.Access,
		SignInHere: in.SignInHere, DeviceName: cred.DeviceName, OnToken: cred.ID,
		Share: a.shareWith(r.Context(), owner.ID, in.CollectionIDs),
	}, auth.MemberPolicy{Enabled: policy.MembersEnabled, Max: policy.MaxMembers})
	if err != nil {
		a.writeMemberError(w, err, "create member failed", "could not add the member", "user", owner.ID)
		return
	}
	a.audit(r, "member.create", m.Username, map[string]any{
		"member": m.Username, "kind": m.Kind, "access": m.Access, "sign_in_here": in.SignInHere,
	})
	member, err := a.decoratedMember(r.Context(), owner, m)
	if err != nil {
		a.writeMemberError(w, err, "load member failed", "could not load the member", "member", m.ID)
		return
	}
	out := map[string]any{"member": member}
	if signedIn != nil {
		out["session"] = a.sessionEnvelope(r, secret, signedIn)
	}
	writeJSON(w, http.StatusCreated, out)
}

// handleMemberSession answers POST /me/household/members/{id}/session: 201 and
// the session envelope of the member signed in on the caller's device (409
// member_disabled for a member the admin disabled).
func (a *API) handleMemberSession(w http.ResponseWriter, r *http.Request) {
	id, ok := memberID(w, r)
	if !ok {
		return
	}
	owner := a.memberOwner(w, r, true)
	if owner == nil {
		return
	}
	cred := credentialFrom(r.Context())
	secret, u, err := a.auth.MemberSession(r.Context(), owner.ID, id, cred.DeviceName, cred.ID)
	if err != nil {
		a.writeMemberError(w, err, "member session failed", "could not sign the member in", "member", id)
		return
	}
	a.audit(r, "member.session", u.Username, map[string]any{"member": u.Username})
	writeJSON(w, http.StatusCreated, a.sessionEnvelope(r, secret, u))
}

// handleMemberPair answers POST /me/household/members/{id}/pair: the pairing
// payload for the member's own device (single use, 10 minutes, built like
// handlePair's; 409 member_disabled for a member the admin disabled).
func (a *API) handleMemberPair(w http.ResponseWriter, r *http.Request) {
	id, ok := memberID(w, r)
	if !ok {
		return
	}
	owner := a.memberOwner(w, r, true)
	if owner == nil {
		return
	}
	m, token, err := a.auth.MemberPairing(r.Context(), owner.ID, id, pairingTTL)
	if err != nil {
		a.writeMemberError(w, err, "member pairing failed", "could not set up the device", "member", id)
		return
	}
	payload, err := a.buildPairing(r, token)
	if err != nil {
		a.log.Warn("build member pairing failed", "member", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not build pairing payload")
		return
	}
	a.audit(r, "member.pair", m.Username, map[string]any{"member": m.Username})
	writeJSON(w, http.StatusOK, payload)
}

// handleMemberDevices answers GET /me/household/members/{id}/devices: the
// member's live sessions and API keys, as a bare array ([] for none); current is
// always false (none of them makes this request).
func (a *API) handleMemberDevices(w http.ResponseWriter, r *http.Request) {
	id, ok := memberID(w, r)
	if !ok {
		return
	}
	owner := a.memberOwner(w, r, false)
	if owner == nil {
		return
	}
	devices, err := a.auth.MemberDevices(r.Context(), owner.ID, id)
	if err != nil {
		a.writeMemberError(w, err, "list member devices failed", "could not load the devices", "member", id)
		return
	}
	out := make([]myDevice, len(devices))
	for i, d := range devices {
		out[i] = toMyDevice(d, 0)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeMemberDevice answers DELETE /me/household/members/{id}/devices/
// {device_id}: 204, or 404 for a device that isn't the member's.
func (a *API) handleRevokeMemberDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := memberID(w, r)
	if !ok {
		return
	}
	device, ok := pathID(w, r, "device_id", "invalid device id")
	if !ok {
		return
	}
	owner := a.memberOwner(w, r, false)
	if owner == nil {
		return
	}
	m, err := a.auth.RevokeMemberDevice(r.Context(), owner.ID, id, device)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		a.writeMemberError(w, err, "revoke member device failed", "could not sign the device out", "member", id)
		return
	}
	a.audit(r, "member.device_revoke", m.Username, map[string]any{"member": m.Username, "device_id": device})
	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateMember answers PATCH /me/household/members/{id} {name?, kind?,
// access?, collection_ids?} (strict; {} is 400): 200 and the member. Switching a
// grown-up to a kid clears their password, API keys, pairing codes, invites and
// recovery code, and the collections shared between them and anyone but the
// caller; their devices stay signed in.
func (a *API) handleUpdateMember(w http.ResponseWriter, r *http.Request) {
	id, ok := memberID(w, r)
	if !ok {
		return
	}
	owner := a.memberOwner(w, r, false)
	if owner == nil {
		return
	}
	var in struct {
		Name          *string  `json:"name"`
		Kind          *string  `json:"kind"`
		Access        *string  `json:"access"`
		CollectionIDs *[]int64 `json:"collection_ids"`
	}
	if err := decodeJSON(r, &in, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if in.Name == nil && in.Kind == nil && in.Access == nil && in.CollectionIDs == nil {
		writeError(w, http.StatusBadRequest, "nothing to change")
		return
	}
	m, changes, err := a.auth.UpdateMember(r.Context(), owner.ID, id, auth.MemberUpdate{
		Name: in.Name, Kind: in.Kind, Access: in.Access, Share: a.shareWith(r.Context(), owner.ID, in.CollectionIDs),
	})
	if err != nil {
		a.writeMemberError(w, err, "update member failed", "could not save the member", "member", id)
		return
	}
	if in.CollectionIDs != nil {
		changes["collections"] = len(*in.CollectionIDs)
	}
	if len(changes) > 0 {
		changes["member"] = m.Username
		a.audit(r, "member.update", m.Username, changes)
	}
	member, err := a.decoratedMember(r.Context(), owner, m)
	if err != nil {
		a.writeMemberError(w, err, "load member failed", "could not load the member", "member", id)
		return
	}
	writeJSON(w, http.StatusOK, member)
}

// handleDeleteMember answers DELETE /me/household/members/{id}: 204, the
// account and all its state deleted (the schema's cascade).
func (a *API) handleDeleteMember(w http.ResponseWriter, r *http.Request) {
	id, ok := memberID(w, r)
	if !ok {
		return
	}
	owner := a.memberOwner(w, r, false)
	if owner == nil {
		return
	}
	m, err := a.auth.DeleteMember(r.Context(), owner.ID, id)
	if err != nil {
		a.writeMemberError(w, err, "delete member failed", "could not delete the member", "member", id)
		return
	}
	a.audit(r, "member.delete", m.Username, map[string]any{"member": m.Username})
	w.WriteHeader(http.StatusNoContent)
}

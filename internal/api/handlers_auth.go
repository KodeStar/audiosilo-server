package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/web"
)

// pairingTTL bounds how long an UNLINKED pairing token is valid (/auth/pair,
// demo sessions). Tokens minted by redeeming a code get their lifetime from
// internal/auth instead: invite-derived ones inherit the invite's own expiry -
// so the QR built from one stays scannable for as long as the invite is
// redeemable - and recovery-derived ones carry a short TTL of their own.
const pairingTTL = 10 * time.Minute

// handleServerInfo reports server identity and capabilities. Public so a client
// can discover the server before authenticating; a future federation/directory
// layer can build on it.
func (a *API) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":      "AudioSilo",
		"server_id": a.cfg.ServerID, // stable per-install identity; clients key per-server state on it
		"version":   Version,
		"api":       "v1",
		"capabilities": map[string]bool{
			"admin_ui":   true,                        // baked-in admin console at /admin
			"web_player": web.HasPlayer(a.cfg.WebDir), // web player served at /web (when web_dir is populated)
			"transcode":  a.ffmpeg != "",              // on-the-fly MP3 transcoding via ffmpeg
			"upload":     false,                       // Phase B
			"websocket":  false,                       // Phase C
			"api_keys":   true,                        // user-minted personal access tokens (POST /auth/tokens)
		},
		"auth": map[string]any{
			"methods": []string{"auth_code", "password"},
		},
		"demo": map[string]bool{
			"enabled": a.cfg.Demo.Enabled, // clients show a "Try the demo" affordance
		},
	})
}

// healthTimeout bounds the /healthz database probe so it returns promptly even
// when the writer is stalled (the reader pool answers it).
const healthTimeout = 2 * time.Second

// handleHealth reports whether the database is reachable for reads. Public and
// unauthenticated so it can back a container/orchestrator healthcheck (a wedged
// backend then restarts itself). It probes the reader pool under a short,
// independent deadline, so it stays green while reads serve and only fails when
// the database is genuinely unreachable.
func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if err := a.cat.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRedeem validates an auth code and returns a pairing token plus a QR
// payload, WITHOUT consuming a use - the use is claimed when a device actually
// exchanges (handleExchange), so opening an invite link costs nothing. This
// backs the "enter your auth code" connect screen.
func (a *API) handleRedeem(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !allowAttempt(w, a.redeemLimiter.Allowed(ip)) {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &req, 0); err != nil || req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}
	rc, err := a.auth.ResolveAuthCode(r.Context(), req.Code)
	if err != nil {
		a.redeemLimiter.Fail(ip)
		writeError(w, http.StatusUnauthorized, "invalid or expired auth code")
		return
	}
	a.redeemLimiter.Reset(ip)

	token, err := a.auth.IssuePairingToken(r.Context(), rc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue pairing token")
		return
	}
	payload, err := a.buildPairing(r, token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build pairing payload")
		return
	}
	payload.CodeExpiresAt = rc.ExpiresAt
	payload.UsesRemaining = rc.UsesRemaining()
	writeJSON(w, http.StatusOK, payload)
}

// handleExchange turns a pairing token (scanned from a QR) into a durable,
// device-named session token. This is where an invite use is claimed: a token
// linked to an auth code stays valid afterwards - governed by the code's
// remaining uses and expiry - so one QR can pair several devices; an unlinked
// token (/auth/pair, demo) is single-use and revoked here. Exchange shares the
// redeem limiter since it is now the claim point (a session-issuance failure
// after a successful claim burns a use, same failure shape redeem had).
func (a *API) handleExchange(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !allowAttempt(w, a.redeemLimiter.Allowed(ip)) {
		return
	}
	var req struct {
		PairingToken string `json:"pairing_token"`
		DeviceName   string `json:"device_name"`
	}
	if err := decodeJSON(r, &req, 0); err != nil || req.PairingToken == "" {
		writeError(w, http.StatusBadRequest, "pairing_token is required")
		return
	}
	u, err := a.auth.ConsumePairingToken(r.Context(), req.PairingToken)
	if err != nil {
		a.redeemLimiter.Fail(ip)
		switch {
		case errors.Is(err, auth.ErrCodeExhausted):
			writeError(w, http.StatusUnauthorized, "invite already used on all its devices - ask for a new invite")
		case errors.Is(err, auth.ErrCodeExpired):
			writeError(w, http.StatusUnauthorized, "invite has expired - ask for a new invite")
		default:
			writeError(w, http.StatusUnauthorized, "invalid or expired pairing token")
		}
		return
	}
	a.redeemLimiter.Reset(ip)
	session, err := a.auth.IssueToken(r.Context(), u.ID, auth.KindSession, req.DeviceName, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue session")
		return
	}
	full, err := a.auth.GetUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load account")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     session,
		"user":      full,
		"server_id": a.cfg.ServerID, // so the client keys its per-server state at pairing time
	})
}

// handleLogin authenticates username/password and issues a session token.
func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !allowAttempt(w, a.loginLimiter.Allowed(ip)) {
		return
	}
	var req struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		DeviceName string `json:"device_name"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	u, err := a.auth.Authenticate(r.Context(), req.Username, req.Password)
	if err != nil {
		a.loginLimiter.Fail(ip)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	a.loginLimiter.Reset(ip)
	session, err := a.auth.IssueToken(r.Context(), u.ID, auth.KindSession, req.DeviceName, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue session")
		return
	}
	full, err := a.auth.GetUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load account")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": session, "user": full, "server_id": a.cfg.ServerID})
}

// handlePair issues a fresh pairing QR for the already-authenticated user, e.g.
// to add another device from an existing session. The token is deliberately
// UNLINKED (no parent auth code): single-use and 10-minute, since the user is
// present and can mint another with a tap.
func (a *API) handlePair(w http.ResponseWriter, r *http.Request) {
	if denyAPIKey(w, r) {
		return
	}
	u := userFrom(r.Context())
	token, err := a.auth.IssueToken(r.Context(), u.ID, auth.KindPairing, "", pairingTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue pairing token")
		return
	}
	payload, err := a.buildPairing(r, token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build pairing payload")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleLogout revokes the caller's current session token.
func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := a.auth.RevokeToken(r.Context(), bearerToken(r, false)); err != nil {
		writeError(w, http.StatusInternalServerError, "could not revoke token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the authenticated user, reloaded so the derived
// has_password/has_recovery/last_seen_at fields are present.
func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	full, err := a.auth.GetUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load account")
		return
	}
	writeJSON(w, http.StatusOK, full)
}

// handleSetPassword lets the signed-in user set or change their own password -
// the conventional way back in after a sign-out. The primary case is a
// password-less player setting their first one (no current password to present),
// so no challenge is required then; but changing an existing password requires
// the current one, so a stolen session token can't plant a persistent credential.
// Clearing a password is admin-only (PATCH /admin/users); self-service rejects an
// empty password so a user can't lock themselves out. Refused for demo accounts.
func (a *API) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	if denyAPIKey(w, r) {
		return
	}
	full := a.gateSelfService(w, r)
	if full == nil {
		return
	}
	var req struct {
		Password        string `json:"password"`
		CurrentPassword string `json:"current_password"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "password is required")
		return
	}
	if full.HasPassword {
		if err := a.auth.CheckPassword(r.Context(), full.ID, req.CurrentPassword); err != nil {
			writeError(w, http.StatusUnauthorized, "current password is incorrect")
			return
		}
	}
	if err := a.auth.SetPassword(r.Context(), full.ID, req.Password); err != nil {
		a.writeUserError(w, err, "could not update password")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGenerateRecovery mints (or replaces) the signed-in user's durable
// recovery code and returns it once. Saved by the user, it lets them re-pair on
// any device via the connect screen without an admin - the recovery path for
// password-less accounts. Reuses the redeem flow (it is just an auth code).
// Rate-limited and refused for demo accounts so a throwaway session can't mint a
// durable login.
func (a *API) handleGenerateRecovery(w http.ResponseWriter, r *http.Request) {
	if denyAPIKey(w, r) {
		return
	}
	full := a.gateSelfService(w, r)
	if full == nil {
		return
	}
	code, err := a.auth.GenerateRecoveryCode(r.Context(), full.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not generate recovery code")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"recovery_code": code})
}

// handleDeleteRecovery removes the signed-in user's recovery code.
func (a *API) handleDeleteRecovery(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if err := a.auth.ClearRecoveryCode(r.Context(), u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not clear recovery code")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxAPIKeyLabelLen caps the user-facing label of a personal API key.
const maxAPIKeyLabelLen = 100

// denyAPIKey refuses a request that authenticated with a personal API key
// (auth.KindAPI) on a route that would mint a FRESH durable credential - another
// API key, a recovery code, a pairing token, or a first password. It writes 403
// and returns true when the caller is an api key, so the handler must return;
// a session (or any non-api credential) passes through.
//
// Containment invariant: revoking a leaked API key must cut off everything it
// could reach, so a key can never spawn a credential that survives its own
// revocation (mirrors GitHub's "a token cannot create tokens"). A key still acts
// as its owner everywhere else, including listing/revoking keys and clearing a
// recovery code - those only reduce access, never extend it.
func denyAPIKey(w http.ResponseWriter, r *http.Request) bool {
	if tokenKindFrom(r.Context()) == auth.KindAPI {
		writeError(w, http.StatusForbidden, "not available when authenticating with an API key")
		return true
	}
	return false
}

// gateSelfService applies the guards shared by the self-service account
// endpoints: the per-IP account rate limit and the demo-account refusal. On
// success it returns the caller's freshly loaded account; otherwise it has
// already written the 429/403/500 response and returns nil, so the handler must
// return. Shared by handleSetPassword, handleGenerateRecovery and the API-key
// endpoints so the rate-limit + demo-refusal guard lives in one place.
func (a *API) gateSelfService(w http.ResponseWriter, r *http.Request) *auth.User {
	if !allowAttempt(w, a.accountLimiter.Acquire(clientIP(r))) {
		return nil
	}
	u := userFrom(r.Context())
	full, err := a.auth.GetUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load account")
		return nil
	}
	if full.IsDemo {
		writeError(w, http.StatusForbidden, "not available for demo accounts")
		return nil
	}
	return full
}

// handleCreateAPIToken mints a personal API key ("API key") for the signed-in
// user and returns the plaintext secret exactly once, alongside the stored key's
// metadata. The key acts as its owner on any bearer-authenticated route (an
// admin's key passes requireAdmin) and never expires - revocation is its
// lifecycle. Rate-limited and refused for demo accounts, like recovery/password.
func (a *API) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	if denyAPIKey(w, r) {
		return
	}
	full := a.gateSelfService(w, r)
	if full == nil {
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		writeError(w, http.StatusBadRequest, "label is required")
		return
	}
	if utf8.RuneCountInString(label) > maxAPIKeyLabelLen {
		writeError(w, http.StatusBadRequest, "label is too long")
		return
	}
	secret, meta, err := a.auth.IssueAPIToken(r.Context(), full.ID, label)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create API key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": secret, "api_key": meta})
}

// handleListAPITokens returns the signed-in user's live API keys (metadata
// only - never a secret or hash), newest first.
func (a *API) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	full := a.gateSelfService(w, r)
	if full == nil {
		return
	}
	keys, err := a.auth.ListAPITokens(r.Context(), full.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list API keys")
		return
	}
	if keys == nil {
		keys = []auth.APIToken{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_keys": keys})
}

// handleRevokeAPIToken revokes one of the signed-in user's API keys by id. It is
// owner-scoped and api-kind-only: a missing id, another user's key, or a non-api
// token id all yield 404 (the key can never be another user's, and a session/
// pairing token can't be revoked through this path).
func (a *API) handleRevokeAPIToken(w http.ResponseWriter, r *http.Request) {
	full := a.gateSelfService(w, r)
	if full == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid API key id")
		return
	}
	if err := a.auth.RevokeTokenByID(r.Context(), full.ID, id); err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			writeError(w, http.StatusNotFound, "API key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not revoke API key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

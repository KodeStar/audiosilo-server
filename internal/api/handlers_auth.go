package api

import (
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/web"
)

// pairingTTL bounds how long a pairing token is valid before it must be
// re-issued by redeeming the auth code again.
const pairingTTL = 10 * time.Minute

// handleServerInfo reports server identity and capabilities. Public so a client
// can discover the server before authenticating; a future federation/directory
// layer can build on it.
func (a *API) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "AudioSilo",
		"version": Version,
		"api":     "v1",
		"capabilities": map[string]bool{
			"admin_ui":   true,                        // baked-in admin console at /admin
			"web_player": web.HasPlayer(a.cfg.WebDir), // web player served at /web (when web_dir is populated)
			"transcode":  a.ffmpeg != "",              // on-the-fly MP3 transcoding via ffmpeg
			"upload":     false,                       // Phase B
			"websocket":  false,                       // Phase C
		},
		"auth": map[string]any{
			"methods": []string{"auth_code", "password"},
		},
		"demo": map[string]bool{
			"enabled": a.cfg.Demo.Enabled, // clients show a "Try the demo" affordance
		},
	})
}

// handleRedeem exchanges an auth code for a short-lived pairing token plus a QR
// payload. This backs the "enter your auth code" connect screen.
func (a *API) handleRedeem(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !a.redeemLimiter.Allowed(ip) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &req, 0); err != nil || req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}
	u, err := a.auth.RedeemAuthCode(r.Context(), req.Code)
	if err != nil {
		a.redeemLimiter.Fail(ip)
		writeError(w, http.StatusUnauthorized, "invalid or expired auth code")
		return
	}
	a.redeemLimiter.Reset(ip)

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

// handleExchange turns a pairing token (scanned from a QR) into a durable,
// device-named session token. The pairing token is single-use: it is revoked on
// success.
func (a *API) handleExchange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PairingToken string `json:"pairing_token"`
		DeviceName   string `json:"device_name"`
	}
	if err := decodeJSON(r, &req, 0); err != nil || req.PairingToken == "" {
		writeError(w, http.StatusBadRequest, "pairing_token is required")
		return
	}
	u, err := a.auth.ResolveToken(r.Context(), req.PairingToken, auth.KindPairing)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid or expired pairing token")
		return
	}
	session, err := a.auth.IssueToken(r.Context(), u.ID, auth.KindSession, req.DeviceName, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue session")
		return
	}
	_ = a.auth.RevokeToken(r.Context(), req.PairingToken)
	full, err := a.auth.GetUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load account")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": session,
		"user":  full,
	})
}

// handleLogin authenticates username/password and issues a session token.
func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !a.loginLimiter.Allowed(ip) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
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
	writeJSON(w, http.StatusOK, map[string]any{"token": session, "user": full})
}

// handlePair issues a fresh pairing QR for the already-authenticated user, e.g.
// to add another device from an existing session.
func (a *API) handlePair(w http.ResponseWriter, r *http.Request) {
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
	if err := a.auth.RevokeToken(r.Context(), bearerToken(r)); err != nil {
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

// handleSetPassword lets the signed-in user set or change their own password —
// the conventional way back in after a sign-out. The primary case is a
// password-less player setting their first one (no current password to present),
// so no challenge is required then; but changing an existing password requires
// the current one, so a stolen session token can't plant a persistent credential.
// Clearing a password is admin-only (PATCH /admin/users); self-service rejects an
// empty password so a user can't lock themselves out. Refused for demo accounts.
func (a *API) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	if !a.accountLimiter.Acquire(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	u := userFrom(r.Context())
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
	full, err := a.auth.GetUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load account")
		return
	}
	if full.IsDemo {
		writeError(w, http.StatusForbidden, "not available for demo accounts")
		return
	}
	if full.HasPassword {
		if err := a.auth.CheckPassword(r.Context(), u.ID, req.CurrentPassword); err != nil {
			writeError(w, http.StatusUnauthorized, "current password is incorrect")
			return
		}
	}
	if err := a.auth.SetPassword(r.Context(), u.ID, req.Password); err != nil {
		writeUserError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGenerateRecovery mints (or replaces) the signed-in user's durable
// recovery code and returns it once. Saved by the user, it lets them re-pair on
// any device via the connect screen without an admin — the recovery path for
// password-less accounts. Reuses the redeem flow (it is just an auth code).
// Rate-limited and refused for demo accounts so a throwaway session can't mint a
// durable login.
func (a *API) handleGenerateRecovery(w http.ResponseWriter, r *http.Request) {
	if !a.accountLimiter.Acquire(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	u := userFrom(r.Context())
	full, err := a.auth.GetUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load account")
		return
	}
	if full.IsDemo {
		writeError(w, http.StatusForbidden, "not available for demo accounts")
		return
	}
	code, err := a.auth.GenerateRecoveryCode(r.Context(), u.ID)
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

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
			"transcode":  false,                       // Phase C
			"upload":     false,                       // Phase B
			"websocket":  false,                       // Phase C
		},
		"auth": map[string]any{
			"methods": []string{"auth_code", "password"},
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
	writeJSON(w, http.StatusOK, map[string]any{
		"token": session,
		"user":  u,
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
	writeJSON(w, http.StatusOK, map[string]any{"token": session, "user": u})
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

// handleMe returns the authenticated user.
func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, userFrom(r.Context()))
}

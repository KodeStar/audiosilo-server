package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

// handleDemoSession provisions a throwaway demo account and returns a ready-to-use
// session token plus a pairing payload (QR), so a visitor is immediately logged in
// on the web and can scan the QR to continue on their phone as the same user. It is
// public but gated on demo mode, per-IP rate limited, and capped to bound abuse;
// idle demo accounts are reaped in the background (auth.ReapIdleDemoUsers).
func (a *API) handleDemoSession(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.Demo.Enabled {
		writeError(w, http.StatusNotFound, "demo mode is not enabled")
		return
	}
	// Resolve the demo library first so a misconfigured demo.library fails fast
	// without consuming the caller's per-IP budget.
	lib, err := a.cat.GetLibraryByName(r.Context(), a.cfg.Demo.Library)
	if err != nil {
		a.log.Error("demo: configured library not found", "library", a.cfg.Demo.Library, "err", err)
		writeError(w, http.StatusInternalServerError, "demo library is not available")
		return
	}

	// Acquire atomically records this attempt and reports whether it is under the
	// per-IP cap. Metering at admission (not on success) means a partial failure
	// after CreateUser still counts, and there is no Allowed/Fail race that would
	// let concurrent requests slip past the cap.
	ip := clientIP(r)
	if !a.demoLimiter.Acquire(ip) {
		writeError(w, http.StatusTooManyRequests, "too many demo sessions, try again later")
		return
	}

	var req struct {
		DeviceName string `json:"device_name"`
	}
	_ = decodeJSON(r, &req, 0) // body is optional
	deviceName := req.DeviceName
	if deviceName == "" {
		deviceName = "Demo"
	}

	// Cap live demo accounts so abuse can't grow the database unbounded. An unset
	// demo.max_users falls back to a safe default; an explicit 0 means unlimited.
	if limit := a.cfg.Demo.EffectiveMaxUsers(); limit > 0 {
		n, err := a.auth.CountDemoUsers(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not check demo capacity")
			return
		}
		if n >= limit {
			writeError(w, http.StatusServiceUnavailable, "demo is at capacity, try again later")
			return
		}
	}

	u, err := a.auth.CreateDemoUser(r.Context(), auth.DemoUsernamePrefix+randomDemoSuffix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create demo account")
		return
	}
	if err := a.cat.GrantWholeLibrary(r.Context(), u.ID, lib.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not grant demo library")
		return
	}

	// A session token logs this browser in immediately; a separate single-use
	// pairing token backs the QR so a phone can join as the same demo user.
	session, err := a.auth.IssueToken(r.Context(), u.ID, auth.KindSession, deviceName, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue session")
		return
	}
	pt, err := a.auth.IssueToken(r.Context(), u.ID, auth.KindPairing, "", pairingTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue pairing token")
		return
	}
	payload, err := a.buildPairing(r, pt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build pairing payload")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"token":   session,
		"user":    u,
		"pairing": payload,
	})
}

// randomDemoSuffix returns a short random, username-safe suffix for demo accounts.
func randomDemoSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

package api

import (
	"crypto/subtle"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/web"
)

// The first-run setup wizard (enabled via API.EnableSetup) lets a non-technical
// home user create the admin account in the browser; the admin console's "Set
// up your server" flow (handlers_admin_setup.go) takes it from there. It is
// locked down three ways so it's safe even if the port is exposed:
//   - it is OFF unless the launcher enabled it (setupToken non-empty);
//   - it self-closes the instant an admin exists (setupDone);
//   - POST requires the one-time setup token (carried in the URL fragment, so it
//     never lands in server logs), compared in constant time.

// setupAvailable reports whether the wizard should respond at all: enabled by the
// launcher AND no admin yet. Any error resolving admin state closes the wizard.
func (a *API) setupAvailable(r *http.Request) bool {
	if a.setupToken == "" {
		return false
	}
	exists, err := a.auth.AdminExists(r.Context())
	if err != nil {
		return false
	}
	return !exists
}

// handleSetupPage serves the first-run wizard. When setup isn't available it
// redirects an already-set-up server to /admin, and 404s when the wizard was
// never enabled (so a normal deployment doesn't expose a /setup surface).
func (a *API) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	if !a.setupAvailable(r) {
		if a.setupToken == "" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	html, err := web.Asset("setup.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "setup page unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", web.ContentSecurityPolicy)
	_, _ = w.Write(html)
}

// handleSetup completes first-run setup: it creates the first admin account,
// 201 {user}. The page asks only for the account and hands over to the console's
// setup flow, which adds the library. The library_name/library_root pair an older
// page and the archived desktop manager sent was retired in v2.7.0: a body that
// still carries either is 400 (an unknown field) and creates nothing. It is the
// only unauthenticated way to create an admin, so it is gated by setupAvailable
// + the one-time token.
func (a *API) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !a.setupAvailable(r) {
		writeError(w, http.StatusConflict, "setup is not available (already completed or not enabled)")
		return
	}
	var req struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Constant-time token check (the token rides in the URL fragment, so it is
	// never logged; this still guards against a leaked/guessed value).
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(a.setupToken)) != 1 {
		writeError(w, http.StatusForbidden, "invalid setup token")
		return
	}
	if req.Username == "" {
		req.Username = "admin"
	}
	// CreateUser enforces the password rules (admins must have one). The admin
	// existing closes the wizard; the console's setup flow does the rest.
	adminUser, err := a.auth.CreateUser(r.Context(), req.Username, req.Password, auth.RoleAdmin)
	if err != nil {
		a.writeUserError(w, err, "could not create user")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": adminUser})
}

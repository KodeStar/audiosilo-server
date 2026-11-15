package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/oidc"
)

// Sign in with OpenID Connect (server#235): the browser flow and the console's
// routes. Transport only: internal/oidc is the provider, the flow cookie and
// the decision; auth/identities.go the storage.
//
// GET /api/v1/auth/oidc/start?return=web|admin sets the sealed flow cookie and
// sends the browser to the provider; GET /api/v1/auth/oidc/callback checks it,
// exchanges the code, applies the decision and sends the browser on: a sign-in
// gets a 60 s single-use pairing token, which the console (/admin/#oidc_token=)
// or the web player (/web/connect?token=) exchanges for a session at
// POST /auth/exchange, the one way a session is made. Anything else lands on
// /connect#oidc=<reason>. Every URL the provider or the browser is sent to is
// built from public_url, never from the request's Host.

// Paths and names of the flow.
const (
	oidcStartPath    = "/api/v1/auth/oidc/start"
	oidcCallbackPath = "/api/v1/auth/oidc/callback"
	oidcCookie       = "audiosilo_oidc"
	oidcCookiePath   = "/api/v1/auth/oidc/"
	// oidcPairingTTL is the life of the pairing token a sign-in hands the
	// console or the web player: long enough for the page to load and exchange it.
	oidcPairingTTL = 60 * time.Second
)

// Where a sign-in returns (?return=): an enum, never a URL.
const (
	oidcReturnWeb   = "web"
	oidcReturnAdmin = "admin"
)

// The /connect#oidc= reasons (internal/web/assets/connect.js words each).
const (
	oidcOff         = "off"         // OIDC is not turned on
	oidcUnavailable = "unavailable" // configured wrong, or the provider can't be reached
	oidcPending     = "pending"     // waiting for an admin (no account matched, or not linkable)
	oidcDisabled    = "disabled"    // the account is disabled
	oidcNotAllowed  = "not_allowed" // auto_create on, but not in allowed_group; or a member's account
	oidcFailed      = "failed"      // the flow failed (tampering, a refused token, the provider said no)
	oidcExpired     = "expired"     // the flow cookie is gone or over ten minutes old
	oidcRateLimited = "rate_limited"
)

// oidcFlow is the running OIDC sign-in, built at start from the config the
// server started with (nil when auth.oidc.enabled is off).
type oidcFlow struct {
	name string
	// problem says why OIDC can't run with the configured provider ("" when it
	// can); provider is nil then.
	problem  string
	provider *oidc.Provider
	sealer   *oidc.Sealer
	replay   *oidc.Replay
}

// newOIDCFlow builds the flow for cfg, or nil when OIDC is off. A bad provider
// config is logged and kept as the flow's problem, never an error: the server
// starts without OIDC.
func newOIDCFlow(cfg config.OIDCConfig, log *slog.Logger) *oidcFlow {
	if !cfg.Enabled {
		return nil
	}
	f := &oidcFlow{name: cfg.DisplayName(), replay: oidc.NewReplay(nil)}
	sealer, err := oidc.NewSealer(nil)
	if err != nil {
		f.problem = "could not make the sign-in key"
		log.Error("oidc: sign-in is off", "problem", f.problem, "err", err)
		return f
	}
	f.sealer = sealer
	// public_url is live (checked per request): any placeholder passes here.
	if f.problem = cfg.Problem("-"); f.problem != "" {
		log.Warn("oidc: sign-in is off", "problem", f.problem)
		return f
	}
	secret, err := cfg.Secret()
	if err != nil {
		f.problem = err.Error()
		log.Warn("oidc: sign-in is off", "problem", f.problem)
		return f
	}
	client, err := oidc.HTTPClientFor(cfg.CAFile)
	if err != nil {
		f.problem = err.Error()
		log.Warn("oidc: sign-in is off", "problem", f.problem)
		return f
	}
	f.provider, err = oidc.New(oidc.Settings{
		Issuer: cfg.Issuer, ClientID: strings.TrimSpace(cfg.ClientID), ClientSecret: secret,
		Scopes: cfg.EffectiveScopes(), UsernameClaim: cfg.EffectiveUsernameClaim(), GroupsClaim: cfg.EffectiveGroupsClaim(),
		HTTPClient: client,
	})
	if err != nil {
		f.problem = err.Error()
		log.Warn("oidc: sign-in is off", "problem", f.problem)
	}
	return f
}

// oidcProblem says why the OIDC sign-in can't run now ("" when it can): off,
// a provider config problem, or no public_url.
func (a *API) oidcProblem() string {
	switch {
	case a.oidc == nil:
		return "single sign-on is off"
	case a.oidc.problem != "":
		return a.oidc.problem
	}
	return a.config().Auth.OIDC.Problem(a.config().PublicURL)
}

// oidcOn is the `oidc` capability: configured and able to run (the provider
// may still be unreachable: the button then lands on #oidc=unavailable).
func (a *API) oidcOn() bool { return a.oidcProblem() == "" }

// oidcBase is public_url without its trailing slash.
func (a *API) oidcBase() string { return strings.TrimRight(a.config().PublicURL, "/") }

// oidcRedirectURI is the redirect URI the provider sends people back to.
func (a *API) oidcRedirectURI() string { return a.oidcBase() + oidcCallbackPath }

// oidcResult sends the browser to the connect page's result panel. base is
// public_url when set, else the request's own page (a relative redirect: off
// or misconfigured, nothing to build an absolute URL from).
func (a *API) oidcResult(w http.ResponseWriter, r *http.Request, reason string) {
	noStore(w)
	http.Redirect(w, r, a.oidcBase()+"/connect#oidc="+reason, http.StatusSeeOther)
}

// sameHost reports whether host is public's host (default ports filled from
// public's scheme).
func sameHost(public *url.URL, host string) bool {
	port := "80"
	if public.Scheme == "https" {
		port = "443"
	}
	return strings.EqualFold(config.HostWithPort(host, port), config.HostWithPort(public.Host, port))
}

// randomToken is 32 random bytes, base64url.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// handleOIDCStart begins a sign-in (public): ?return=web|admin (400 for
// anything else). Off or misconfigured: /connect#oidc=off|unavailable. On
// another host than public_url's, it first sends the browser to this route on
// public_url (once: ?hop=1), so the flow cookie is set where the provider
// returns. Then the cookie (sealed state, nonce, PKCE verifier, return; ten
// minutes, this path only, HttpOnly, SameSite=Lax, Secure on an https
// public_url) and a 302 to the provider's authorization URL.
func (a *API) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	// Never the request's own value from here on: the enum's constant.
	var ret string
	switch r.URL.Query().Get("return") {
	case oidcReturnWeb:
		ret = oidcReturnWeb
	case oidcReturnAdmin:
		ret = oidcReturnAdmin
	default:
		writeError(w, http.StatusBadRequest, "return must be web or admin")
		return
	}
	if a.oidc == nil {
		a.oidcResult(w, r, oidcOff)
		return
	}
	if p := a.oidcProblem(); p != "" {
		a.oidcResult(w, r, oidcUnavailable)
		return
	}
	public, err := url.Parse(a.oidcBase())
	if err != nil {
		a.oidcResult(w, r, oidcUnavailable)
		return
	}
	if !sameHost(public, r.Host) && r.URL.Query().Get("hop") == "" {
		noStore(w)
		http.Redirect(w, r, a.oidcBase()+oidcStartPath+"?return="+ret+"&hop=1", http.StatusFound)
		return
	}
	var f oidc.Flow
	if f.State, err = randomToken(); err == nil {
		f.Nonce, err = randomToken()
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the sign-in")
		return
	}
	f.Verifier, f.Return = oauth2.GenerateVerifier(), ret
	authURL, err := a.oidc.provider.AuthURL(r.Context(), a.oidcRedirectURI(), f)
	if err != nil {
		a.log.Warn("oidc: start failed", "err", err)
		a.oidcResult(w, r, oidcUnavailable)
		return
	}
	sealed, err := a.oidc.sealer.Seal(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the sign-in")
		return
	}
	// Secure only on an https public_url: a browser never sends a Secure cookie
	// back over plain http (a server on the home network without TLS).
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows public_url's scheme, see above
		Name: oidcCookie, Value: sealed, Path: oidcCookiePath, MaxAge: int(oidc.FlowMaxAge / time.Second),
		HttpOnly: true, Secure: public.Scheme == "https", SameSite: http.SameSiteLaxMode,
	})
	noStore(w)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOIDCCallback finishes a sign-in (public; the provider's redirect). The
// flow cookie is deleted whatever happens. Refused, with a failure charged to
// the address's oidcLimiter: a missing, foreign or expired cookie, a state that
// isn't the cookie's (constant time), a flow already used (a copied cookie), a
// failed exchange or a refused ID token. The provider's ?error= (the person
// cancelled) is a failure that charges nothing. Otherwise oidc.Decide's answer
// (auth.OIDCSignIn) and its audits, then the pairing token for the flow's
// return, or the result panel.
func (a *API) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows public_url's scheme, as handleOIDCStart's
		Name: oidcCookie, Value: "", Path: oidcCookiePath, MaxAge: -1,
		HttpOnly: true, Secure: strings.HasPrefix(a.oidcBase(), "https:"), SameSite: http.SameSiteLaxMode,
	})
	ip := clientIP(r)
	if !a.oidcLimiter.Allowed(ip) {
		setRetryAfter(w, a.oidcLimiter.LockedFor(ip))
		a.oidcResult(w, r, oidcRateLimited)
		return
	}
	if a.oidc == nil {
		a.oidcResult(w, r, oidcOff)
		return
	}
	if a.oidcProblem() != "" {
		a.oidcResult(w, r, oidcUnavailable)
		return
	}
	fail := func(reason, why string, err error) {
		a.oidcLimiter.Fail(ip)
		if err != nil {
			a.log.Warn("oidc: sign-in refused", "why", why, "err", err)
		} else {
			a.log.Warn("oidc: sign-in refused", "why", why)
		}
		a.oidcResult(w, r, reason)
	}
	q := r.URL.Query()
	c, err := r.Cookie(oidcCookie)
	if err != nil {
		fail(oidcExpired, "no flow cookie", nil)
		return
	}
	f, err := a.oidc.sealer.Open(c.Value)
	switch {
	case errors.Is(err, oidc.ErrFlowExpired):
		fail(oidcExpired, "flow expired", nil)
		return
	case err != nil:
		fail(oidcFailed, "flow cookie refused", nil)
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1 {
		fail(oidcFailed, "state mismatch", nil)
		return
	}
	if !a.oidc.replay.Use(f.State) {
		fail(oidcFailed, "flow used before", nil)
		return
	}
	if e := q.Get("error"); e != "" {
		a.log.Info("oidc: the provider ended the sign-in", "error", clip(e, 64))
		a.oidcResult(w, r, oidcFailed)
		return
	}
	if f.Return != oidcReturnWeb && f.Return != oidcReturnAdmin {
		fail(oidcFailed, "flow return", nil)
		return
	}
	claims, err := a.oidc.provider.Exchange(r.Context(), a.oidcRedirectURI(), q.Get("code"), f)
	if errors.Is(err, oidc.ErrUnavailable) {
		a.log.Warn("oidc: the provider is unavailable", "err", err)
		a.oidcResult(w, r, oidcUnavailable)
		return
	}
	if err != nil {
		fail(oidcFailed, "exchange or token refused", err)
		return
	}
	pol := a.config().Auth.OIDC
	res, err := a.auth.OIDCSignIn(r.Context(), claims, oidc.Policy{
		AutoCreate: pol.AutoCreate, AllowedGroup: pol.AllowedGroup, AdminGroup: pol.AdminGroup,
	})
	if err != nil {
		a.log.Error("oidc: sign-in failed", "err", err)
		a.oidcResult(w, r, oidcFailed)
		return
	}
	a.oidcLimiter.Reset(ip)
	a.auditSignIn(r, claims, res)
	d := res.Decision
	switch d.Outcome {
	case oidc.Pending:
		if d.Reason == oidc.ReasonNotAllowed {
			a.oidcResult(w, r, oidcNotAllowed)
			return
		}
		a.oidcResult(w, r, oidcPending)
		return
	case oidc.Refuse:
		if d.Reason == oidc.ReasonDisabled {
			a.oidcResult(w, r, oidcDisabled)
			return
		}
		a.oidcResult(w, r, oidcNotAllowed)
		return
	}
	u := res.User
	if u.Role == auth.RoleAdmin {
		if err := a.auth.NoteAdminSignIn(r.Context()); err != nil {
			a.log.Warn("noting the admin's first sign-in failed", "err", err)
		}
	}
	token, err := a.auth.IssueToken(r.Context(), u.ID, auth.KindPairing, "", oidcPairingTTL)
	if err != nil {
		a.log.Error("oidc: could not issue the pairing token", "err", err)
		a.oidcResult(w, r, oidcFailed)
		return
	}
	noStore(w)
	if f.Return == oidcReturnAdmin {
		http.Redirect(w, r, a.oidcBase()+"/admin/#oidc_token="+url.QueryEscape(token), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, a.oidcBase()+"/web/connect?token="+url.QueryEscape(token), http.StatusSeeOther)
}

// clip cuts s to n bytes for a log line.
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// auditSignIn records what a sign-in changed, as the server's own act (no
// admin made the request): a link by email, an account made, a role the
// admin_group rule changed or would have (a refused demotion is logged too).
func (a *API) auditSignIn(r *http.Request, c oidc.Claims, res auth.OIDCResult) {
	u := res.User
	if u == nil {
		return
	}
	switch res.Decision.Outcome {
	case oidc.Link:
		a.auditSystem(r, "user.oidc_link", u.Username, map[string]any{"method": auth.LinkAutoEmail, "issuer": c.Issuer})
	case oidc.Create:
		a.auditSystem(r, "user.oidc_create", u.Username, map[string]any{"issuer": c.Issuer})
	}
	if rs := res.Role; rs != nil {
		switch {
		case rs.Refused:
			a.log.Warn("oidc: the last enabled admin left the admin group: still an admin", "user", u.Username)
			a.auditSystem(r, "user.role_sync", u.Username, map[string]any{"role": rs.From, "refused": "last_admin"})
		case rs.Changed():
			a.auditSystem(r, "user.role_sync", u.Username, map[string]any{"from": rs.From, "role": rs.To})
		}
	}
}

// auditSystem records an audit event as the server's own act (no actor).
func (a *API) auditSystem(r *http.Request, action, target string, details map[string]any) {
	e := catalog.AuditEvent{Action: action, Target: target, Details: details, Via: catalog.ViaSystem}
	if err := a.cat.RecordAudit(context.WithoutCancel(r.Context()), e); err != nil {
		a.log.Warn("audit: record failed", "action", action, "err", err)
	}
}

// oidcStatus is GET /admin/oidc/status.
type oidcStatus struct {
	// Enabled: auth.oidc.enabled was on at start. Configured: and it can run
	// (Problem says why not).
	Enabled         bool         `json:"enabled"`
	Configured      bool         `json:"configured"`
	Problem         string       `json:"problem,omitempty"`
	Name            string       `json:"name"`
	Issuer          string       `json:"issuer"`
	ClientID        string       `json:"client_id"`
	ClientSecretSet bool         `json:"client_secret_set"`
	RedirectURI     string       `json:"redirect_uri"`
	Scopes          []string     `json:"scopes"`
	UsernameClaim   string       `json:"username_claim"`
	GroupsClaim     string       `json:"groups_claim"`
	Discovery       *oidc.Status `json:"discovery,omitempty"`
}

// handleOIDCStatus reports the OIDC sign-in's config as running and the
// provider's discovery, trying it when it is due (admin only). Never the
// client secret: only whether one is set.
func (a *API) handleOIDCStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.boot.Auth.OIDC
	st := oidcStatus{
		Enabled: a.oidc != nil, Name: cfg.DisplayName(), Issuer: cfg.Issuer, ClientID: cfg.ClientID,
		ClientSecretSet: cfg.SecretSet(), Scopes: cfg.EffectiveScopes(),
		UsernameClaim: cfg.EffectiveUsernameClaim(), GroupsClaim: cfg.EffectiveGroupsClaim(),
	}
	if a.config().PublicURL != "" {
		st.RedirectURI = a.oidcRedirectURI()
	}
	if a.oidc != nil {
		st.Problem = a.oidcProblem()
		st.Configured = st.Problem == ""
		if a.oidc.provider != nil && st.Configured {
			ds := a.oidc.provider.Check(r.Context())
			st.Discovery = &ds
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// handleOIDCPending lists the sign-ins waiting for an admin (admin only).
func (a *API) handleOIDCPending(w http.ResponseWriter, r *http.Request) {
	rows, err := a.auth.ListPending(r.Context())
	if err != nil {
		a.writeUserError(w, err, "could not list pending sign-ins")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": rows})
}

// handleOIDCPendingLink links a waiting sign-in to an account (admin only):
// {"user_id": n}. 404 for a gone row or account, 409 not_linkable (a member or
// demo account), user_has_identity, identity_linked.
func (a *API) handleOIDCPendingLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		UserID int64 `json:"user_id"`
	}
	if err := decodeJSON(r, &req, 0); err != nil || req.UserID <= 0 {
		writeError(w, http.StatusBadRequest, "user_id is required")
		return
	}
	p, u, err := a.auth.LinkPending(r.Context(), id, req.UserID, userFrom(r.Context()).ID)
	if errors.Is(err, auth.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, codeNotFound, "the sign-in or the account is gone")
		return
	}
	if err != nil {
		a.writeUserError(w, err, "could not link the sign-in")
		return
	}
	a.audit(r, "user.oidc_link", u.Username, map[string]any{"method": auth.LinkAdmin, "issuer": p.Issuer})
	full, err := a.auth.GetUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load user")
		return
	}
	writeJSON(w, http.StatusOK, toAdminUser(full))
}

// handleOIDCPendingCreate makes an ordinary account for a waiting sign-in
// (admin only): {"username": optional} (else derived from the sign-in), 201
// with the account. 409 username_taken, identity_linked.
func (a *API) handleOIDCPendingCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := decodeJSONOptional(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	p, u, err := a.auth.CreateFromPending(r.Context(), id, req.Username, userFrom(r.Context()).ID)
	if errors.Is(err, auth.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, codeNotFound, "the sign-in is gone")
		return
	}
	if err != nil {
		a.writeUserError(w, err, "could not create the account")
		return
	}
	a.audit(r, "user.oidc_create", u.Username, map[string]any{"issuer": p.Issuer})
	writeJSON(w, http.StatusCreated, toAdminUser(u))
}

// handleOIDCPendingDismiss drops a waiting sign-in (admin only); the person's
// next attempt records it again.
func (a *API) handleOIDCPendingDismiss(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	p, err := a.auth.DismissPending(r.Context(), id)
	if errors.Is(err, auth.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, codeNotFound, "the sign-in is gone")
		return
	}
	if err != nil {
		a.writeUserError(w, err, "could not dismiss the sign-in")
		return
	}
	target := p.Username
	if target == "" {
		target = p.Email
	}
	a.audit(r, "oidc.pending_dismiss", target, map[string]any{"issuer": p.Issuer})
	w.WriteHeader(http.StatusNoContent)
}

// handleUnlinkIdentity removes an account's identity (admin only): 404 for
// none, 409 admin_needs_password when it is a password-less admin's last way
// into the console.
func (a *API) handleUnlinkIdentity(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	ident, ok2 := pathInt(r, "identity_id")
	if !ok || !ok2 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	u := a.lookupUser(w, r, id)
	if u == nil {
		return
	}
	got, err := a.auth.UnlinkIdentity(r.Context(), id, ident)
	if errors.Is(err, auth.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, codeNotFound, "no such identity on this account")
		return
	}
	if errors.Is(err, auth.ErrAdminNeedsPassword) {
		writeErrorCode(w, http.StatusConflict, codeAdminNeedsPassword,
			"this admin has no password: set one before unlinking their last single sign-on")
		return
	}
	if err != nil {
		a.writeUserError(w, err, "could not unlink")
		return
	}
	a.audit(r, "user.oidc_unlink", u.Username, map[string]any{"issuer": got.Issuer})
	w.WriteHeader(http.StatusNoContent)
}

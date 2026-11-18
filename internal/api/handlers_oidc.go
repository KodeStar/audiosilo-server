package api

import (
	"cmp"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/oidc"
)

// Sign in with OpenID Connect (server#235): the browser flow and the console's
// routes. Transport only: internal/oidc is the provider, the flow cookie and
// the decision; auth/identities.go the storage.
//
// GET /api/v1/auth/oidc/start?return=web|admin sets the sealed flow's cookie
// (one per flow, named by its state, so sign-ins started in two tabs both
// finish) and sends the browser to the provider; GET /api/v1/auth/oidc/callback checks it,
// exchanges the code, applies the decision and sends the browser on: a sign-in
// gets a 60 s single-use token (auth.KindOIDC) in an HttpOnly cookie of its
// flow's own (never in a URL: a link carrying one would sign whoever follows
// it in as its owner, a login CSRF), and the console
// (/admin/#oidc=done&flow=<key>, or the console page the sign-in started from:
// /start's ?next=) or the web player (/web/connect?oidc=1&flow=<key>) trades
// it for a session at POST /api/v1/auth/oidc/finish, naming the flow, which
// answers as POST /auth/exchange does. Anything else lands on
// /connect#oidc=<reason>. Every URL the provider or the browser is sent to is
// built from public_url, never from the request's Host: the pages on its
// origin (oidcPageBase), the API routes under its path.
//
// The web player may tag its trip: /start's optional ?intent= (a random value
// the player keeps in its tab) rides the sealed flow, then the done cookie,
// and the finish echoes it as "intent", once, only for the sign-in that flow
// completed (audiosilo#202). The player gives a grown-up's pass through kids
// mode back only for its own tag: the address typed later, with a live
// provider session, completes a sign-in too, but never with the player's tag.
// A finish that names the tag it expects ("intent" in its body) is refused
// for any other sign-in before a session is made (403 oidc_intent_mismatch).

// Paths and names of the flow.
const (
	oidcStartPath    = "/api/v1/auth/oidc/start"
	oidcCallbackPath = "/api/v1/auth/oidc/callback"
	oidcFinishPath   = "/api/v1/auth/oidc/finish"
	// oidcCookie names the cookies that hold sealed flows from /start to the
	// callback, one per flow: oidcCookie + flowKey(state), so a second tab's
	// sign-in never replaces the first's (server#406). Their names and path
	// as set depend on public_url's scheme: oidcCookieName.
	oidcCookie = "audiosilo_oidc_flow_"
	// oidcMaxFlows is how many flows a browser holds at once: /start drops
	// the oldest beyond it (every cookie rides on each request to its path),
	// and the callback the oldest finished sign-ins (oidcDoneCookie) beyond it.
	oidcMaxFlows = 4
	// oidcDoneCookie names the cookies that hold finished sign-ins' tokens
	// (auth.KindOIDC) until the page each lands on trades it at
	// oidcFinishPath, one per flow like the flow's own: oidcDoneCookie +
	// flowKey(state), so two tabs whose callbacks both land before either
	// finishes each finish their own.
	oidcDoneCookie = "audiosilo_oidc_done_"
	// oidcCookiePath is the flow's cookies' path on a plain-http public_url, a
	// flow's under public_url's own path too (oidcCookieName).
	oidcCookiePath = "/api/v1/auth/oidc/"
	// hostCookiePrefix makes a browser take the cookie only when it is Secure,
	// set by this origin itself (no Domain) for Path=/: a sibling subdomain
	// or a plain-http answer can't plant one by that name.
	hostCookiePrefix = "__Host-"
	// oidcIntentSep joins a done cookie's token and the flow's intent tag:
	// neither (base64url) holds it.
	oidcIntentSep = "."
	// oidcDoneTTL is the life of the token a sign-in hands the console or
	// the web player (and of its cookie): long enough for the page to load and
	// finish.
	oidcDoneTTL = 60 * time.Second
)

// Where a finished sign-in lands, on public_url's origin (oidcPageBase), with
// oidcFlowParam and its flow's key after: the page then calls oidcFinishPath
// naming the flow.
const (
	oidcDoneAdmin = oidcConsolePath + oidcDoneMarker
	oidcDoneWeb   = "/web/connect?oidc=1"
	// oidcDoneMarker is the console's fragment for a finished sign-in, on its
	// start page or on the deep link the sign-in started from (Flow.Next).
	oidcDoneMarker = "#oidc=done"
	// oidcFlowParam joins the flow's key (flowKey) to the landing page: the
	// web player's query and the console's fragment both end with it.
	oidcFlowParam = "&flow="
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
	// intents: the player's intent tags spent (handleOIDCCallback), a memory
	// of their own so tags never take the places flow states need.
	intents *oidc.Replay
	// noGroupsWarned: the accounts whose sign-in without a groups claim (while
	// admin_group is set) has been logged this run, a noGroupsKey each
	// (auditSignIn).
	noGroupsWarned sync.Map
	// evictions: the waiting rows the list's cap dropped, for auditSignIn's
	// warning.
	evictions evictionTally
}

// evictionTally counts the waiting sign-ins dropped at the list's cap between
// warnings: at the cap every new person's attempt drops one, so the warning
// is logged at most once an hour, with the count since the last.
type evictionTally struct {
	mu       sync.Mutex
	dropped  int64
	loggedAt time.Time
}

// note counts n more rows dropped and returns how many to report now: all
// since the last report when that was an hour or more ago (or never), else 0
// (kept for the next).
func (e *evictionTally) note(n int64, now time.Time) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dropped += n
	if !e.loggedAt.IsZero() && now.Sub(e.loggedAt) < time.Hour {
		return 0
	}
	n, e.dropped, e.loggedAt = e.dropped, 0, now
	return n
}

// noGroupsKey is an account and whether its identity had carried the groups
// claim before: each of its two warnings is logged once a run.
type noGroupsKey struct {
	user int64
	seen bool
}

// newOIDCFlow builds the flow for cfg, or nil when OIDC is off. A bad provider
// config is logged and kept as the flow's problem, never an error: the server
// starts without OIDC.
func newOIDCFlow(cfg config.OIDCConfig, log *slog.Logger) *oidcFlow {
	if !cfg.Enabled {
		return nil
	}
	f := &oidcFlow{name: cfg.DisplayName(), replay: oidc.NewReplay(nil), intents: oidc.NewReplay(nil)}
	sealer, err := oidc.NewSealer(nil)
	if err != nil {
		f.problem = "could not make the sign-in key"
		log.Error("oidc: sign-in is off", "problem", f.problem, "err", err)
		return f
	}
	f.sealer = sealer
	if f.provider, f.problem = buildOIDCProvider(cfg, log); f.problem != "" {
		log.Warn("oidc: sign-in is off", "problem", f.problem)
	}
	return f
}

// buildOIDCProvider makes cfg's provider, or says why it can't.
func buildOIDCProvider(cfg config.OIDCConfig, log *slog.Logger) (*oidc.Provider, string) {
	// public_url is live (checked per request): any placeholder passes here.
	if p := cfg.Problem("-"); p != "" {
		return nil, p
	}
	secret, err := cfg.Secret()
	if err != nil {
		return nil, err.Error()
	}
	client, err := oidc.HTTPClientFor(cfg.CAFile)
	if err != nil {
		return nil, err.Error()
	}
	p, err := oidc.New(oidc.Settings{
		Issuer: cfg.Issuer, ClientID: strings.TrimSpace(cfg.ClientID), ClientSecret: secret,
		UsernameClaim: cfg.EffectiveUsernameClaim(), GroupsClaim: cfg.EffectiveGroupsClaim(),
		HTTPClient: client, Log: log,
	})
	if err != nil {
		return nil, err.Error()
	}
	return p, ""
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

// oidcIssuer is the issuer of the single sign-on that can run now, "" while
// it can't: the provider whose identities are an admin's way into the console
// (auth.SetSSOIssuer).
func (a *API) oidcIssuer() string {
	if !a.oidcOn() {
		return ""
	}
	return a.boot.Auth.OIDC.Issuer
}

// oidcOn is the `oidc` capability: configured and able to run (the provider
// may still be unreachable: the button then lands on #oidc=unavailable).
func (a *API) oidcOn() bool { return a.oidcProblem() == "" }

// oidcBase is public_url without its trailing slash.
func (a *API) oidcBase() string { return strings.TrimRight(a.config().PublicURL, "/") }

// oidcRedirectURI is the redirect URI the provider sends people back to.
func (a *API) oidcRedirectURI() string { return a.oidcBase() + oidcCallbackPath }

// oidcScopes are the scopes a sign-in asks for now: the provider's, read at
// start, with groups among the defaults only while the live settings hold a
// group policy (config.OIDCConfig.GroupPolicy).
func (a *API) oidcScopes() []string {
	return config.OIDCScopes(a.boot.Auth.OIDC.Scopes, a.config().Auth.OIDC.GroupPolicy())
}

// oidcPageBase is where the browser is sent for a page after the provider
// (the console, the web player, the connect page's result panel): public_url's
// origin, scheme and host, without its path. Those pages load their assets and
// route from the origin's root (/admin/, /web/, /assets/), so behind a proxy
// serving the server under a prefix (public_url .../audiosilo/) they route
// only there, and the finish they call is on the origin's own /api/v1 too,
// where the done cookies are (oidcCookieName). The provider's redirect URI and
// the hop to /start keep public_url's path: API routes, served under it.
// Empty when public_url is: a relative redirect to the request's own origin.
func (a *API) oidcPageBase() string {
	base := a.oidcBase()
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return base
	}
	return u.Scheme + "://" + u.Host
}

// oidcResult sends the browser to the connect page's result panel, on
// oidcPageBase (with no public_url, off or misconfigured, the request's own
// origin: a relative redirect).
func (a *API) oidcResult(w http.ResponseWriter, r *http.Request, reason string) {
	noStore(w)
	http.Redirect(w, r, a.oidcPageBase()+"/connect#oidc="+reason, http.StatusSeeOther)
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

// oidcCookieName is the name one of the flow's cookies (oidcCookie,
// oidcDoneCookie) is set and read by, the paths it is set on and whether it
// is Secure: an https public_url (in any case: it keeps the case it was typed
// in). There it is __Host-<name> on Path=/ (the prefix demands that path,
// Secure and no Domain), so neither a sibling subdomain nor a network attacker
// answering plain http can plant one: a planted flow or done cookie would
// sign the browser in as the attacker (a login CSRF). On plain http no prefix
// can be had (it needs Secure), so a network attacker can plant one there;
// the cookies keep the flow's path then, oidcCookiePath, each where it is
// asked for. Under a public_url with a path of its own (a proxy serving the
// server at /audiosilo/, server#406) a flow's cookie is asked for at two: the
// console and the web player ask /start on their origin's own /api/v1, the
// provider sends the callback to /audiosilo/api/v1/auth/oidc/callback (and
// the hop sends /start there). So it is set on both, one copy each, and
// /start sees the flows the browser holds wherever it is asked (heldFlows:
// the cap, the same trip's tag). The done cookies' is the origin's own only:
// the console and the web player call the finish there (oidcPageBase).
func (a *API) oidcCookieName(name string) (cookie string, paths []string, secure bool) {
	base := a.oidcBase()
	if strings.HasPrefix(strings.ToLower(base), "https:") {
		return hostCookiePrefix + name, []string{"/"}, true
	}
	paths = []string{oidcCookiePath}
	if u, err := url.Parse(base); err == nil && strings.HasPrefix(name, oidcCookie) {
		if prefix := strings.TrimRight(u.EscapedPath(), "/"); prefix != "" {
			paths = append(paths, prefix+oidcCookiePath)
		}
	}
	return name, paths, false
}

// setOIDCCookie sets one of the flow's cookies, name (maxAge < 0 deletes it),
// as oidcCookieName names and places it (every path it is held on), HttpOnly
// and SameSite=Lax. Secure only on an https public_url: a browser never sends
// a Secure cookie back over plain http (a server on the home network without
// TLS).
func (a *API) setOIDCCookie(w http.ResponseWriter, name, value string, maxAge int) {
	name, paths, secure := a.oidcCookieName(name)
	for _, path := range paths {
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows public_url's scheme, see above
			Name: name, Value: value, Path: path, MaxAge: maxAge,
			HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
		})
	}
}

// oidcCookieValue is one of the flow's cookies the request carries, by the
// name oidcCookieName gives it ("" when absent).
func (a *API) oidcCookieValue(r *http.Request, name string) string {
	name, _, _ = a.oidcCookieName(name)
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}

// statePattern is a flow's state as /start makes it (43 base64url
// characters), with room to spare: anything else names no flow cookie.
var statePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// flowKey is the part of a flow's state its cookies are named by (oidcCookie
// and oidcDoneCookie + the key), "" for a state no flow of ours has. Only a
// prefix: the cookie's sealed state is still compared whole at the callback.
func flowKey(state string) string {
	if !statePattern.MatchString(state) {
		return ""
	}
	return state[:16]
}

// validFlowKey reports whether s is a flow's key as flowKey makes it: a
// finish's "flow" and a done cookie's name hold one.
func validFlowKey(s string) bool { return s != "" && flowKey(s) == s }

// heldCookie is an OIDC cookie the browser sent: its name as setOIDCCookie
// takes it (the base name + the flow's state or key) and its value.
type heldCookie struct{ name, value string }

// heldCookies are the request's cookies named base + a key valid takes (under
// oidcCookieName's prefix), in the order the browser sent them.
func (a *API) heldCookies(r *http.Request, base string, valid func(string) bool) []heldCookie {
	prefix, _, _ := a.oidcCookieName(base)
	var out []heldCookie
	for _, c := range r.Cookies() {
		if key, found := strings.CutPrefix(c.Name, prefix); found && valid(key) {
			out = append(out, heldCookie{name: base + key, value: c.Value})
		}
	}
	return out
}

// heldFlow is a flow cookie the browser sent: its name as oidcCookieName takes
// it, and its flow (the zero Flow when it doesn't open: IssuedAt 0, no intent).
type heldFlow struct {
	name string
	flow oidc.Flow
}

// heldFlows are the flow cookies the request carries, the sign-ins started in
// this browser (other tabs, trips abandoned at the provider) and not yet
// back, oldest first (one that doesn't open, a restart's or tampered with,
// counts as oldest).
func (a *API) heldFlows(r *http.Request) []heldFlow {
	var out []heldFlow
	for _, c := range a.heldCookies(r, oidcCookie, statePattern.MatchString) {
		f, _ := a.oidc.sealer.Open(c.value) // the zero Flow when it doesn't open
		out = append(out, heldFlow{name: c.name, flow: f})
	}
	slices.SortStableFunc(out, func(x, y heldFlow) int {
		return cmp.Compare(x.flow.IssuedAt, y.flow.IssuedAt)
	})
	return out
}

// oidcNextMax bounds a console deep link carried through a sign-in: with the
// rest of the flow each sealed flow cookie stays under about 1.3 KB, so the
// oidcMaxFlows a page elsewhere can start in this browser keep its requests'
// Cookie header well under a proxy's 8 KB limit.
const oidcNextMax = 512

// oidcConsolePath is where the console's pages live: a deep link is under it.
const oidcConsolePath = "/admin/"

// validNext reports whether s is a console page to return to after a sign-in
// (/start's ?next=, server#406): a path under oidcConsolePath with an optional
// query, printable ASCII (a page's own escapes kept), no fragment (the
// sign-in's marker is the fragment), no backslash, no double quote (a browser
// sends it escaped, and the sealed flow's JSON would spend two bytes on each:
// oidcNextMax), no empty, "." or ".." segment (a browser reads %2e as a dot
// too). Always a path on public_url's own origin: it is appended to that
// origin (oidcPageBase). Anything else is dropped, never an error: the sign-in
// lands on the console's start page.
func validNext(s string) bool {
	if len(s) > oidcNextMax || !strings.HasPrefix(s, oidcConsolePath) {
		return false
	}
	for i := range len(s) {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == '\\' || c == '#' || c == '"' {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return false
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	for i, seg := range segs {
		if seg == "." || seg == ".." || (seg == "" && i != len(segs)-1) || strings.Contains(seg, `\`) {
			return false
		}
	}
	return true
}

// intentPattern is a player's intent tag: 22 to 128 base64url characters (the
// player sends 64 hex).
var intentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22,128}$`)

// validIntent reports whether s can be a player's intent tag (intentPattern).
// Anything else is dropped, never an error: the sign-in runs, untagged.
func validIntent(s string) bool { return intentPattern.MatchString(s) }

// handleOIDCStart begins a sign-in (public): ?return=web|admin (400 for
// anything else), an optional ?intent= tag (validIntent; a bad one is
// dropped, and so is one a flow cookie the browser still holds carries: that
// trip's start sent again) and, for the console, an optional ?next= deep link
// (validNext; a bad one dropped), sealed into the flow. Off or misconfigured:
// /connect#oidc=off|unavailable. On
// another host than public_url's, or over plain HTTP for an https public_url,
// it first sends the browser to this route on public_url (once: ?hop=1), so
// the flow cookie is set where the provider returns (the tag and the deep
// link go along). Then the flow's own cookie, oidcCookie + flowKey(state)
// (sealed state, nonce, PKCE verifier, the redirect URI, return, the tag, the
// deep link; ten minutes, HttpOnly, SameSite=Lax; named and placed by
// oidcCookieName, Secure on an https public_url), beside the flows other tabs
// started (the oldest beyond oidcMaxFlows deleted), and a 302 to the
// provider's authorization URL, asking for
// oidcScopes. A provider discovered before is probed first (within the call
// budget, so a slow one still signs people in; at most every 30 s, a failure
// asked again after 5 s): one gone down lands on #oidc=unavailable rather
// than the browser being sent to it.
func (a *API) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	// Never the request's own value from here on: the enum's constant.
	var ret string
	switch query.Get("return") {
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
	if a.oidcProblem() != "" {
		a.oidcResult(w, r, oidcUnavailable)
		return
	}
	public, err := url.Parse(a.oidcBase())
	if err != nil {
		a.oidcResult(w, r, oidcUnavailable)
		return
	}
	// A plain HTTP request that came straight from the browser (home HTTP on a
	// self-signed server, the same host and port as an https public_url) hops
	// too: a browser drops a Secure cookie set over plain HTTP, and the flow
	// would end on "expired". Behind a proxy the scheme isn't ours to know.
	plain := public.Scheme == "https" && r.TLS == nil && !proxied(r)
	intent := query.Get("intent")
	if !validIntent(intent) {
		intent = ""
	}
	// A console deep link, only for the console.
	next := query.Get("next")
	if ret != oidcReturnAdmin || !validNext(next) {
		next = ""
	}
	if (plain || !sameHost(public, r.Host)) && query.Get("hop") == "" {
		hop := url.Values{"return": {ret}, "hop": {"1"}}
		if intent != "" {
			hop.Set("intent", intent)
		}
		if next != "" {
			hop.Set("next", next)
		}
		noStore(w)
		http.Redirect(w, r, a.oidcBase()+oidcStartPath+"?"+hop.Encode(), http.StatusFound)
		return
	}
	held := a.heldFlows(r)
	// The same trip's start sent again (its address from the history, the trip
	// abandoned at the provider, so its flow cookie is still held and its tag
	// unspent): the new flow goes untagged, never the abandoned trip's tag.
	for _, h := range held {
		if intent != "" && h.flow.Intent == intent {
			intent = ""
		}
	}
	// 32 random bytes each, base64url.
	f := oidc.Flow{State: oauth2.GenerateVerifier(), Nonce: oauth2.GenerateVerifier(),
		Verifier: oauth2.GenerateVerifier(), RedirectURI: a.oidcRedirectURI(), Return: ret, Intent: intent, Next: next}
	authURL, err := a.oidc.provider.AuthURL(r.Context(), f, a.oidcScopes())
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
	// The other tabs' flows stay (server#406); the oldest beyond the cap go.
	for _, h := range held[:max(0, len(held)-(oidcMaxFlows-1))] {
		a.setOIDCCookie(w, h.name, "", -1)
	}
	a.setOIDCCookie(w, oidcCookie+flowKey(f.State), sealed, int(oidc.FlowMaxAge/time.Second))
	noStore(w)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOIDCCallback finishes a sign-in (public; the provider's redirect). The
// flow's cookie, found by the state (flowKey), is deleted whatever happens;
// other tabs' flows are left for their own returns. Refused, with a failure charged to
// the address's oidcLimiter: a foreign or expired cookie, a state that isn't
// the cookie's (constant time), a failed exchange or a refused ID token, a
// flow already used (a copied cookie; checked after the exchange, so only a
// completed flow is remembered). A request with no cookie is refused and
// charges nothing: it reaches nothing to guess at, and any page can make a
// visitor's browser send ten (SameSite=Lax keeps the cookie off them), which
// would lock that address out of signing in. The provider's ?error= (the
// person cancelled) charges nothing either, and lands on "failed" with or
// without a flow: one sent back with no state finds no cookie, and its flow's
// stays until it times out. Otherwise oidc.Decide's answer
// (auth.OIDCSignIn) and its audits, then the sign-in's token in its cookie and
// the flow's return page (a console flow's deep link, Flow.Next, when it
// carried one), or the result panel. The flow's intent tag joins the
// token in the done cookie (oidcIntentSep), once: a tag a completed flow
// carried before (the start URL with it sent again, from the history) is
// dropped, so its sign-in finishes untagged.
func (a *API) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// This flow's cookie, by its state: another tab's flow stays for its own
	// return.
	sealed := ""
	if key := flowKey(q.Get("state")); key != "" {
		sealed = a.oidcCookieValue(r, oidcCookie+key)
		a.setOIDCCookie(w, oidcCookie+key, "", -1)
	}
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
	if sealed == "" {
		// The provider's own ending that names no flow of this browser's (some
		// send ?error= without the state): the sign-in didn't finish, it did
		// not time out. Whose flow it was can't be told, so that flow's cookie
		// stays for its ten minutes, within the cap (oidcMaxFlows).
		if e := q.Get("error"); e != "" {
			a.log.Info("oidc: the provider ended the sign-in", "error", clip(e, 64), "flow", "none")
			a.oidcResult(w, r, oidcFailed)
			return
		}
		a.log.Info("oidc: sign-in refused", "why", "no flow cookie")
		a.oidcResult(w, r, oidcExpired)
		return
	}
	f, err := a.oidc.sealer.Open(sealed)
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
	if e := q.Get("error"); e != "" {
		a.log.Info("oidc: the provider ended the sign-in", "error", clip(e, 64))
		a.oidcResult(w, r, oidcFailed)
		return
	}
	if f.Return != oidcReturnWeb && f.Return != oidcReturnAdmin {
		fail(oidcFailed, "flow return", nil)
		return
	}
	// The policy as it is now (live): read once, so the exchange asks userinfo
	// for the groups the decision below reads.
	pol := a.config().Auth.OIDC
	claims, err := a.oidc.provider.Exchange(r.Context(), q.Get("code"), f, pol.GroupPolicy())
	if errors.Is(err, oidc.ErrUnavailable) {
		a.log.Warn("oidc: the provider is unavailable", "err", err)
		a.oidcResult(w, r, oidcUnavailable)
		return
	}
	if err != nil {
		fail(oidcFailed, "exchange or token refused", err)
		return
	}
	// Once per flow, and only a flow the provider completed takes a place in
	// the memory: a flood of /start states sent back bare (or with ?error=)
	// can't fill it and so refuse everyone's sign-in.
	if !a.oidc.replay.Use(f.State) {
		fail(oidcFailed, "flow used before", nil)
		return
	}
	// The tag (checked at /start, sealed since) is spent with the first flow
	// the provider completes for it, whatever the outcome below (a waiting
	// sign-in approved since must not echo it on a second trip). A full
	// memory refuses it: untagged, the player gives nothing back.
	intent := f.Intent
	if intent != "" && !a.oidc.intents.Use(intent) {
		intent = ""
	}
	// The groups trimmed as the console saves them (config.yaml and the
	// environment aren't): a claim's group is, so " admins" would match no one
	// and every sign-in would lose its admin role. The finish token is minted
	// in the sign-in's own transaction: a sign-out everywhere can't land
	// between the decision and the token and miss it.
	res, err := a.auth.OIDCSignIn(r.Context(), claims, oidc.Policy{
		AutoCreate: pol.AutoCreate, AllowedGroup: strings.TrimSpace(pol.AllowedGroup), AdminGroup: strings.TrimSpace(pol.AdminGroup),
	}, oidcDoneTTL)
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
		// Their next attempt asks userinfo for a claim it lacked this time: an
		// email added at the provider since is what links it to their account.
		a.oidc.provider.AskAgain(claims.Subject)
		a.rt.Notify.OIDCPending(r.Context(), res.PendingID, ip, a.oidc.name, claims.Name, claims.Username, d.Reason)
		if d.Reason == oidc.ReasonNotAllowed {
			a.oidcResult(w, r, oidcNotAllowed)
			return
		}
		a.oidcResult(w, r, oidcPending)
		return
	case oidc.Refuse:
		switch d.Reason {
		case oidc.ReasonDisabled:
			a.oidcResult(w, r, oidcDisabled)
		case oidc.ReasonIncomplete:
			// A first sign-in userinfo couldn't complete: the provider's
			// trouble, worth trying again, never a second account nor a
			// waiting row for someone allowed_group may list.
			a.oidcResult(w, r, oidcUnavailable)
		default:
			a.oidcResult(w, r, oidcNotAllowed)
		}
		return
	}
	if res.User.Role == auth.RoleAdmin {
		if err := a.auth.NoteAdminSignIn(r.Context()); err != nil {
			a.log.Warn("noting the admin's first sign-in failed", "err", err)
		}
	}
	done := res.Token
	if intent != "" {
		done += oidcIntentSep + intent
	}
	// The flow's own done cookie, named by the key its flow cookie had (the
	// state matched it whole above), beside other tabs' finished sign-ins,
	// the oldest beyond oidcMaxFlows deleted as /start deletes flows: on an
	// https public_url each rides on every request to the origin, and a page
	// elsewhere can complete trips a live provider session doesn't stop.
	held := a.heldCookies(r, oidcDoneCookie, validFlowKey)
	for _, h := range held[:max(0, len(held)-(oidcMaxFlows-1))] {
		a.setOIDCCookie(w, h.name, "", -1)
	}
	key := flowKey(f.State)
	a.setOIDCCookie(w, oidcDoneCookie+key, done, int(oidcDoneTTL/time.Second))
	noStore(w)
	page := oidcDoneWeb
	if f.Return == oidcReturnAdmin {
		page = oidcDoneAdmin
		// The console page the sign-in started from (checked at /start, sealed
		// since; checked again, so only a console path is ever appended).
		if validNext(f.Next) {
			page = f.Next + oidcDoneMarker
		}
	}
	http.Redirect(w, r, a.oidcPageBase()+page+oidcFlowParam+key, http.StatusSeeOther)
}

// handleOIDCFinish trades a finished sign-in for a session (public): the
// token the callback left in its flow's done cookie (auth.KindOIDC, never a
// pairing token: one planted in that cookie, an invite's QR or a person's own
// /auth/pair, would sign the browser in as someone else, and is refused and
// left unspent), consumed once with the device's session minted in the same
// transaction (auth.ConsumeOIDC: a 500, uncharged, when the session can't be
// written, the token left unspent), then the answer POST /auth/exchange gives
// (the session envelope; the new device reported as there), with "intent":
// the tag the flow carried (handleOIDCStart), only when it did. A tag rides
// only with the token it was set beside, so it is echoed once. Optional body
// {"device_name", "intent", "flow"}. "flow" is the flow's key the landing page
// was given (oidcFlowParam): the finish takes that flow's done cookie only, so
// two tabs whose callbacks both landed each finish their own (a malformed one
// is a 400). Without it (a page from before the key) heldDone picks one.
// "intent" is the tag the player expects, and a sign-in whose flow carried
// another tag or none is refused, 403 code oidc_intent_mismatch, before its
// token is tried: no session, the token unspent, nothing charged (no secret
// was tried), its cookie cleared, so the sign-in the player disowned can't be
// finished without the tag instead. Present means a string, the empty one too;
// absent or null asks nothing (the console, a player before the field). The
// done cookie stays while the token is untried or unspent: a 429
// (oidcLimiter), a 400 (the body) and a 500 (the store failed, so nothing was
// spent: a retry finishes it, for the rest of its minute). It is cleared once
// the token is spent (a session) or refused, and on the 401s and the 403.
// 401 code oidc_no_sign_in without the cookie, whatever
// the body expects (charging nothing: no secret was tried),
// oidc_sign_in_expired for a token spent, expired, unknown, of another kind,
// or its account's disabled or now a member (charged to the address's
// oidcLimiter, the callback's), and while single sign-on can't run now (turned
// off or misconfigured since the callback: its token outlives a restart;
// nothing charged, the token left to expire). Not redeemLimiter: any page can
// make a visitor's browser fail ten exchanges or redeems (a simple POST), which
// would lock that address out of finishing a sign-in the provider completed.
// A page on another site can't make a browser send the cookie here
// (SameSite=Lax, a POST), nor read the answer.
func (a *API) handleOIDCFinish(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	ip := clientIP(r)
	if !allowAttempt(w, a.oidcLimiter, ip) {
		return
	}
	var req struct {
		DeviceName string `json:"device_name"`
		// Intent: the tag the player expects the sign-in to carry, nil for
		// no expectation.
		Intent *string `json:"intent"`
		// Flow: the key of the flow whose sign-in this page finishes, ""
		// from a page before the key.
		Flow string `json:"flow"`
	}
	if err := decodeJSONOptional(r, &req, 0); err != nil || (req.Flow != "" && !validFlowKey(req.Flow)) {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	held := a.heldDone(r, req.Flow, req.Intent)
	dropHeld := func() {
		if held.name != "" {
			a.setOIDCCookie(w, held.name, "", -1)
		}
	}
	token, intent, _ := strings.Cut(held.value, oidcIntentSep)
	if !validIntent(intent) {
		intent = ""
	}
	if token == "" {
		dropHeld()
		writeErrorCode(w, http.StatusUnauthorized, codeOIDCNoSignIn, "no single sign-on to finish in this browser")
		return
	}
	if !a.oidcOn() {
		dropHeld()
		writeErrorCode(w, http.StatusUnauthorized, codeOIDCSignInExpired, "single sign-on is off: sign in another way")
		return
	}
	// Before the token is tried: a sign-in that isn't the trip the player
	// started makes no session and stays unspent (to expire, out of reach once
	// the browser drops the cookie).
	if req.Intent != nil && (intent == "" || subtle.ConstantTimeCompare([]byte(*req.Intent), []byte(intent)) != 1) {
		dropHeld()
		writeErrorCode(w, http.StatusForbidden, codeOIDCIntentMismatch, "this single sign-on isn't the one this page started: sign in again")
		return
	}
	u, session, err := a.auth.ConsumeOIDC(r.Context(), token, req.DeviceName)
	switch {
	case auth.Refused(err):
		dropHeld()
		a.oidcLimiter.Fail(ip)
		writeErrorCode(w, http.StatusUnauthorized, codeOIDCSignInExpired, "the single sign-on has expired or was used: sign in again")
		return
	case err != nil:
		// Nothing was spent (the token and the session share one
		// transaction), so the cookie stays for the page's retry.
		a.log.Error("oidc: finishing a sign-in failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not finish the single sign-on: try again")
		return
	}
	dropHeld()
	a.oidcLimiter.Reset(ip)
	out := a.pairedSession(r, u, session, req.DeviceName, false)
	if intent != "" {
		out["intent"] = intent
	}
	writeJSON(w, http.StatusOK, out)
}

// heldDone is the finished sign-in a finish is for: the done cookie of flow
// (the key the landing page was given) when the finish names one, else (a
// page from before the key) the first held whose tag is the one expected
// (intent; none when not given, so a page that expects no tag takes an
// untagged sign-in before another tab's tagged trip), else the first held.
// The zero value when the browser holds none. Its value is the token, and the
// tag after oidcIntentSep.
func (a *API) heldDone(r *http.Request, flow string, intent *string) heldCookie {
	if flow != "" {
		if v := a.oidcCookieValue(r, oidcDoneCookie+flow); v != "" {
			return heldCookie{name: oidcDoneCookie + flow, value: v}
		}
		return heldCookie{}
	}
	want := ""
	if intent != nil {
		want = *intent
	}
	var first heldCookie
	for _, h := range a.heldCookies(r, oidcDoneCookie, validFlowKey) {
		if h.value == "" {
			continue
		}
		if _, tag, _ := strings.Cut(h.value, oidcIntentSep); subtle.ConstantTimeCompare([]byte(tag), []byte(want)) == 1 {
			return h
		}
		if first.name == "" {
			first = h
		}
	}
	return first
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
// A sign-in with no groups claim while admin_group is set is logged once per
// account this run (its role was left alone; the identity's earlier claims
// say whether the provider stopped sending it), and waiting sign-ins that
// pushed the oldest out of the list are logged for the admin (at most once an
// hour, with how many were dropped since: evictionTally).
func (a *API) auditSignIn(r *http.Request, c oidc.Claims, res auth.OIDCResult) {
	if n := res.PendingEvicted; n > 0 {
		if n = a.oidc.evictions.note(n, time.Now()); n > 0 {
			a.log.Warn("oidc: the waiting sign-ins are at their cap: the oldest were dropped", "dropped", n)
		}
	}
	u := res.User
	if u == nil {
		return
	}
	if res.Decision.NoGroups {
		if _, warned := a.oidc.noGroupsWarned.LoadOrStore(noGroupsKey{u.ID, res.GroupsSeen}, true); !warned {
			msg := "oidc: admin_group is set but the provider sent no groups claim (never for this person): the role is unchanged"
			if res.GroupsSeen {
				msg = "oidc: admin_group is set but the provider sent no groups claim this time (it did before): the role is unchanged; " +
					"check groups_claim and the provider's groups mapper, or change the role by hand"
			}
			a.log.Warn(msg, "user", u.Username, "groups_claim", a.boot.Auth.OIDC.EffectiveGroupsClaim())
		}
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

// handleOIDCStatus reports the OIDC sign-in's config as running (the scopes a
// sign-in asks for now) and the provider's discovery, trying it when it is due
// and probing a provider discovered before (admin only). Never the client
// secret: only whether one is set.
func (a *API) handleOIDCStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.boot.Auth.OIDC
	st := oidcStatus{
		Enabled: a.oidc != nil, Name: cfg.DisplayName(), Issuer: cfg.Issuer, ClientID: cfg.ClientID,
		ClientSecretSet: cfg.SecretSet(), Scopes: a.oidcScopes(),
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

// handleOIDCPending lists the sign-ins waiting for an admin (admin only), and
// `linked_user_ids`: per issuer among them, the accounts already holding an
// identity from it, which Link would refuse (409 user_has_identity).
func (a *API) handleOIDCPending(w http.ResponseWriter, r *http.Request) {
	rows, err := a.auth.ListPending(r.Context())
	if err != nil {
		a.writeUserError(w, err, "could not list pending sign-ins")
		return
	}
	linked, err := a.auth.LinkedAccounts(r.Context())
	if err != nil {
		a.writeUserError(w, err, "could not list pending sign-ins")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": rows, "linked_user_ids": linked})
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
	writeJSON(w, http.StatusOK, a.adminUserOf(r.Context(), u))
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
	writeJSON(w, http.StatusCreated, a.adminUserOf(r.Context(), u))
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
	// Never the email: the audit keeps none (CONVENTIONS.md, Email).
	target := p.Username
	if target == "" {
		target = p.Subject
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

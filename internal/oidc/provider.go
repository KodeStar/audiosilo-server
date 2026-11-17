package oidc

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// SigningAlgs are the ID token signatures accepted: asymmetric only. An HMAC
// (HS*) token is signed with the client secret, which a public client doesn't
// have and a confidential one shares; "none" is never a signature.
var SigningAlgs = []string{
	gooidc.RS256, gooidc.RS384, gooidc.RS512,
	gooidc.PS256, gooidc.PS384, gooidc.PS512,
	gooidc.ES256, gooidc.ES384, gooidc.ES512,
	gooidc.EdDSA,
}

// Settings configure a Provider (from config.OIDCConfig, read at start). The
// scopes are no setting: AuthURL takes them per sign-in, since whether the
// groups scope is asked for follows the live group policy.
type Settings struct {
	Issuer        string
	ClientID      string
	ClientSecret  string // "" for a public client (PKCE alone)
	UsernameClaim string
	GroupsClaim   string
	// HTTPClient makes every call to the provider (discovery, keys, token,
	// userinfo); nil is HTTPClientFor("").
	HTTPClient *http.Client
	// Budget bounds one use's calls to the provider together (0 = CallBudget;
	// tests shorten it).
	Budget time.Duration
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
	// Log takes the warnings a sign-in that goes on gives (userinfo failed, so
	// the ID token's claims are used); nil discards them.
	Log *slog.Logger
}

// CallBudget bounds a sign-in's calls to the provider together: AuthURL's
// discovery when it is due, or Exchange's discovery, token request, keys fetch
// and userinfo share one deadline. It starts once the use is past any wait for
// another use's discovery (discoveryWait), so together they fit under the
// API's 30 s request timeout (api's requestTimeout), and a slow provider ends
// the sign-in on its result panel, not on the timeout's bare 503.
const CallBudget = 20 * time.Second

// discoveryWait bounds how long a use waits for a discovery another use has
// under way before it ends as unavailable; that wait is not charged to its
// own CallBudget.
const discoveryWait = 5 * time.Second

// UseTime is the longest one use of the provider takes: a wait for another
// use's discovery, then its own CallBudget.
const UseTime = discoveryWait + CallBudget

// The provider probe: once discovered, a provider is asked for its discovery
// document again (probeTimeout) before a sign-in is sent there or the console
// shows its state, at most every probeEvery, so a provider gone down is shown
// as unavailable rather than as the connection error the browser would meet.
const (
	probeTimeout = 3 * time.Second
	probeEvery   = 30 * time.Second
)

// userinfoRecheck is how long a claim userinfo didn't hold for a person (the
// email, the username) is not asked of it again for that person when the ID
// token lacks only that. Per person: one account without an email says nothing
// of the next, whose email may decide which account a first sign-in links to;
// and only while their sign-ins sign in (AskAgain: the email of a person left
// waiting decides the same at their next attempt).
// maxUserinfoLacks bounds the people remembered; past it a person is asked
// every time.
const (
	userinfoRecheck  = time.Hour
	maxUserinfoLacks = 1000
)

// userinfoLack is until when userinfo is not asked for a person's email or
// username again (zero: ask).
type userinfoLack struct{ email, username time.Time }

// HTTPClientFor is the client a Provider uses: CallBudget as its timeout (what
// bounds the key set's own fetch, which runs on the provider's context, not a
// request's) and, with caFile set, that PEM file's certificates trusted beside
// the system's.
func HTTPClientFor(caFile string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // G304: an admin-configured path
		if err != nil {
			return nil, fmt.Errorf("ca_file can't be read: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("ca_file holds no PEM certificate")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Timeout: CallBudget, Transport: tr}, nil
}

// Discovery backoff: a failed discovery is tried again no sooner than
// minBackoff, doubling to maxBackoff.
const (
	minBackoff = time.Minute
	maxBackoff = 15 * time.Minute
)

// Discovery states (Status.State).
const (
	StatePending = "pending" // not tried yet
	StateOK      = "ok"
	StateError   = "error"
)

// Status is the provider's discovery as the console shows it.
type Status struct {
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

// ErrUnavailable is any call while discovery hasn't succeeded, and an
// Exchange the provider didn't answer within the call budget.
var ErrUnavailable = errors.New("oidc: the provider is unavailable")

// Provider is one OpenID provider: discovered on first use (and retried with
// backoff), then fixed for the life of the process, its reachability probed
// (probe) while it is in use.
type Provider struct {
	s      Settings
	client *http.Client
	ctx    context.Context // carries client for go-oidc and x/oauth2
	log    *slog.Logger

	mu sync.Mutex
	// discovering is closed when the discovery under way ends (nil: none).
	discovering chan struct{}
	provider    *gooidc.Provider
	verifier    *gooidc.IDTokenVerifier
	lastErr     string
	checkedAt   time.Time
	nextTry     time.Time
	backoff     time.Duration
	// The last probe: when, and what failed ("" when the provider answered).
	probedAt time.Time
	probeErr string
	// lacks: per subject, until when userinfo is not asked for the email or
	// the username again (it didn't hold that claim for the person).
	lacks map[string]userinfoLack

	probeMu sync.Mutex // one probe at a time; the others take its answer
}

// New returns a Provider; nothing is fetched until it is used.
func New(s Settings) (*Provider, error) {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Budget <= 0 {
		s.Budget = CallBudget
	}
	client := s.HTTPClient
	if client == nil {
		var err error
		if client, err = HTTPClientFor(""); err != nil {
			return nil, err
		}
	}
	ctx := gooidc.ClientContext(context.Background(), client)
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	log := s.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Provider{s: s, client: client, ctx: ctx, log: log, lacks: map[string]userinfoLack{}}, nil
}

// Status reports the discovery's state.
func (p *Provider) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Status{State: StatePending}
	at := p.checkedAt
	switch {
	case p.provider != nil && p.probeErr != "":
		st.State, st.Error = StateError, p.probeErr
	case p.provider != nil:
		st.State = StateOK
	case p.lastErr != "":
		st.State, st.Error = StateError, p.lastErr
	}
	if p.provider != nil && p.probedAt.After(at) {
		at = p.probedAt
	}
	if !at.IsZero() {
		st.CheckedAt = at.UTC().Format(time.RFC3339)
	}
	return st
}

// Check tries discovery when it is due (not during a backoff), probes a
// provider discovered before, and reports the state: what the console's status
// asks.
func (p *Provider) Check(ctx context.Context) Status {
	ctx, cancel, _, _, err := p.begin(ctx)
	defer cancel()
	if err == nil {
		_ = p.probe(ctx)
	}
	return p.Status()
}

// budget is ctx bounded by the provider's call budget: every call one use
// makes shares it.
func (p *Provider) budget(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.s.Budget)
}

// outOfTime is err, or ErrUnavailable when the use's budget ran out (or its
// caller left): a provider too slow to answer refused nothing, so the sign-in
// ends as unavailable and charges the address no failure.
func outOfTime(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w: no answer within the sign-in's time (%v)", ErrUnavailable, err)
	}
	return err
}

// begin starts one use of the provider: the provider discovered (once, and
// not during a backoff: ErrUnavailable wraps why it isn't) and ctx bounded by
// the use's budget. The budget starts once the use is past any wait for a
// discovery another use has under way (at most discoveryWait, else
// ErrUnavailable): that wait is not charged to it, while a discovery it runs
// itself is. The cancel func is never nil.
func (p *Provider) begin(ctx context.Context) (context.Context, context.CancelFunc, *gooidc.Provider, *gooidc.IDTokenVerifier, error) {
	noop := func() {}
	waitUntil := time.Now().Add(discoveryWait)
	for {
		p.mu.Lock()
		if p.provider != nil {
			prov, ver := p.provider, p.verifier
			p.mu.Unlock()
			bctx, cancel := p.budget(ctx)
			return bctx, cancel, prov, ver, nil
		}
		now := p.s.Now()
		if now.Before(p.nextTry) {
			err := fmt.Errorf("%w: %s", ErrUnavailable, p.lastErr)
			p.mu.Unlock()
			return ctx, noop, nil, nil, err
		}
		if ch := p.discovering; ch != nil {
			p.mu.Unlock()
			t := time.NewTimer(time.Until(waitUntil))
			select {
			case <-ch:
				t.Stop()
				continue
			case <-t.C:
				return ctx, noop, nil, nil, fmt.Errorf("%w: the provider's discovery is still under way", ErrUnavailable)
			case <-ctx.Done():
				t.Stop()
				return ctx, noop, nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
			}
		}
		p.discovering = make(chan struct{})
		p.mu.Unlock()
		bctx, cancel := p.budget(ctx)
		prov, ver, err := p.discover(bctx, now)
		if err != nil {
			cancel()
			return ctx, noop, nil, nil, err
		}
		return bctx, cancel, prov, ver, nil
	}
}

// discover runs the discovery begin started (p.discovering set), records its
// outcome and ends it. It keeps ctx's deadline (the use's budget) but not its
// cancellation.
func (p *Provider) discover(ctx context.Context, now time.Time) (*gooidc.Provider, *gooidc.IDTokenVerifier, error) {
	// Not the caller's cancellation: a browser that leaves mid-discovery would
	// otherwise count as the provider failing and start a backoff for everyone.
	// Its deadline still holds: the budget the rest of the use shares.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(p.s.Budget)
	}
	dctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	prov, err := gooidc.NewProvider(gooidc.ClientContext(dctx, p.client), p.s.Issuer)
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.discovering)
	p.discovering = nil
	p.checkedAt = now
	if err != nil {
		p.backoff = min(max(p.backoff*2, minBackoff), maxBackoff)
		p.nextTry = now.Add(p.backoff)
		p.lastErr = discoveryError(err)
		return nil, nil, fmt.Errorf("%w: %s", ErrUnavailable, p.lastErr)
	}
	// A discovery that just answered is a probe that did.
	p.provider, p.lastErr, p.backoff = prov, "", 0
	p.probedAt, p.probeErr = now, ""
	// The key set's fetches use the provider's client, not the request's
	// context (which ends with the request).
	p.verifier = prov.VerifierContext(p.ctx, &gooidc.Config{
		ClientID:             p.s.ClientID,
		SupportedSigningAlgs: SigningAlgs,
		Now:                  p.s.Now,
	})
	return p.provider, p.verifier, nil
}

// probe asks a discovered provider for its discovery document again
// (probeTimeout), unless it was asked within probeEvery (that answer stands):
// ErrUnavailable when it can't be reached or doesn't answer 200. Discovery
// itself is fixed for the process; the probe only says whether the provider
// is there now. Not the caller's cancellation: its answer is everyone's.
func (p *Provider) probe(ctx context.Context) error {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	now := p.s.Now()
	p.mu.Lock()
	at, last := p.probedAt, p.probeErr
	p.mu.Unlock()
	if !at.IsZero() && !now.Before(at) && now.Sub(at) < probeEvery {
		return probeResult(last)
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
	defer cancel()
	msg := ""
	req, err := http.NewRequestWithContext(pctx, http.MethodGet,
		strings.TrimSuffix(p.s.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err == nil {
		var resp *http.Response
		if resp, err = p.client.Do(req); err == nil {
			// Drained so the connection is reused by the sign-in that follows.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				msg = "the provider answered " + resp.Status
			}
		}
	}
	if err != nil {
		msg = "the provider can't be reached: " + capped(withoutBody(err.Error()), 300)
	}
	p.mu.Lock()
	p.probedAt, p.probeErr = now, msg
	p.mu.Unlock()
	return probeResult(msg)
}

// probeResult is a probe's failure as an error (nil for none).
func probeResult(msg string) error {
	if msg == "" {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrUnavailable, msg)
}

// discoveryError words a failed discovery for the console (no response body:
// a provider's error page can be long).
func discoveryError(err error) string {
	var mismatch *gooidc.IssuerMismatchError
	if errors.As(err, &mismatch) {
		return fmt.Sprintf("the provider calls itself %q, not %q: set issuer to exactly that", mismatch.Discovered, mismatch.Provided)
	}
	return "discovery failed: " + capped(withoutBody(err.Error()), 300)
}

// capped cuts a provider failure's wording to limit bytes (the console, a log
// line).
func capped(msg string, limit int) string {
	if len(msg) > limit {
		return msg[:limit] + "..."
	}
	return msg
}

// keysErrors are how go-oidc starts a failed fetch of the provider's keys (a
// signature that can't be checked): "<status> <response body>" or
// "<error> <response body>" follows.
var keysErrors = []string{"oidc: get keys failed: ", "oidc: failed to decode keys: "}

// withoutBody cuts go-oidc's "<status>: <response body>" error (a failed
// discovery or userinfo request) down to the status, and a failed keys fetch
// down to its status code.
func withoutBody(msg string) string {
	status, _, found := strings.Cut(msg, ": ")
	if found && len(status) >= 4 && status[3] == ' ' && strings.Trim(status[:3], "0123456789") == "" {
		return status
	}
	for _, m := range keysErrors {
		i := strings.Index(msg, m)
		if i < 0 {
			continue
		}
		if rest := msg[i+len(m):]; len(rest) >= 3 && strings.Trim(rest[:3], "0123456789") == "" {
			return msg[:i+len(m)+3]
		}
		return msg[:i+len(m)-2]
	}
	return msg
}

// oauthConfig is the client's config for a flow's redirect URI.
func (p *Provider) oauthConfig(prov *gooidc.Provider, redirectURI string) *oauth2.Config {
	ep := prov.Endpoint()
	if p.s.ClientSecret == "" {
		ep.AuthStyle = oauth2.AuthStyleInParams // a public client: client_id in the body, no secret
	}
	return &oauth2.Config{
		ClientID:     p.s.ClientID,
		ClientSecret: p.s.ClientSecret,
		Endpoint:     ep,
		RedirectURL:  redirectURI,
	}
}

// AuthURL is the provider's authorization URL for a flow asking for scopes:
// its redirect URI, state, nonce and the PKCE S256 challenge of its verifier.
// A provider discovered before is probed first (probe), so one gone down ends
// the sign-in here as unavailable rather than sending the browser to it.
func (p *Provider) AuthURL(ctx context.Context, f Flow, scopes []string) (string, error) {
	ctx, cancel, prov, _, err := p.begin(ctx)
	defer cancel()
	if err != nil {
		return "", err
	}
	if err := p.probe(ctx); err != nil {
		return "", err
	}
	cfg := p.oauthConfig(prov, f.RedirectURI)
	cfg.Scopes = scopes
	return cfg.AuthCodeURL(f.State, gooidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)), nil
}

// Errors from Exchange, beyond ErrUnavailable. None carries the code or a token.
var (
	ErrExchange    = errors.New("oidc: the code exchange failed")
	ErrIDToken     = errors.New("oidc: the ID token was refused")
	ErrNoIDToken   = errors.New("oidc: the token response held no ID token")
	ErrNoSubject   = errors.New("oidc: the ID token names no subject")
	ErrNonce       = errors.New("oidc: the ID token's nonce is not this flow's")
	ErrAudience    = errors.New("oidc: the ID token's authorized party is not this client")
	ErrIssuedAt    = errors.New("oidc: the ID token's issue time is missing or in the future")
	ErrSubMismatch = errors.New("oidc: userinfo is for another subject")
)

// issuedAtLeeway is how far in the future an ID token's iat may be (clock skew).
const issuedAtLeeway = 5 * time.Minute

// Exchange trades a callback's code for the person's claims: the token request
// with the flow's redirect URI and PKCE verifier, then the ID token's checks
// (signature by an allowed algorithm from the provider's keys, iss, aud holding
// the client id, azp when present or when there are several audiences, exp,
// iat no more than five minutes ahead, the flow's nonce), then userinfo for
// what the ID token lacks (askUserinfo; its sub must be the ID token's). The
// groups claim is asked of userinfo only when needGroups (a group policy is
// set: nothing else reads it). A userinfo request that fails leaves the ID
// token's claims, a warning logged and Claims.Partial set: a claim it can't
// add is absent, which never signs anyone in as more (and Decide doesn't
// decide a first sign-in on an email, or allowed_group on groups, it lacks for
// that). Every call shares one deadline, the call budget.
func (p *Provider) Exchange(ctx context.Context, code string, f Flow, needGroups bool) (Claims, error) {
	ctx, cancel, prov, verifier, err := p.begin(ctx)
	defer cancel()
	if err != nil {
		return Claims{}, err
	}
	octx := context.WithValue(ctx, oauth2.HTTPClient, p.client)
	tok, err := p.oauthConfig(prov, f.RedirectURI).Exchange(octx, code, oauth2.VerifierOption(f.Verifier))
	if err != nil {
		return Claims{}, outOfTime(ctx, fmt.Errorf("%w: %s", ErrExchange, exchangeError(err)))
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Claims{}, ErrNoIDToken
	}
	idt, err := verifier.Verify(gooidc.ClientContext(ctx, p.client), raw)
	if err != nil {
		return Claims{}, outOfTime(ctx, fmt.Errorf("%w: %s", ErrIDToken, exchangeError(err)))
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.Nonce)) != 1 {
		return Claims{}, ErrNonce
	}
	if idt.IssuedAt.IsZero() || idt.IssuedAt.After(p.s.Now().Add(issuedAtLeeway)) {
		return Claims{}, ErrIssuedAt
	}
	var set claimSet
	if err := idt.Claims(&set); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrIDToken, err)
	}
	azp := set.str("azp")
	if (azp != "" || len(idt.Audience) > 1) && azp != p.s.ClientID {
		return Claims{}, ErrAudience
	}
	c := Claims{Issuer: p.s.Issuer, Subject: clean(idt.Subject, maxClaim)}
	if c.Subject == "" || c.Subject != idt.Subject {
		return Claims{}, ErrNoSubject
	}
	c.fill(set, p.s.UsernameClaim, p.s.GroupsClaim)
	if !p.askUserinfo(c, needGroups) || prov.UserInfoEndpoint() == "" {
		return c, nil
	}
	ui, err := prov.UserInfo(gooidc.ClientContext(ctx, p.client), oauth2.StaticTokenSource(tok))
	if err != nil {
		p.log.Warn("oidc: userinfo failed: signing in with the ID token's claims",
			"err", outOfTime(ctx, errors.New(exchangeError(err))))
		c.Partial = true
		return c, nil
	}
	if ui.Subject != idt.Subject {
		return Claims{}, ErrSubMismatch
	}
	var uset claimSet
	if err := ui.Claims(&uset); err != nil {
		p.log.Warn("oidc: userinfo unreadable: signing in with the ID token's claims", "err", err)
		c.Partial = true
		return c, nil
	}
	before := c
	c.fill(uset, p.s.UsernameClaim, p.s.GroupsClaim)
	p.noteUserinfo(before, c)
	return c, nil
}

// askUserinfo reports whether userinfo is worth asking for what c (the ID
// token's claims) lacks: the groups claim when needGroups, every time (a
// role or an account may hang on it); the email or the username unless
// userinfo lacked that claim for this person within userinfoRecheck
// (noteUserinfo), so a claim the provider never sends them doesn't cost a
// request at each of their sign-ins (AskAgain ends that for a person whose
// sign-in was left waiting).
func (p *Provider) askUserinfo(c Claims, needGroups bool) bool {
	if needGroups && c.Groups == nil {
		return true
	}
	now := p.s.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	l := p.lacks[c.Subject]
	return (c.Email == "" && !now.Before(l.email)) ||
		(c.Username == "" && !now.Before(l.username))
}

// noteUserinfo records which claims the ID token lacked (before) and userinfo
// didn't add (after) for the person: not asked of it again for them for
// userinfoRecheck. With maxUserinfoLacks people remembered (the expired
// dropped first), a new one isn't: they are asked again next time.
func (p *Provider) noteUserinfo(before, after Claims) {
	now := p.s.Now()
	until := now.Add(userinfoRecheck)
	p.mu.Lock()
	defer p.mu.Unlock()
	l, known := p.lacks[after.Subject]
	note := func(lackedUntil *time.Time, lacked, got bool) {
		if lacked {
			*lackedUntil = until
			if got {
				*lackedUntil = time.Time{}
			}
		}
	}
	note(&l.email, before.Email == "", after.Email != "")
	note(&l.username, before.Username == "", after.Username != "")
	if l == (userinfoLack{}) {
		delete(p.lacks, after.Subject)
		return
	}
	if !known && len(p.lacks) >= maxUserinfoLacks {
		for k, v := range p.lacks {
			if !now.Before(v.email) && !now.Before(v.username) {
				delete(p.lacks, k)
			}
		}
		if len(p.lacks) >= maxUserinfoLacks {
			return
		}
	}
	p.lacks[after.Subject] = l
}

// AskAgain forgets what userinfo lacked for subject, so their next sign-in
// asks it again. For a sign-in that signed no one in (left waiting): the
// person may come back within userinfoRecheck with the claim it lacked (an
// email added at the provider), which decides what that sign-in links to or
// whether it makes a second account.
func (p *Provider) AskAgain(subject string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.lacks, subject)
}

// exchangeError words a failed token or userinfo request, or a refused ID
// token, without a response body (which some providers fill with the request's
// own parameters) and cut to 200 bytes (the provider's error code included).
func exchangeError(err error) string {
	msg := withoutBody(err.Error())
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		switch {
		case re.ErrorCode != "":
			msg = re.ErrorCode
		case re.Response != nil:
			msg = re.Response.Status
		}
	}
	return capped(msg, 200)
}

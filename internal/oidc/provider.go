package oidc

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
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

// Settings configure a Provider (from config.OIDCConfig, read at start).
type Settings struct {
	Issuer        string
	ClientID      string
	ClientSecret  string // "" for a public client (PKCE alone)
	Scopes        []string
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
}

// CallBudget bounds a sign-in's calls to the provider together: AuthURL's
// discovery when it is due, or Exchange's discovery, token request, keys fetch
// and userinfo share one deadline. It fits under the API's 30 s request timeout
// (api's requestTimeout), so a slow provider ends the sign-in on its result
// panel, not on the timeout's bare 503.
const CallBudget = 20 * time.Second

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
// backoff), then fixed for the life of the process.
type Provider struct {
	s      Settings
	client *http.Client
	ctx    context.Context // carries client for go-oidc and x/oauth2

	mu        sync.Mutex
	provider  *gooidc.Provider
	verifier  *gooidc.IDTokenVerifier
	lastErr   string
	checkedAt time.Time
	nextTry   time.Time
	backoff   time.Duration
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
	return &Provider{s: s, client: client, ctx: ctx}, nil
}

// Status reports the discovery's state.
func (p *Provider) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Status{State: StatePending}
	switch {
	case p.provider != nil:
		st.State = StateOK
	case p.lastErr != "":
		st.State, st.Error = StateError, p.lastErr
	}
	if !p.checkedAt.IsZero() {
		st.CheckedAt = p.checkedAt.UTC().Format(time.RFC3339)
	}
	return st
}

// Check tries discovery when it is due (not during a backoff) and reports the
// state: what the console's status asks.
func (p *Provider) Check(ctx context.Context) Status {
	ctx, cancel := p.budget(ctx)
	defer cancel()
	_, _, _ = p.ready(ctx)
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

// ready discovers the provider once (one attempt at a time, none during a
// backoff): ErrUnavailable wraps why it isn't. Discovery keeps ctx's deadline
// (the use's budget) but not its cancellation.
func (p *Provider) ready(ctx context.Context) (*gooidc.Provider, *gooidc.IDTokenVerifier, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return p.provider, p.verifier, nil
	}
	now := p.s.Now()
	if now.Before(p.nextTry) {
		return nil, nil, fmt.Errorf("%w: %s", ErrUnavailable, p.lastErr)
	}
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
	p.checkedAt = now
	if err != nil {
		p.backoff = min(max(p.backoff*2, minBackoff), maxBackoff)
		p.nextTry = now.Add(p.backoff)
		p.lastErr = discoveryError(err)
		return nil, nil, fmt.Errorf("%w: %s", ErrUnavailable, p.lastErr)
	}
	p.provider, p.lastErr, p.backoff = prov, "", 0
	// The key set's fetches use the provider's client, not the request's
	// context (which ends with the request).
	p.verifier = prov.VerifierContext(p.ctx, &gooidc.Config{
		ClientID:             p.s.ClientID,
		SupportedSigningAlgs: SigningAlgs,
		Now:                  p.s.Now,
	})
	return p.provider, p.verifier, nil
}

// discoveryError words a failed discovery for the console (no response body:
// a provider's error page can be long).
func discoveryError(err error) string {
	var mismatch *gooidc.IssuerMismatchError
	if errors.As(err, &mismatch) {
		return fmt.Sprintf("the provider calls itself %q, not %q: set issuer to exactly that", mismatch.Discovered, mismatch.Provided)
	}
	msg := withoutBody(err.Error())
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return "discovery failed: " + msg
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

// oauthConfig is the client's config for redirectURI.
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
		Scopes:       p.s.Scopes,
	}
}

// AuthURL is the provider's authorization URL for a flow: state, nonce and the
// PKCE S256 challenge of verifier.
func (p *Provider) AuthURL(ctx context.Context, redirectURI string, f Flow) (string, error) {
	ctx, cancel := p.budget(ctx)
	defer cancel()
	prov, _, err := p.ready(ctx)
	if err != nil {
		return "", err
	}
	return p.oauthConfig(prov, redirectURI).AuthCodeURL(f.State,
		gooidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)), nil
}

// Errors from Exchange, beyond ErrUnavailable. None carries the code or a token.
var (
	ErrExchange    = errors.New("oidc: the code exchange failed")
	ErrIDToken     = errors.New("oidc: the ID token was refused")
	ErrUserinfo    = errors.New("oidc: userinfo was refused")
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
// with the flow's PKCE verifier, then the ID token's checks (signature by an
// allowed algorithm from the provider's keys, iss, aud holding the client id,
// azp when present or when there are several audiences, exp, iat no more than
// five minutes ahead, the flow's nonce), then userinfo for what the ID token
// lacks (its sub must be the ID token's). Every call shares one deadline, the
// call budget.
func (p *Provider) Exchange(ctx context.Context, redirectURI, code string, f Flow) (Claims, error) {
	ctx, cancel := p.budget(ctx)
	defer cancel()
	prov, verifier, err := p.ready(ctx)
	if err != nil {
		return Claims{}, err
	}
	octx := context.WithValue(ctx, oauth2.HTTPClient, p.client)
	tok, err := p.oauthConfig(prov, redirectURI).Exchange(octx, code, oauth2.VerifierOption(f.Verifier))
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
	if !c.complete() && prov.UserInfoEndpoint() != "" {
		ui, err := prov.UserInfo(gooidc.ClientContext(ctx, p.client), oauth2.StaticTokenSource(tok))
		if err != nil {
			return Claims{}, outOfTime(ctx, fmt.Errorf("%w: %s", ErrUserinfo, exchangeError(err)))
		}
		if ui.Subject != idt.Subject {
			return Claims{}, ErrSubMismatch
		}
		var uset claimSet
		if err := ui.Claims(&uset); err != nil {
			return Claims{}, fmt.Errorf("%w: %v", ErrUserinfo, err)
		}
		c.fill(uset, p.s.UsernameClaim, p.s.GroupsClaim)
	}
	return c, nil
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
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return msg
}

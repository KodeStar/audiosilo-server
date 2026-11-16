package oidc_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/kodestar/audiosilo-server/internal/oidc"
	"github.com/kodestar/audiosilo-server/internal/oidc/oidctest"
)

const redirectURI = "https://audiosilo.test/api/v1/auth/oidc/callback"

// scopes are what the tests ask for.
var scopes = []string{"openid", "email"}

var alice = oidctest.Person{Subject: "sub-alice", Email: "alice@example.org", EmailVerified: true,
	PreferredUsername: "alice", Name: "Alice A", Groups: []string{"listeners", "audiosilo-admins"}}

func newProvider(t *testing.T, idp *oidctest.Provider, secret string) *oidc.Provider {
	t.Helper()
	p, err := oidc.New(oidc.Settings{Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: secret,
		UsernameClaim: "preferred_username", GroupsClaim: "groups"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// signIn runs the flow to the callback: the authorize redirect's code and state.
func signIn(t *testing.T, p *oidc.Provider, f oidc.Flow) (code string) {
	t.Helper()
	authURL, err := p.AuthURL(context.Background(), redirectURI, f, scopes)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if loc.Query().Get("state") != f.State {
		t.Fatalf("state %q came back as %q", f.State, loc.Query().Get("state"))
	}
	return loc.Query().Get("code")
}

func flow() oidc.Flow {
	return oidc.Flow{State: "state-1", Nonce: "nonce-1", Verifier: oauth2.GenerateVerifier(), Return: "admin"}
}

func TestExchangeAllowed(t *testing.T) {
	t.Parallel()
	for _, secret := range []string{"s3cret", ""} {
		idp := oidctest.New(t, "audiosilo", secret, alice)
		p := newProvider(t, idp, secret)
		f := flow()
		code := signIn(t, p, f)
		q := idp.LastAuthorize()
		if q.Get("code_challenge_method") != "S256" || q.Get("nonce") != f.Nonce || q.Get("redirect_uri") != redirectURI ||
			!strings.Contains(q.Get("scope"), "openid") {
			t.Fatalf("authorize query: %v", q)
		}
		c, err := p.Exchange(context.Background(), redirectURI, code, f, true)
		if err != nil {
			t.Fatalf("secret %q: %v", secret, err)
		}
		if c.Issuer != idp.Issuer || c.Subject != "sub-alice" || c.Email != "alice@example.org" || !c.EmailVerified ||
			c.Username != "alice" || c.Name != "Alice A" || len(c.Groups) != 2 {
			t.Fatalf("claims: %+v", c)
		}
		if n := idp.Calls("userinfo"); n != 0 {
			t.Fatalf("userinfo asked %d times for a complete ID token", n)
		}
		if p.Status().State != oidc.StateOK {
			t.Fatalf("status %+v", p.Status())
		}
		// The code works once.
		if _, err := p.Exchange(context.Background(), redirectURI, code, f, true); !errors.Is(err, oidc.ErrExchange) {
			t.Fatalf("a replayed code: %v", err)
		}
	}
}

func TestExchangeUserinfoFillsGaps(t *testing.T) {
	t.Parallel()
	idp := oidctest.New(t, "audiosilo", "s3cret", alice)
	idp.SetKnobs(oidctest.Knobs{UserinfoOnly: true})
	p := newProvider(t, idp, "s3cret")
	f := flow()
	c, err := p.Exchange(context.Background(), redirectURI, signIn(t, p, f), f, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Email != "alice@example.org" || !c.EmailVerified || c.Username != "alice" || len(c.Groups) != 2 {
		t.Fatalf("claims from userinfo: %+v", c)
	}
	// userinfo for another subject is refused.
	idp.SetKnobs(oidctest.Knobs{UserinfoOnly: true, UserinfoSubject: "sub-mallory"})
	f = flow()
	if _, err := p.Exchange(context.Background(), redirectURI, signIn(t, p, f), f, true); !errors.Is(err, oidc.ErrSubMismatch) {
		t.Fatalf("userinfo of another subject: %v", err)
	}
}

func TestExchangeEmailVerifiedString(t *testing.T) {
	t.Parallel()
	p2 := alice
	p2.EmailVerified = "true"
	idp := oidctest.New(t, "audiosilo", "s3cret", p2)
	p := newProvider(t, idp, "s3cret")
	f := flow()
	c, err := p.Exchange(context.Background(), redirectURI, signIn(t, p, f), f, true)
	if err != nil || !c.EmailVerified {
		t.Fatalf("email_verified \"true\": %+v %v", c, err)
	}
	p2.EmailVerified = false
	idp.SetPerson(p2)
	f = flow()
	if c, err := p.Exchange(context.Background(), redirectURI, signIn(t, p, f), f, true); err != nil || c.EmailVerified {
		t.Fatalf("email_verified false: %+v %v", c, err)
	}
}

// TestExchangeDenied: each check refuses a token that fails it.
func TestExchangeDenied(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		knobs oidctest.Knobs
		want  error
	}{
		{"another issuer", oidctest.Knobs{TokenIssuer: "https://evil.example"}, oidc.ErrIDToken},
		{"another audience", oidctest.Knobs{Audience: []string{"other-client"}}, oidc.ErrIDToken},
		{"azp of another client", oidctest.Knobs{AZP: "other-client"}, oidc.ErrAudience},
		{"several audiences without azp", oidctest.Knobs{Audience: []string{"audiosilo", "other"}}, oidc.ErrAudience},
		{"expired", oidctest.Knobs{ExpiresIn: -time.Minute}, oidc.ErrIDToken},
		{"issued in the future", oidctest.Knobs{IssuedAt: 10 * time.Minute}, oidc.ErrIssuedAt},
		{"no iat", oidctest.Knobs{NoIssuedAt: true}, oidc.ErrIssuedAt},
		{"another nonce", oidctest.Knobs{Nonce: "nonce-of-another-flow"}, oidc.ErrNonce},
		{"HS256 (symmetric)", oidctest.Knobs{Alg: "HS256"}, oidc.ErrIDToken},
		{"signed by an unknown key", oidctest.Knobs{Alg: "other-key"}, oidc.ErrIDToken},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			idp := oidctest.New(t, "audiosilo", "s3cret", alice)
			idp.SetKnobs(c.knobs)
			p := newProvider(t, idp, "s3cret")
			f := flow()
			if _, err := p.Exchange(context.Background(), redirectURI, signIn(t, p, f), f, true); !errors.Is(err, c.want) {
				t.Fatalf("got %v; want %v", err, c.want)
			}
		})
	}
	// Allowed twins of the azp rules: several audiences with this client's azp,
	// and one audience with its azp.
	for _, k := range []oidctest.Knobs{{Audience: []string{"audiosilo", "other"}, AZP: "audiosilo"}, {AZP: "audiosilo"}, {IssuedAt: 4 * time.Minute}} {
		idp := oidctest.New(t, "audiosilo", "s3cret", alice)
		idp.SetKnobs(k)
		p := newProvider(t, idp, "s3cret")
		f := flow()
		if _, err := p.Exchange(context.Background(), redirectURI, signIn(t, p, f), f, true); err != nil {
			t.Fatalf("%+v: %v", k, err)
		}
	}
}

func TestExchangePKCEAndClient(t *testing.T) {
	t.Parallel()
	idp := oidctest.New(t, "audiosilo", "s3cret", alice)
	p := newProvider(t, idp, "s3cret")
	f := flow()
	code := signIn(t, p, f)
	wrong := f
	wrong.Verifier = oauth2.GenerateVerifier()
	if _, err := p.Exchange(context.Background(), redirectURI, code, wrong, true); !errors.Is(err, oidc.ErrExchange) {
		t.Fatalf("another verifier: %v", err)
	}
	// Another redirect URI than the authorize request's.
	f = flow()
	code = signIn(t, p, f)
	if _, err := p.Exchange(context.Background(), "https://evil.example/cb", code, f, true); !errors.Is(err, oidc.ErrExchange) {
		t.Fatalf("another redirect URI: %v", err)
	}
	// A wrong client secret.
	bad := newProvider(t, idp, "wrong")
	f = flow()
	code = signIn(t, bad, f)
	if _, err := bad.Exchange(context.Background(), redirectURI, code, f, true); !errors.Is(err, oidc.ErrExchange) {
		t.Fatalf("a wrong secret: %v", err)
	}
}

func TestDiscoveryBackoff(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	idp := oidctest.New(t, "audiosilo", "s3cret", alice)
	idp.SetKnobs(oidctest.Knobs{FailDiscovery: true})
	p, err := oidc.New(oidc.Settings{Issuer: idp.Issuer, ClientID: "audiosilo", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if st := p.Status(); st.State != oidc.StatePending {
		t.Fatalf("before use: %+v", st)
	}
	if _, err := p.AuthURL(context.Background(), redirectURI, flow(), scopes); !errors.Is(err, oidc.ErrUnavailable) {
		t.Fatalf("discovery down: %v", err)
	}
	if st := p.Status(); st.State != oidc.StateError || st.Error == "" || st.CheckedAt == "" {
		t.Fatalf("after a failure: %+v", st)
	}
	idp.SetKnobs(oidctest.Knobs{})
	// Within the backoff: not asked again.
	if _, err := p.AuthURL(context.Background(), redirectURI, flow(), scopes); !errors.Is(err, oidc.ErrUnavailable) {
		t.Fatalf("within the backoff: %v", err)
	}
	if n := idp.Calls("discovery"); n != 1 {
		t.Fatalf("discovery asked %d times within the backoff", n)
	}
	now = now.Add(time.Minute)
	if _, err := p.AuthURL(context.Background(), redirectURI, flow(), scopes); err != nil {
		t.Fatalf("after the backoff: %v", err)
	}
	if st := p.Status(); st.State != oidc.StateOK {
		t.Fatalf("after recovery: %+v", st)
	}
}

// TestDiscoveryOutlivesTheCaller: a caller that leaves mid-discovery (its
// context cancelled) is not the provider failing: discovery still runs, and
// no backoff starts for everyone else.
func TestDiscoveryOutlivesTheCaller(t *testing.T) {
	t.Parallel()
	idp := oidctest.New(t, "audiosilo", "s3cret", alice)
	p := newProvider(t, idp, "s3cret")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = p.AuthURL(ctx, redirectURI, flow(), scopes)
	if st := p.Status(); st.State != oidc.StateOK {
		t.Fatalf("a cancelled caller counted against the provider: %+v", st)
	}
	if _, err := p.AuthURL(context.Background(), redirectURI, flow(), scopes); err != nil {
		t.Fatalf("the next caller: %v", err)
	}
}

func TestDiscoveryIssuerMismatch(t *testing.T) {
	t.Parallel()
	idp := oidctest.New(t, "audiosilo", "s3cret", alice)
	idp.SetKnobs(oidctest.Knobs{DiscoveryIssuer: "https://elsewhere.example/"})
	p := newProvider(t, idp, "s3cret")
	if _, err := p.AuthURL(context.Background(), redirectURI, flow(), scopes); !errors.Is(err, oidc.ErrUnavailable) {
		t.Fatalf("issuer mismatch: %v", err)
	}
	if st := p.Status(); !strings.Contains(st.Error, "https://elsewhere.example/") {
		t.Fatalf("the error doesn't name the discovered issuer: %+v", st)
	}
}

// TestCallsShareOneBudget: a sign-in's calls to the provider (discovery, token,
// keys, userinfo) share one deadline. Each slow answer alone is well inside the
// budget, so only a shared deadline ends the use that needs four; the same
// provider within a larger budget signs in.
func TestCallsShareOneBudget(t *testing.T) {
	t.Parallel()
	const delay = 150 * time.Millisecond
	for _, tc := range []struct {
		budget time.Duration
		ok     bool
	}{{400 * time.Millisecond, false}, {5 * time.Second, true}} {
		idp := oidctest.New(t, "audiosilo", "s3cret", alice)
		settings := oidc.Settings{Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: "s3cret",
			UsernameClaim: "preferred_username", GroupsClaim: "groups", Budget: tc.budget}
		p, err := oidc.New(settings)
		if err != nil {
			t.Fatal(err)
		}
		// The authorize step on its own (its discovery is the use's only call).
		f := flow()
		code := signIn(t, p, f)
		idp.SetKnobs(oidctest.Knobs{Delay: delay, UserinfoOnly: true})
		// A fresh provider so the exchange discovers again: four slow calls.
		p, err = oidc.New(settings)
		if err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		_, err = p.Exchange(context.Background(), redirectURI, code, f, true)
		took := time.Since(began)
		if tc.ok && err != nil {
			t.Fatalf("budget %v: %v", tc.budget, err)
		}
		// Out of time is the provider unavailable (nothing refused), not a failure.
		if !tc.ok && (!errors.Is(err, oidc.ErrUnavailable) || took > tc.budget+delay) {
			t.Fatalf("budget %v: err %v after %v", tc.budget, err, took)
		}
	}
}

func TestHTTPClientFor(t *testing.T) {
	t.Parallel()
	if _, err := oidc.HTTPClientFor(t.TempDir() + "/missing.pem"); err == nil {
		t.Fatal("a missing ca_file was accepted")
	}
	c, err := oidc.HTTPClientFor("")
	if err != nil || c.Timeout == 0 {
		t.Fatalf("default client: %+v %v", c, err)
	}
}

// TestAuthURLAsksForTheGivenScopes: the scopes are the sign-in's (the live
// group policy decides whether groups is among them), not fixed at start.
func TestAuthURLAsksForTheGivenScopes(t *testing.T) {
	t.Parallel()
	idp := oidctest.New(t, "audiosilo", "s3cret", alice)
	p := newProvider(t, idp, "s3cret")
	for _, want := range []string{"openid profile email", "openid profile email groups"} {
		signInWith(t, p, flow(), strings.Fields(want))
		if got := idp.LastAuthorize().Get("scope"); got != want {
			t.Fatalf("scope %q, want %q", got, want)
		}
	}
}

// signInWith is signIn asking for scopes.
func signInWith(t *testing.T, p *oidc.Provider, f oidc.Flow, sc []string) {
	t.Helper()
	authURL, err := p.AuthURL(context.Background(), redirectURI, f, sc)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// TestUserinfoFailureFallsBack: a userinfo request that fails leaves the ID
// token's claims (allowed), while userinfo for another subject is still
// refused (denied, TestExchangeUserinfoFillsGaps).
func TestUserinfoFailureFallsBack(t *testing.T) {
	t.Parallel()
	person := alice
	person.PreferredUsername = ""
	idp := oidctest.New(t, "audiosilo", "s3cret", person)
	idp.SetKnobs(oidctest.Knobs{FailUserinfo: true})
	p := newProvider(t, idp, "s3cret")
	f := flow()
	c, err := p.Exchange(context.Background(), redirectURI, signIn(t, p, f), f, true)
	if err != nil {
		t.Fatalf("a userinfo failure ended the sign-in: %v", err)
	}
	if idp.Calls("userinfo") != 1 || c.Subject != "sub-alice" || c.Email != "alice@example.org" || len(c.Groups) != 2 || c.Username != "" {
		t.Fatalf("claims: %+v (userinfo asked %d times)", c, idp.Calls("userinfo"))
	}
}

// TestUserinfoNotAskedForANeverSentClaim: a claim userinfo didn't hold (the
// username here) is not asked of it again at the next sign-in, until the
// recheck; the groups claim, when a group policy needs it, is asked every
// time, and without one it is never the reason to ask.
func TestUserinfoNotAskedForANeverSentClaim(t *testing.T) {
	t.Parallel()
	now := time.Now()
	person := alice
	person.PreferredUsername = ""
	idp := oidctest.New(t, "audiosilo", "s3cret", person)
	idp.SetKnobs(oidctest.Knobs{ExpiresIn: 3 * time.Hour})
	p, err := oidc.New(oidc.Settings{Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: "s3cret",
		UsernameClaim: "preferred_username", GroupsClaim: "groups", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	exchange := func(needGroups bool) {
		t.Helper()
		f := flow()
		if _, err := p.Exchange(context.Background(), redirectURI, signIn(t, p, f), f, needGroups); err != nil {
			t.Fatal(err)
		}
	}
	exchange(false)
	exchange(false)
	if n := idp.Calls("userinfo"); n != 1 {
		t.Fatalf("userinfo asked %d times for a username it never sends, want 1", n)
	}
	// Groups left out of the ID token: asked every time a policy needs them.
	person.Groups = nil
	idp.SetPerson(person)
	exchange(true)
	exchange(true)
	if n := idp.Calls("userinfo"); n != 3 {
		t.Fatalf("userinfo asked %d times, want 3 (groups needed twice)", n)
	}
	exchange(false)
	if n := idp.Calls("userinfo"); n != 3 {
		t.Fatalf("userinfo asked for groups no policy needs: %d", n)
	}
	// Past the recheck: asked again.
	now = now.Add(time.Hour + time.Minute)
	exchange(false)
	if n := idp.Calls("userinfo"); n != 4 {
		t.Fatalf("userinfo not asked again after the recheck: %d", n)
	}
}

// TestProbeSeesTheProviderGone: once discovered, a provider that stops
// answering is seen at the next sign-in's start (after the probe's interval)
// and in the status, rather than the browser being sent to it; once it is
// back, so is the sign-in.
func TestProbeSeesTheProviderGone(t *testing.T) {
	t.Parallel()
	now := time.Now()
	idp := oidctest.New(t, "audiosilo", "s3cret", alice)
	p, err := oidc.New(oidc.Settings{Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: "s3cret",
		UsernameClaim: "preferred_username", GroupsClaim: "groups", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AuthURL(context.Background(), redirectURI, flow(), scopes); err != nil {
		t.Fatal(err)
	}
	idp.SetKnobs(oidctest.Knobs{FailDiscovery: true})
	// Allowed within the interval: the last answer stands, nothing asked.
	if _, err := p.AuthURL(context.Background(), redirectURI, flow(), scopes); err != nil {
		t.Fatalf("within the probe's interval: %v", err)
	}
	if n := idp.Calls("discovery"); n != 1 {
		t.Fatalf("discovery asked %d times within the interval", n)
	}
	// Denied: past it, the provider is asked and found gone.
	now = now.Add(31 * time.Second)
	if _, err := p.AuthURL(context.Background(), redirectURI, flow(), scopes); !errors.Is(err, oidc.ErrUnavailable) {
		t.Fatalf("a provider gone down: %v", err)
	}
	if st := p.Check(context.Background()); st.State != oidc.StateError || !strings.Contains(st.Error, "500") {
		t.Fatalf("status of a provider gone down: %+v", st)
	}
	idp.SetKnobs(oidctest.Knobs{})
	now = now.Add(31 * time.Second)
	if st := p.Check(context.Background()); st.State != oidc.StateOK {
		t.Fatalf("status once it is back: %+v", st)
	}
	if _, err := p.AuthURL(context.Background(), redirectURI, flow(), scopes); err != nil {
		t.Fatalf("a sign-in once it is back: %v", err)
	}
}

// TestBudgetStartsAfterAnotherDiscovery: a use that waits for a discovery
// another use has under way is not charged that wait: its own calls still get
// the whole budget. Here the wait plus its two slow calls (token, keys) is
// over the budget, the two calls alone well within it.
func TestBudgetStartsAfterAnotherDiscovery(t *testing.T) {
	t.Parallel()
	const delay = 200 * time.Millisecond
	idp := oidctest.New(t, "audiosilo", "s3cret", alice)
	settings := oidc.Settings{Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: "s3cret",
		UsernameClaim: "preferred_username", GroupsClaim: "groups", Budget: 500 * time.Millisecond}
	first, err := oidc.New(settings)
	if err != nil {
		t.Fatal(err)
	}
	f := flow()
	code := signIn(t, first, f)
	idp.SetKnobs(oidctest.Knobs{Delay: delay})
	p, err := oidc.New(settings)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = p.AuthURL(context.Background(), redirectURI, flow(), scopes) }() // discovers, slowly
	for idp.Calls("discovery") < 2 {                                                    // the discovery is under way
		time.Sleep(time.Millisecond)
	}
	if _, err := p.Exchange(context.Background(), redirectURI, code, f, false); err != nil {
		t.Fatalf("the wait for another use's discovery was charged to this one: %v", err)
	}
}

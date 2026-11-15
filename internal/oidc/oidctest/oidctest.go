// Package oidctest is a fake OpenID provider for tests (server#235): discovery,
// a JWKS of a generated RSA key, an authorize endpoint that approves at once, a
// token endpoint that checks the client, the redirect URI and PKCE, and
// userinfo. Knobs bend each part of the answer, so a test can show the server
// refuses it.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Person is who signs in at the provider.
type Person struct {
	Subject           string
	Email             string
	EmailVerified     any // true/false, or a string ("true") as some providers send
	PreferredUsername string
	Name              string
	Groups            []string
}

// Knobs bend the provider's answers (zero values answer correctly).
type Knobs struct {
	// TokenIssuer is the ID token's iss ("" = the issuer).
	TokenIssuer string
	// Audience is the ID token's aud (nil = [client id]); AZP its azp ("" = none).
	Audience []string
	AZP      string
	// Alg signs the ID token: "" or RS256 with the provider's key, HS256 with
	// the client secret (a symmetric signature the server must refuse), ES256
	// with a key the JWKS doesn't hold.
	Alg string
	// ExpiresIn is the ID token's life (0 = 5 minutes; negative = expired).
	ExpiresIn time.Duration
	// IssuedAt shifts iat from now; NoIssuedAt leaves it out.
	IssuedAt   time.Duration
	NoIssuedAt bool
	// Nonce replaces the flow's nonce in the ID token when set.
	Nonce string
	// UserinfoOnly leaves email, username and groups out of the ID token, so
	// only userinfo has them.
	UserinfoOnly bool
	// UserinfoSubject replaces sub in userinfo when set.
	UserinfoSubject string
	// DiscoveryIssuer is what discovery says the issuer is ("" = the issuer).
	DiscoveryIssuer string
	// FailDiscovery answers discovery 500.
	FailDiscovery bool
}

// Provider is a running fake provider.
type Provider struct {
	Server       *httptest.Server
	Issuer       string
	ClientID     string
	ClientSecret string // "" for a public client

	mu       sync.Mutex
	key      *rsa.PrivateKey
	codes    map[string]grant
	access   map[string]Person
	person   Person
	knobs    Knobs
	calls    map[string]int
	lastAuth url.Values
}

type grant struct {
	redirect, challenge, nonce string
	person                     Person
}

// New starts a provider for a confidential client (secret "" for a public one)
// that signs in p.
func New(t testing.TB, clientID, secret string, p Person) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &Provider{ClientID: clientID, ClientSecret: secret, key: key, person: p,
		codes: map[string]grant{}, access: map[string]Person{}, calls: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /jwks", f.jwks)
	mux.HandleFunc("GET /authorize", f.authorize)
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("GET /userinfo", f.userinfo)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	f.Issuer = f.Server.URL
	return f
}

// SetPerson changes who signs in next.
func (f *Provider) SetPerson(p Person) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.person = p
}

// SetKnobs replaces the knobs.
func (f *Provider) SetKnobs(k Knobs) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.knobs = k
}

// Calls is how many requests an endpoint ("token", "userinfo", "discovery") had.
func (f *Provider) Calls(endpoint string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[endpoint]
}

// LastAuthorize is the query of the last authorize request.
func (f *Provider) LastAuthorize() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth
}

func (f *Provider) count(endpoint string) {
	f.mu.Lock()
	f.calls[endpoint]++
	f.mu.Unlock()
}

func (f *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	f.count("discovery")
	f.mu.Lock()
	k := f.knobs
	f.mu.Unlock()
	if k.FailDiscovery {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	iss := f.Issuer
	if k.DiscoveryIssuer != "" {
		iss = k.DiscoveryIssuer
	}
	writeJSON(w, map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                f.Issuer + "/authorize",
		"token_endpoint":                        f.Issuer + "/token",
		"userinfo_endpoint":                     f.Issuer + "/userinfo",
		"jwks_uri":                              f.Issuer + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256", "HS256", "ES256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (f *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
}

func (f *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	f.count("authorize")
	q := r.URL.Query()
	f.mu.Lock()
	f.lastAuth = q
	p := f.person
	f.mu.Unlock()
	if q.Get("client_id") != f.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("redirect_uri") == "" {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	code := random()
	f.mu.Lock()
	f.codes[code] = grant{redirect: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), person: p}
	f.mu.Unlock()
	back, _ := url.Parse(q.Get("redirect_uri"))
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound) //nolint:gosec // G710: a test provider, sending back to the client's redirect URI as a provider does

}

func (f *Provider) token(w http.ResponseWriter, r *http.Request) {
	f.count("token")
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != f.ClientID || (f.ClientSecret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(f.ClientSecret)) != 1) {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	f.mu.Lock()
	g, ok := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code")) // one use
	k := f.knobs
	f.mu.Unlock()
	if !ok || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != g.redirect {
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	idToken, err := f.idToken(g, k)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	access := random()
	f.mu.Lock()
	f.access[access] = g.person
	f.mu.Unlock()
	writeJSON(w, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300, "id_token": idToken})
}

func (f *Provider) idToken(g grant, k Knobs) (string, error) {
	now := time.Now()
	claims := map[string]any{
		"iss": f.Issuer,
		"sub": g.person.Subject,
		"exp": now.Add(5 * time.Minute).Unix(),
	}
	if k.TokenIssuer != "" {
		claims["iss"] = k.TokenIssuer
	}
	if k.ExpiresIn != 0 {
		claims["exp"] = now.Add(k.ExpiresIn).Unix()
	}
	if !k.NoIssuedAt {
		claims["iat"] = now.Add(k.IssuedAt).Unix()
	}
	if aud := k.Audience; aud != nil {
		claims["aud"] = aud
	} else {
		claims["aud"] = f.ClientID
	}
	if k.AZP != "" {
		claims["azp"] = k.AZP
	}
	claims["nonce"] = g.nonce
	if k.Nonce != "" {
		claims["nonce"] = k.Nonce
	}
	if !k.UserinfoOnly {
		addPerson(claims, g.person)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	var sk jose.SigningKey
	switch k.Alg {
	case "", "RS256":
		sk = jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: f.key, KeyID: "k1"}}
	case "HS256":
		sk = jose.SigningKey{Algorithm: jose.HS256, Key: []byte(f.ClientSecret + strings.Repeat("x", 32))}
	default:
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return "", err
		}
		sk = jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: other, KeyID: "k1"}}
	}
	signer, err := jose.NewSigner(sk, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return jws.CompactSerialize()
}

func addPerson(claims map[string]any, p Person) {
	if p.Email != "" {
		claims["email"] = p.Email
	}
	if p.EmailVerified != nil {
		claims["email_verified"] = p.EmailVerified
	}
	if p.PreferredUsername != "" {
		claims["preferred_username"] = p.PreferredUsername
	}
	if p.Name != "" {
		claims["name"] = p.Name
	}
	if p.Groups != nil {
		claims["groups"] = p.Groups
	}
}

func (f *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	f.count("userinfo")
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	p, ok := f.access[tok]
	k := f.knobs
	f.mu.Unlock()
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	claims := map[string]any{"sub": p.Subject}
	if k.UserinfoSubject != "" {
		claims["sub"] = k.UserinfoSubject
	}
	addPerson(claims, p)
	writeJSON(w, claims)
}

func tokenError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func random() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

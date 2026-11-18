package oidc

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// The flow cookie: what /start chose (state, nonce, PKCE verifier, the
// redirect URI it sent the provider, where the sign-in returns, the player's
// intent tag) carried to
// /callback in the browser, sealed with AES-GCM under a key made at start and
// held only in memory, so no database row exists for an anonymous /start and a
// restart ends every flow under way. It is valid for FlowMaxAge after /start
// and once (Replay).

// FlowMaxAge is how long a flow may take, /start to /callback.
const FlowMaxAge = 10 * time.Minute

// Errors opening a flow cookie.
var (
	// ErrFlowInvalid: the cookie is missing, malformed, or sealed by another key
	// (another process, or tampered with).
	ErrFlowInvalid = errors.New("oidc: sign-in flow is not valid")
	// ErrFlowExpired: sealed here, but older than FlowMaxAge.
	ErrFlowExpired = errors.New("oidc: sign-in flow has expired")
)

// Flow is the cookie's content.
type Flow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	// RedirectURI is the one /start sent the provider: the code exchange must
	// name the same, whatever public_url says by the callback.
	RedirectURI string `json:"u"`
	Return      string `json:"r"`
	// Intent is the web player's own random tag for this trip (its /start's
	// ?intent=, "" for none): echoed only by the finish of the sign-in this
	// flow completes, so the player can tell its own trip's return from an
	// address typed later (audiosilo#202).
	Intent string `json:"i,omitempty"`
	// Next is the console page the sign-in started from (its /start's
	// ?next=, "" for the start page): where a console sign-in lands
	// (server#406).
	Next     string `json:"x,omitempty"`
	IssuedAt int64  `json:"t"` // Unix seconds
}

// flowAD binds a sealed cookie to its use.
var flowAD = []byte("audiosilo-oidc-flow-v1")

// Sealer seals and opens flow cookies.
type Sealer struct {
	aead cipher.AEAD
	now  func() time.Time
}

// NewSealer makes a sealer with a fresh random 32-byte key. now may be nil.
func NewSealer(now func() time.Time) (*Sealer, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Sealer{aead: aead, now: now}, nil
}

// Seal returns f sealed as a cookie value (base64url), stamped now. Its JSON
// keeps & < > as they are: escaped for HTML (\u0026, six bytes each) a deep
// link of them (Flow.Next) would grow the cookie past the 4 KB a browser keeps.
func (s *Sealer) Seal(f Flow) (string, error) {
	f.IssuedAt = s.now().Unix()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return "", err
	}
	plain := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(s.aead.Seal(nonce, nonce, plain, flowAD)), nil
}

// Open reads a cookie value Seal made: ErrFlowInvalid for anything else,
// ErrFlowExpired past FlowMaxAge (or stamped in the future).
func (s *Sealer) Open(v string) (Flow, error) {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	n := s.aead.NonceSize()
	if err != nil || len(raw) < n+s.aead.Overhead() {
		return Flow{}, ErrFlowInvalid
	}
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], flowAD)
	if err != nil {
		return Flow{}, ErrFlowInvalid
	}
	var f Flow
	if json.Unmarshal(plain, &f) != nil || f.State == "" || f.Nonce == "" || f.Verifier == "" || f.RedirectURI == "" {
		return Flow{}, ErrFlowInvalid
	}
	age := s.now().Sub(time.Unix(f.IssuedAt, 0))
	if age > FlowMaxAge || age < -time.Minute {
		return Flow{}, ErrFlowExpired
	}
	return f, nil
}

// maxReplay bounds the keys one Replay remembers.
const maxReplay = 10000

// Replay remembers the flows a callback completed (the code exchanged, the ID
// token checked), for FlowMaxAge, so one cookie signs in once even when a copy
// of it is sent again with another code. A second one, of its own, remembers
// the intent tags those flows carried (Flow.Intent), for as long: a key is a
// flow's state or a tag, and a tag is free again once it is forgotten.
type Replay struct {
	mu    sync.Mutex
	used  map[string]time.Time // state or tag -> when it may be forgotten
	swept time.Time            // the last time the forgettable were dropped
	now   func() time.Time
}

// NewReplay returns an empty Replay. now may be nil.
func NewReplay(now func() time.Time) *Replay {
	if now == nil {
		now = time.Now
	}
	return &Replay{used: map[string]time.Time{}, now: now}
}

// Use records state as used, reporting false when it already was. The states
// past their time are dropped once per FlowMaxAge (and when the memory is
// full), so completed flows don't stay. With the memory full of live states it
// refuses (false): a flood of fresh flows can't push an earlier one out to be
// used again.
func (r *Replay) Use(state string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if until, ok := r.used[state]; ok && now.Before(until) {
		return false
	}
	if len(r.used) >= maxReplay || now.Sub(r.swept) >= FlowMaxAge {
		r.swept = now
		for k, until := range r.used {
			if !now.Before(until) {
				delete(r.used, k)
			}
		}
		if len(r.used) >= maxReplay {
			return false
		}
	}
	r.used[state] = now.Add(FlowMaxAge + time.Minute)
	return true
}

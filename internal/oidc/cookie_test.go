package oidc

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSealerRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	s, err := NewSealer(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	in := Flow{State: "st", Nonce: "no", Verifier: "ve", RedirectURI: "https://a.example/cb", Return: "admin", Intent: "in"}
	v, err := s.Seal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(v, "st") && strings.Contains(v, "ve") {
		t.Fatal("the cookie shows its content")
	}
	got, err := s.Open(v)
	if err != nil || got.State != "st" || got.Nonce != "no" || got.Verifier != "ve" || got.Return != "admin" ||
		got.RedirectURI != "https://a.example/cb" || got.Intent != "in" {
		t.Fatalf("Open = %+v, %v", got, err)
	}

	// Expired after ten minutes.
	now = now.Add(FlowMaxAge + time.Second)
	if _, err := s.Open(v); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("an old cookie: %v; want ErrFlowExpired", err)
	}
}

func TestSealerRefusesTampering(t *testing.T) {
	t.Parallel()
	s, _ := NewSealer(nil)
	other, _ := NewSealer(nil)
	v, _ := s.Seal(Flow{State: "st", Nonce: "no", Verifier: "ve", RedirectURI: "https://a.example/cb", Return: "web"})
	raw, _ := base64.RawURLEncoding.DecodeString(v)
	raw[len(raw)-1] ^= 1
	for name, c := range map[string]string{
		"flipped bit":   base64.RawURLEncoding.EncodeToString(raw),
		"empty":         "",
		"not base64":    "%%%",
		"short":         "AAAA",
		"another key's": mustSeal(t, other, "https://a.example/cb"),
		"no redirect":   mustSeal(t, s, ""),
	} {
		if _, err := s.Open(c); !errors.Is(err, ErrFlowInvalid) {
			t.Errorf("%s: %v; want ErrFlowInvalid", name, err)
		}
	}
}

func mustSeal(t *testing.T, s *Sealer, redirect string) string {
	t.Helper()
	v, err := s.Seal(Flow{State: "st", Nonce: "no", Verifier: "ve", RedirectURI: redirect})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReplay(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	r := NewReplay(func() time.Time { return now })
	if !r.Use("a") {
		t.Fatal("first use refused")
	}
	if r.Use("a") {
		t.Fatal("second use allowed")
	}
	if !r.Use("b") {
		t.Fatal("another state refused")
	}
	now = now.Add(FlowMaxAge + 2*time.Minute)
	if !r.Use("a") {
		t.Fatal("a state is remembered past the cookie's life")
	}
}

func TestReplayFull(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	r := NewReplay(func() time.Time { return now })
	for i := range maxReplay {
		r.used[strings.Repeat("x", 3)+string(rune(i+0x100))] = now.Add(time.Minute)
	}
	if r.Use("fresh") {
		t.Fatal("a full memory of live states took another")
	}
	now = now.Add(2 * time.Minute)
	if !r.Use("fresh") {
		t.Fatal("expired states were not dropped to make room")
	}
}

// TestReplayForgetsCompletedFlows: states past their time are dropped as new
// ones come, not only once the memory is full.
func TestReplayForgetsCompletedFlows(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	r := NewReplay(func() time.Time { return now })
	for _, st := range []string{"a", "b", "c"} {
		r.Use(st)
	}
	now = now.Add(FlowMaxAge + 2*time.Minute)
	if !r.Use("d") {
		t.Fatal("a new state refused")
	}
	if len(r.used) != 1 {
		t.Fatalf("%d states kept, want only the new one", len(r.used))
	}
}

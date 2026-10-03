package config

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func TestDefaultIsSecure(t *testing.T) {
	c := Default("/data")
	// Secure-by-default invariants tied to "safe to expose to the internet".
	if c.TLS.Mode != TLSSelfSigned {
		t.Fatalf("default TLS mode = %q, want selfsigned (never off by default)", c.TLS.Mode)
	}
	if c.Bind == "" {
		t.Fatal("default bind must be set")
	}
	if len(c.CORSOrigins) != 0 {
		t.Fatal("CORS must be empty by default (no cross-origin grants)")
	}
	if len(c.TrustedProxies) != 0 {
		t.Fatal("no proxies trusted by default")
	}
	if len(c.Libraries) != 0 {
		t.Fatal("no libraries by default")
	}
}

func TestLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()

	cfg, firstRun, err := Load(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if !firstRun {
		t.Fatal("an empty data dir must report firstRun = true")
	}

	cfg.Bind = "127.0.0.1:9999"
	cfg.Libraries = []Library{{Name: "Books", Root: "/srv/books"}}
	cfg.ServerID = "srv-abc123" // the launcher mints this; it must persist verbatim
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, firstRun2, err := Load(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if firstRun2 {
		t.Fatal("after Save the config exists, so firstRun must be false")
	}
	if got.Bind != "127.0.0.1:9999" || len(got.Libraries) != 1 || got.Libraries[0].Name != "Books" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.ServerID != "srv-abc123" {
		t.Fatalf("server id must survive Save/Load, got %q", got.ServerID)
	}

	// Secrets-adjacent config is written owner-only.
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config perms = %v, want 0600", perm)
	}
}

func TestValidate(t *testing.T) {
	base := func() *Config { return Default(t.TempDir()) }

	if err := base().Validate(); err != nil {
		t.Fatalf("default config should validate, got %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"missing data dir", func(c *Config) { c.DataDir = "" }, true},
		{"bad bind", func(c *Config) { c.Bind = "no-port" }, true},
		{"autocert needs hosts", func(c *Config) { c.TLS = TLSConfig{Mode: TLSAutocert} }, true},
		{"autocert with hosts", func(c *Config) { c.TLS = TLSConfig{Mode: TLSAutocert, Hosts: []string{"x.example.com"}} }, false},
		{"invalid tls mode", func(c *Config) { c.TLS.Mode = "bogus" }, true},
		{"bad proxy cidr", func(c *Config) { c.TrustedProxies = []string{"not-a-cidr"} }, true},
		{"good proxy cidr", func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/8"} }, false},
		{"duplicate library names", func(c *Config) {
			c.Libraries = []Library{{Name: "A", Root: "/a"}, {Name: "A", Root: "/b"}}
		}, true},
		{"library missing root", func(c *Config) { c.Libraries = []Library{{Name: "A"}} }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

// TestDemoEffectiveMaxUsers pins the safe-by-default semantics: unset falls back
// to a bounded cap (not unlimited), while an explicit 0 opts into unlimited.
func TestDemoEffectiveMaxUsers(t *testing.T) {
	if got := (DemoConfig{}).EffectiveMaxUsers(); got != DefaultDemoMaxUsers {
		t.Fatalf("unset max_users = %d, want default %d", got, DefaultDemoMaxUsers)
	}
	zero := 0
	if got := (DemoConfig{MaxUsers: &zero}).EffectiveMaxUsers(); got != 0 {
		t.Fatalf("explicit 0 max_users = %d, want 0 (unlimited)", got)
	}
	fifty := 50
	if got := (DemoConfig{MaxUsers: &fifty}).EffectiveMaxUsers(); got != 50 {
		t.Fatalf("explicit max_users = %d, want 50", got)
	}
}

// TestIdleTTLDuration pins the safe fallback: an empty, unparseable or
// non-positive idle_ttl resolves to the 24h default, while a valid positive
// duration is honored.
func TestIdleTTLDuration(t *testing.T) {
	const fallback = 24 * time.Hour
	cases := []struct {
		name string
		ttl  string
		want time.Duration
	}{
		{"empty falls back", "", fallback},
		{"unparseable falls back", "not-a-duration", fallback},
		{"negative falls back", "-5h", fallback},
		{"zero falls back", "0s", fallback},
		{"valid positive honored", "1h", time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DemoConfig{IdleTTL: tc.ttl}.IdleTTLDuration()
			if got != tc.want {
				t.Fatalf("IdleTTLDuration(%q) = %v, want %v", tc.ttl, got, tc.want)
			}
		})
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv("AUDIOSILO_BIND", "0.0.0.0:9000")
	t.Setenv("AUDIOSILO_TLS_MODE", "off")
	t.Setenv("AUDIOSILO_CORS_ORIGINS", "https://a.com, https://b.com")
	t.Setenv("AUDIOSILO_TRUSTED_PROXIES", "10.0.0.0/8")
	t.Setenv("AUDIOSILO_DEMO_ENABLED", "true")
	t.Setenv("AUDIOSILO_DEMO_MAX_USERS", "42")

	c := Default(t.TempDir())
	applyEnv(c)

	if !c.Demo.Enabled {
		t.Fatal("demo enabled override not applied")
	}
	if c.Demo.MaxUsers == nil || *c.Demo.MaxUsers != 42 {
		t.Fatalf("demo max_users = %v, want 42", c.Demo.MaxUsers)
	}

	if c.Bind != "0.0.0.0:9000" {
		t.Fatalf("bind = %q", c.Bind)
	}
	if c.TLS.Mode != TLSOff {
		t.Fatalf("tls mode = %q", c.TLS.Mode)
	}
	if !reflect.DeepEqual(c.CORSOrigins, []string{"https://a.com", "https://b.com"}) {
		t.Fatalf("cors origins = %v (splitList should trim whitespace)", c.CORSOrigins)
	}
	if !reflect.DeepEqual(c.TrustedProxies, []string{"10.0.0.0/8"}) {
		t.Fatalf("trusted proxies = %v", c.TrustedProxies)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a , ,b,c ")
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("splitList dropped/kept the wrong items: %v", got)
	}
}

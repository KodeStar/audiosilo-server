package config

import (
	"math"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The rate limits default to the values the API always had (the token's new),
// are read from config.yaml or their variables, have no console setting, and
// refuse a rate or burst that would never let a request through.
func TestRateLimitConfig(t *testing.T) {
	def := Default("/data").RateLimit
	if def != (RateLimitConfig{}) {
		t.Fatalf("Default sets %+v, want nothing (the defaults apply at use)", def)
	}
	if eff := def.Effective(); eff.Address != (RateBucket{Rate: 50, Burst: 200}) ||
		eff.Media != (RateBucket{Rate: 200, Burst: 2000}) || eff.Token != DefaultTokenRateLimit {
		t.Fatalf("effective defaults = %+v", eff)
	}
	dir := t.TempDir()
	yaml := "rate_limit:\n  address:\n    rate: 10\n    burst: 20\n  token:\n    rate: 0.5\n    burst: 3\n  media:\n    burst: 7\n"
	if err := os.WriteFile(Path(dir), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.RateLimit.Effective()
	if got.Address != (RateBucket{10, 20}) || got.Token != (RateBucket{0.5, 3}) ||
		got.Media != (RateBucket{DefaultMediaRateLimit.Rate, 7}) {
		t.Fatalf("from config.yaml = %+v", got)
	}
	t.Setenv("AUDIOSILO_RATE_LIMIT_TOKEN_RATE", "7500.5")
	t.Setenv("AUDIOSILO_RATE_LIMIT_MEDIA_RATE", "99")
	cfg, _, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RateLimit.Effective(); got.Token != (RateBucket{7500.5, 3}) || got.Media != (RateBucket{99, 7}) ||
		got.Address != (RateBucket{10, 20}) {
		t.Fatalf("with the environment = %+v", got)
	}
	for section, values := range cfg.Settings() {
		for name := range values {
			if strings.Contains(section+"."+name, "rate") {
				t.Fatalf("console setting %s.%s", section, name)
			}
		}
	}

	for _, bad := range []func(*Config){
		func(c *Config) { c.RateLimit.Token.Rate = -1 },
		func(c *Config) { c.RateLimit.Media.Rate = math.NaN() },
		func(c *Config) { c.RateLimit.Token.Rate = math.Inf(1) },
		func(c *Config) { c.RateLimit.Address.Burst = -5 },
	} {
		c := Default(t.TempDir())
		bad(c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "rate_limit.") {
			t.Fatalf("Validate(%+v) = %v, want a rate_limit error", c.RateLimit, err)
		}
	}
}

// config.yaml keeps only the rate limits an admin set: a default config saved
// and loaded again follows a changed default, and set values (one of a pair
// included) come back as written. A value from the environment is not saved.
func TestRateLimitSavedOnlyWhenSet(t *testing.T) {
	old := DefaultTokenRateLimit
	t.Cleanup(func() { DefaultTokenRateLimit = old })

	dir := t.TempDir()
	if err := Default(dir).Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "rate_limit") {
		t.Fatalf("a default config saved rate_limit:\n%s", raw)
	}
	DefaultTokenRateLimit = RateBucket{Rate: 123, Burst: 456}
	cfg, _, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RateLimit.Effective().Token; got != DefaultTokenRateLimit {
		t.Fatalf("after the default changed: token = %+v, want %+v", got, DefaultTokenRateLimit)
	}

	cfg.RateLimit.Address = RateBucket{Rate: 5, Burst: 9}
	cfg.RateLimit.Media.Burst = 40
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if cfg, _, err = Load(dir); err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimit != (RateLimitConfig{Address: RateBucket{5, 9}, Media: RateBucket{Burst: 40}}) {
		t.Fatalf("round trip = %+v", cfg.RateLimit)
	}
	if got := cfg.RateLimit.Effective(); got.Media != (RateBucket{DefaultMediaRateLimit.Rate, 40}) || got.Token != DefaultTokenRateLimit {
		t.Fatalf("effective after the round trip = %+v", got)
	}

	t.Setenv("AUDIOSILO_RATE_LIMIT_TOKEN_BURST", "77")
	if cfg, _, err = Load(dir); err != nil {
		t.Fatal(err)
	}
	if got := cfg.RateLimit.Effective().Token; got != (RateBucket{DefaultTokenRateLimit.Rate, 77}) {
		t.Fatalf("token with the environment = %+v", got)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if raw, err = os.ReadFile(Path(dir)); err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := yaml.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.RateLimit != (RateLimitConfig{Address: RateBucket{5, 9}, Media: RateBucket{Burst: 40}}) {
		t.Fatalf("saved with the environment set = %+v, want only the admin's values", saved.RateLimit)
	}
}

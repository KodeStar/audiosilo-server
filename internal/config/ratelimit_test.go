package config

import (
	"math"
	"os"
	"strings"
	"testing"
)

// The rate limits default to the values the API always had (the token's new),
// are read from config.yaml or their variables, have no console setting, and
// refuse a rate or burst that would never let a request through.
func TestRateLimitConfig(t *testing.T) {
	def := Default("/data").RateLimit
	if def.Address != (RateBucket{Rate: 50, Burst: 200}) || def.Media != (RateBucket{Rate: 200, Burst: 2000}) ||
		def.Token != DefaultTokenRateLimit {
		t.Fatalf("defaults = %+v", def)
	}
	dir := t.TempDir()
	yaml := "rate_limit:\n  address:\n    rate: 10\n    burst: 20\n  token:\n    rate: 0.5\n    burst: 3\n"
	if err := os.WriteFile(Path(dir), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RateLimit; got.Address != (RateBucket{10, 20}) || got.Token != (RateBucket{0.5, 3}) || got.Media != DefaultMediaRateLimit {
		t.Fatalf("from config.yaml = %+v", got)
	}
	t.Setenv("AUDIOSILO_RATE_LIMIT_TOKEN_RATE", "7500.5")
	t.Setenv("AUDIOSILO_RATE_LIMIT_MEDIA_BURST", "99")
	cfg, _, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimit.Token.Rate != 7500.5 || cfg.RateLimit.Media.Burst != 99 || cfg.RateLimit.Address.Rate != 10 {
		t.Fatalf("with the environment = %+v", cfg.RateLimit)
	}
	for section, values := range cfg.Settings() {
		for name := range values {
			if strings.Contains(section+"."+name, "rate") {
				t.Fatalf("console setting %s.%s", section, name)
			}
		}
	}

	for _, bad := range []func(*Config){
		func(c *Config) { c.RateLimit.Address.Rate = 0 },
		func(c *Config) { c.RateLimit.Token.Rate = -1 },
		func(c *Config) { c.RateLimit.Media.Rate = math.NaN() },
		func(c *Config) { c.RateLimit.Token.Rate = math.Inf(1) },
		func(c *Config) { c.RateLimit.Token.Burst = 0 },
		func(c *Config) { c.RateLimit.Address.Burst = -5 },
	} {
		c := Default(t.TempDir())
		bad(c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "rate_limit.") {
			t.Fatalf("Validate(%+v) = %v, want a rate_limit error", c.RateLimit, err)
		}
	}
}

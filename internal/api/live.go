package api

import (
	"net"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// liveConfig is the current config plus what the middleware derives from it,
// computed once per settings save instead of once per request. It is never
// changed after it's built: a save builds a new one and swaps it in.
type liveConfig struct {
	*config.Config
	proxies    []*net.IPNet    // trusted_proxies, parsed
	corsAll    bool            // cors_origins holds "*"
	corsAllows map[string]bool // cors_origins
}

func newLiveConfig(c *config.Config) *liveConfig {
	l := &liveConfig{Config: c, corsAllows: map[string]bool{}}
	for _, p := range c.TrustedProxies {
		if _, n, err := net.ParseCIDR(p); err == nil {
			l.proxies = append(l.proxies, n)
		}
	}
	for _, o := range c.CORSOrigins {
		if o == "*" {
			l.corsAll = true
		}
		l.corsAllows[o] = true
	}
	return l
}

// allowsOrigin reports whether CORS grants this browser origin.
func (l *liveConfig) allowsOrigin(origin string) bool { return l.corsAll || l.corsAllows[origin] }

// config is the current config. Restart-only settings read a.boot instead.
func (a *API) config() *liveConfig { return a.live.Load() }

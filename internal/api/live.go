package api

import (
	"net"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// liveConfig is the config the server works with now: the saved settings, with
// each restart setting (config's settings table says which) still as the server
// started, plus what the middleware derives from it, computed once per settings
// save instead of once per request. It is never changed after it's built: a
// save builds a new one and swaps it in.
type liveConfig struct {
	*config.Config
	// saved is what config.yaml holds: Server > Settings shows it, and its
	// restart settings may differ from the embedded (running) ones.
	saved      *config.Config
	proxies    []*net.IPNet    // trusted_proxies, parsed
	corsAll    bool            // cors_origins holds "*"
	corsAllows map[string]bool // cors_origins
}

// newLiveConfig builds the live config for saved settings on a server that
// started with running.
func newLiveConfig(saved, running *config.Config) *liveConfig {
	eff := running
	if saved != running {
		eff = saved.Effective(running)
	}
	l := &liveConfig{Config: eff, saved: saved, corsAllows: map[string]bool{}}
	for _, p := range eff.TrustedProxies {
		if _, n, err := net.ParseCIDR(p); err == nil {
			l.proxies = append(l.proxies, n)
		}
	}
	for _, o := range eff.CORSOrigins {
		if o == "*" {
			l.corsAll = true
		}
		l.corsAllows[o] = true
	}
	return l
}

// allowsOrigin reports whether CORS grants this browser origin.
func (l *liveConfig) allowsOrigin(origin string) bool { return l.corsAll || l.corsAllows[origin] }

// config is the config the server works with now (see liveConfig).
func (a *API) config() *liveConfig { return a.live.Load() }

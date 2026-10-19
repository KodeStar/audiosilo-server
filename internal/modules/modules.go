// Package modules is the server's managed modules framework (BRIDGE-PLAN.md
// Phase 2a): optional features that ship off, that an admin turns on in the
// console (Settings > Modules) without a restart, and that may need a Bridge
// feature to be usable. A module is a value of type Module: its id, the Bridge
// feature it needs, its routes and its background work. The API mounts the
// routes behind the module's toggle, starts and stops the background work as
// the toggle changes, and advertises the id as a capability flag on GET /server
// while the module is on and its Bridge requirement is met.
//
// The registry itself lives in modules/builtin, so a module's own package
// (modules/<id>) can import this one without a cycle.
package modules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Module is one managed module.
type Module struct {
	// ID names the module everywhere: its capability flag on GET /server, its
	// config key modules.<id>.enabled (and setting modules.<id>), its routes'
	// module_off gate and the console's copy (settings.module.<id>.*).
	ID string
	// RequiresBridge is a Bridge feature (GET /server bridge.features) the module
	// needs to be usable; "" for none. The capability flag is false until an
	// online Bridge offers it; the module's routes still answer (each action
	// needing the Bridge refuses on its own), so they don't flap with heartbeats.
	RequiresBridge string
	// Routes registers the module's HTTP routes, each answering 404 module_off
	// while the module is off; nil for none. Called once, by the API's Handler.
	Routes func(r Registrar)
	// Start runs the module's background work while it is on, returning when
	// ctx is cancelled (the module turned off, or the server stopping); nil for
	// none.
	Start func(ctx context.Context, d Deps)
}

// Registrar mounts a module's routes. A pattern is a full ServeMux pattern
// with a method ("GET /api/v1/..."); the handler only ever runs while the
// module is on.
type Registrar interface {
	// Admin mounts an admin-only route.
	Admin(pattern string, h http.Handler)
	// User mounts a route for a signed-in user (a session or an API key),
	// never a kid's account or a demo account (403).
	User(pattern string, h http.Handler)
}

// Deps are what a module's background work gets from the server.
type Deps struct {
	Log     *slog.Logger // tagged with the module's id
	Catalog *catalog.Catalog
	Bridge  *bridge.Service
}

// Where a module stands with the Bridge feature it needs (Module.BridgeState,
// GET /admin/modules bridge_state).
const (
	BridgeOffline   = "offline"    // no paired Bridge heard from within bridge.OnlineWindow
	BridgeNoFeature = "no_feature" // the online Bridge doesn't offer the feature
	BridgeReady     = "ready"      // the online Bridge offers it
)

// BridgeState is where m stands with the Bridge feature it needs, given
// whether a Bridge is online and the features it offers; "" when m needs none.
func (m Module) BridgeState(online bool, features []string) string {
	switch {
	case m.RequiresBridge == "":
		return ""
	case !online:
		return BridgeOffline
	case !slices.Contains(features, m.RequiresBridge):
		return BridgeNoFeature
	}
	return BridgeReady
}

// Active is a module's capability flag: on (its toggle), and its Bridge
// requirement met (bridgeState, from BridgeState: "" or BridgeReady).
func Active(on bool, bridgeState string) bool {
	return on && (bridgeState == "" || bridgeState == BridgeReady)
}

// idPattern is a module id: a capability flag and a config key segment.
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Validate checks a registry: every id well formed and once only, none taken
// by a core capability flag (core), and each with its config row
// (hasConfig: config.HasModule). The launcher refuses to start on an error.
func Validate(ms []Module, hasConfig func(id string) bool, core []string) error {
	var errs []error
	seen := map[string]bool{}
	for _, m := range ms {
		switch {
		case !idPattern.MatchString(m.ID):
			errs = append(errs, fmt.Errorf("module %q: an id is a lower-case letter then up to 31 letters, digits or underscores", m.ID))
		case seen[m.ID]:
			errs = append(errs, fmt.Errorf("module %q: registered twice", m.ID))
		case slices.Contains(core, m.ID):
			errs = append(errs, fmt.Errorf("module %q: a core capability has that name", m.ID))
		case hasConfig == nil || !hasConfig(m.ID):
			errs = append(errs, fmt.Errorf("module %q: no config row modules.%s.enabled", m.ID, m.ID))
		}
		seen[m.ID] = true
	}
	return errors.Join(errs...)
}

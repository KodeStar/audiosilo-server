package api

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/kodestar/audiosilo-server/internal/modules"
)

// Managed modules (internal/modules, BRIDGE-PLAN.md Phase 2a): optional
// features an admin turns on in Settings > Modules. Each module's toggle is a
// console setting (modules.<id>), so turning one on or off is a settings save:
// the live config swaps, then syncModules starts or stops its background work.
// Its routes are always mounted and answer 404 module_off while it is off
// (requireModule), so nothing waits for a restart. Its capability flag on GET
// /server is true while it is on AND its Bridge requirement is met
// (moduleActive); the routes gate on the toggle only, so they don't flap with
// the Bridge's heartbeat (an action that needs the Bridge refuses on its own).

// Where a module stands with the Bridge it needs (GET /admin/modules
// bridge_state).
const (
	bridgeStateOffline   = "offline"    // no paired Bridge heard from within bridge.OnlineWindow
	bridgeStateNoFeature = "no_feature" // the online Bridge doesn't offer the feature
	bridgeStateReady     = "ready"      // the online Bridge offers it
)

// SetModules gives the API its module registry (the launcher passes
// builtin.Modules(); tests a fake), refusing one modules.Validate refuses. Call
// before Handler() and StartModules.
func (a *API) SetModules(ms []modules.Module) error {
	core := slices.Sorted(maps.Keys(a.coreCapabilities(nil)))
	if err := modules.Validate(ms, a.boot.HasModule, core); err != nil {
		return err
	}
	a.modules = slices.Clone(ms)
	return nil
}

// moduleOn reports whether module id is turned on in the live config.
func (a *API) moduleOn(id string) bool { return a.config().ModuleEnabled(id) }

// moduleActive is module m's capability flag: on, and the Bridge feature it
// needs (if any) offered by the online Bridge (bridgeOn, nil when none).
func (a *API) moduleActive(m modules.Module, bridgeOn *bridgeInfo) bool {
	return a.moduleOn(m.ID) && (m.RequiresBridge == "" || moduleBridgeState(m, bridgeOn) == bridgeStateReady)
}

// moduleBridgeState is where m stands with the Bridge feature it needs; "" when
// it needs none.
func moduleBridgeState(m modules.Module, bridgeOn *bridgeInfo) string {
	switch {
	case m.RequiresBridge == "":
		return ""
	case bridgeOn == nil:
		return bridgeStateOffline
	case !slices.Contains(bridgeOn.Features, m.RequiresBridge):
		return bridgeStateNoFeature
	}
	return bridgeStateReady
}

// requireModule answers 404 module_off while module id is off, else runs next.
func (a *API) requireModule(id string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.moduleOn(id) {
			writeErrorCode(w, http.StatusNotFound, codeModuleOff, "this module is turned off")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// moduleRoutes is the Registrar a module's Routes mounts its routes through:
// each behind its auth, then the module's toggle.
type moduleRoutes struct {
	a   *API
	mux *http.ServeMux
	id  string
}

func (r moduleRoutes) Admin(pattern string, h http.Handler) {
	r.mux.Handle(checkModulePattern(r.id, pattern), r.a.requireAdmin(r.a.requireModule(r.id, h)))
}

func (r moduleRoutes) User(pattern string, h http.Handler) {
	r.mux.Handle(checkModulePattern(r.id, pattern), r.a.requireAuth(r.a.requireModule(r.id, h)))
}

// checkModulePattern returns pattern when it names a method and an API path
// ("GET /api/v1/..."), and panics otherwise: a module's routes live under the
// API, where the global middleware and the auth wrappers apply. A programming
// error, found the first time the server starts.
func checkModulePattern(id, pattern string) string {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || method == "" || strings.ToUpper(method) != method || !strings.HasPrefix(path, "/api/v1/") {
		panic("module " + id + ": route pattern " + pattern + " is not \"METHOD /api/v1/...\"")
	}
	return pattern
}

// mountModules registers every module's routes on mux (Handler).
func (a *API) mountModules(mux *http.ServeMux) {
	for _, m := range a.modules {
		if m.Routes != nil {
			m.Routes(moduleRoutes{a: a, mux: mux, id: m.ID})
		}
	}
}

// moduleRunner runs each module's background work (Module.Start) while the
// module is on.
type moduleRunner struct {
	mu   sync.Mutex
	ctx  context.Context // StartModules' context; nil until then
	runs map[string]*moduleRun
}

// moduleRun is one run of a module's Start.
type moduleRun struct {
	cancel  context.CancelFunc
	done    chan struct{} // closed when Start has returned
	stopped bool          // cancelled by syncModules (the module turned off)
}

// StartModules starts the background work of each module that is on, for
// ctx's lifetime (the launcher calls it once, beside StartChapterChecks); from
// then on a settings save starts and stops it (syncModules).
func (a *API) StartModules(ctx context.Context) {
	r := &a.moduleRuns
	r.mu.Lock()
	r.ctx = ctx
	r.runs = map[string]*moduleRun{}
	r.mu.Unlock()
	a.syncModules()
}

// syncModules brings each module's background work in line with the live
// config: it starts a module turned on and cancels one turned off. It does not
// wait for a stopping module (the settings save that called it answers at
// once); a module turned back on meanwhile starts once the old run has
// returned, so two runs of one module never overlap. Nothing before
// StartModules.
func (a *API) syncModules() {
	r := &a.moduleRuns
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil {
		return
	}
	for _, m := range a.modules {
		if m.Start == nil {
			continue
		}
		cur := r.runs[m.ID]
		switch on := a.moduleOn(m.ID); {
		case on && (cur == nil || cur.stopped):
			r.runs[m.ID] = a.runModule(r.ctx, m, cur)
		case !on && cur != nil && !cur.stopped:
			cur.stopped = true
			cur.cancel()
		}
	}
}

// runModule starts a run of m.Start under parent, after prev (the module's
// last run, or nil) has returned.
func (a *API) runModule(parent context.Context, m modules.Module, prev *moduleRun) *moduleRun {
	ctx, cancel := context.WithCancel(parent)
	run := &moduleRun{cancel: cancel, done: make(chan struct{})}
	log := a.log.With("module", m.ID)
	go func() {
		defer close(run.done)
		defer cancel()
		if prev != nil {
			select {
			case <-prev.done:
			case <-ctx.Done():
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		// A module's failure is its own: it stops that module, never the server.
		defer func() {
			if p := recover(); p != nil {
				log.Error("module stopped by a panic", "panic", p)
			}
		}()
		log.Info("module started")
		m.Start(ctx, modules.Deps{Log: log, Catalog: a.cat, Bridge: a.bridge})
		log.Info("module stopped")
	}()
	return run
}

// adminModule is one module in GET /admin/modules.
type adminModule struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"` // the live toggle
	Active  bool   `json:"active"`  // the capability flag: on and its Bridge requirement met
	// RequiresBridge and BridgeState are omitted for a module that needs no Bridge.
	RequiresBridge string `json:"requires_bridge,omitempty"`
	BridgeState    string `json:"bridge_state,omitempty"` // offline | no_feature | ready
}

// handleAdminModules serves GET /admin/modules (admin only): every module in
// registry order, {"modules": [...]} ([] for none). Whether a toggle is locked
// is the settings envelope's locked["modules.<id>"], not repeated here.
func (a *API) handleAdminModules(w http.ResponseWriter, r *http.Request) {
	bridgeOn := a.bridgeOnline(r.Context())
	out := make([]adminModule, 0, len(a.modules))
	for _, m := range a.modules {
		out = append(out, adminModule{
			ID:             m.ID,
			Enabled:        a.moduleOn(m.ID),
			Active:         a.moduleActive(m, bridgeOn),
			RequiresBridge: m.RequiresBridge,
			BridgeState:    moduleBridgeState(m, bridgeOn),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"modules": out})
}

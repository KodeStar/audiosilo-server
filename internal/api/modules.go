package api

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"

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

// SetModules gives the API its module registry (the launcher passes
// builtin.Modules(); tests a fake), refusing one modules.Validate refuses. Call
// before Handler() and StartModules.
func (a *API) SetModules(ms []modules.Module) error {
	core := slices.Collect(maps.Keys(a.coreCapabilities(nil)))
	if err := modules.Validate(ms, a.boot.HasModule, core); err != nil {
		return err
	}
	a.modules = slices.Clone(ms)
	return nil
}

// moduleOn reports whether module id is turned on in the live config.
func (a *API) moduleOn(id string) bool { return a.config().ModuleEnabled(id) }

// moduleBridgeState is where m stands with the Bridge feature it needs, given
// the online Bridge (bridgeOn, nil when none); "" when it needs none.
func moduleBridgeState(m modules.Module, bridgeOn *bridgeInfo) string {
	if bridgeOn == nil {
		return m.BridgeState(false, nil)
	}
	return m.BridgeState(true, bridgeOn.Features)
}

// moduleActive is module m's capability flag: on, and the Bridge feature it
// needs (if any) offered by the online Bridge (bridgeOn, nil when none).
func (a *API) moduleActive(m modules.Module, bridgeOn *bridgeInfo) bool {
	return modules.Active(a.moduleOn(m.ID), moduleBridgeState(m, bridgeOn))
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

// User mounts a route for a signed-in listener: never a kid's account (403
// kid_account) or a demo account (403 demo_account), since a module's actions
// (contributing to the community, first of all) are a grown-up's own.
func (r moduleRoutes) User(pattern string, h http.Handler) {
	r.mux.Handle(checkModulePattern(r.id, pattern),
		r.a.requireAuth(notKid(r.a.notDemo(r.a.requireModule(r.id, h)))))
}

// notDemo answers 403 demo_account for a demo account, else runs next. It reads
// the account (nonDemoAccount): the token's lookup doesn't load is_demo, so the
// request's user can't say.
func (a *API) notDemo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.nonDemoAccount(w, r) == nil {
			return
		}
		next.ServeHTTP(w, r)
	})
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

// StartModules starts the background work of each module that is on, for
// ctx's lifetime (the launcher calls it once, beside StartChapterChecks); from
// then on a settings save starts and stops it (syncModules).
func (a *API) StartModules(ctx context.Context) {
	a.moduleRuns.Start(ctx, a.modules, modules.Deps{Log: a.log, Catalog: a.cat, Bridge: a.bridge}, a.moduleOn)
}

// StopModules ends every module's background work and waits for it to return
// (the launcher defers it after StartModules, so it runs before the database
// closes); a settings save starts nothing after it.
func (a *API) StopModules() { a.moduleRuns.Stop() }

// syncModules brings each module's background work in line with the live
// config (modules.Runner.Sync: it never waits for a stopping module).
func (a *API) syncModules() { a.moduleRuns.Sync(a.moduleOn) }

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
		on, state := a.moduleOn(m.ID), moduleBridgeState(m, bridgeOn)
		out = append(out, adminModule{
			ID:             m.ID,
			Enabled:        on,
			Active:         modules.Active(on, state),
			RequiresBridge: m.RequiresBridge,
			BridgeState:    state,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"modules": out})
}

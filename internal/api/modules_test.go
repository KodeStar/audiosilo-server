package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/modules"
)

// Bridge Phase 2a: the managed modules framework. The fakes reuse the id
// "contributions", the one module with a config row (its toggle), with the
// requirement, routes and work each test needs.

// withModules gives e's API the registry ms and serves a fresh Handler, so the
// fakes' routes are mounted. Call before pairing a Bridge through e.
func withModules(t *testing.T, e *testEnv, ms ...modules.Module) {
	t.Helper()
	if err := e.api.SetModules(ms); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e.api.Handler())
	t.Cleanup(srv.Close)
	e.srv = srv
}

func adminSession(t *testing.T, e *testEnv) string {
	t.Helper()
	tok, err := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "console", 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// setModule turns contributions on or off through the console's settings save.
func setModule(t *testing.T, e *testEnv, adminTok string, on bool) {
	t.Helper()
	body := `{"modules":{"contributions":false}}`
	if on {
		body = `{"modules":{"contributions":true}}`
	}
	if resp, out := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("toggle %t: %d %s", on, resp.StatusCode, out)
	}
}

// capability reads GET /server's contributions flag, failing when it is absent
// (a module's flag is false while off, never missing).
func capability(t *testing.T, e *testEnv) bool {
	t.Helper()
	on, ok := serverCapabilities(t, e)["contributions"]
	if !ok {
		t.Fatal("no contributions capability on GET /server")
	}
	return on
}

func adminModules(t *testing.T, e *testEnv, tok string) string {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/admin/modules", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/modules = %d %s", resp.StatusCode, body)
	}
	return strings.TrimSpace(body)
}

// The capability flag is the toggle AND the module's Bridge requirement: off;
// on without a requirement; on needing contribute with no Bridge, a Bridge
// without the feature, then one with it; and back off.
func TestModuleCapabilityMatrix(t *testing.T) {
	t.Parallel()

	// No Bridge requirement: the toggle alone.
	e := newTestEnv(t)
	withModules(t, e, modules.Module{ID: "contributions"})
	adminTok := adminSession(t, e)
	if capability(t, e) {
		t.Fatal("a module is on by default")
	}
	setModule(t, e, adminTok, true)
	if !capability(t, e) {
		t.Fatal("a module needing no Bridge is off while on")
	}
	if got := adminModules(t, e, adminTok); got != `{"modules":[{"id":"contributions","enabled":true,"active":true}]}` {
		t.Fatalf("no requirement: %s", got)
	}

	// The shipped registry: contributions needs a Bridge that can contribute.
	b := newBridgeEnv(t)
	setModule(t, b.testEnv, b.adminTok, true)
	b.beat(t, "bridge.verify") // no estimates: no contribute feature
	if capability(t, b.testEnv) {
		t.Fatal("on, Bridge without the feature: flag true")
	}
	want := `{"modules":[{"id":"contributions","enabled":true,"active":false,"requires_bridge":"contribute","bridge_state":"no_feature"},{"id":"podcasts","enabled":false,"active":false,"requires_bridge":"podcast","bridge_state":"no_feature"}]}`
	if got := adminModules(t, b.testEnv, b.adminTok); got != want {
		t.Fatalf("no feature:\n got %s\nwant %s", got, want)
	}
	b.beatWith(t, true, true, `{"sidecars":{"can_contribute":true}}`)
	if !capability(t, b.testEnv) {
		t.Fatal("on, Bridge with the feature: flag false")
	}
	_, body := b.do(t, "GET", "/api/v1/server", b.userTok, "")
	if !strings.Contains(body, `"features":["bridge","contribute","sidecars"]`) {
		t.Fatalf("bridge.features without contribute: %s", body)
	}
	want = `{"modules":[{"id":"contributions","enabled":true,"active":true,"requires_bridge":"contribute","bridge_state":"ready"},{"id":"podcasts","enabled":false,"active":false,"requires_bridge":"podcast","bridge_state":"no_feature"}]}`
	if got := adminModules(t, b.testEnv, b.adminTok); got != want {
		t.Fatalf("ready:\n got %s\nwant %s", got, want)
	}
	// Unpaired: offline, and the flag drops at once.
	if resp, out := b.do(t, "DELETE", "/api/v1/admin/bridge", b.adminTok, ""); resp.StatusCode >= 300 {
		t.Fatalf("unpair: %d %s", resp.StatusCode, out)
	}
	if capability(t, b.testEnv) {
		t.Fatal("on, Bridge unpaired: flag true")
	}
	want = `{"modules":[{"id":"contributions","enabled":true,"active":false,"requires_bridge":"contribute","bridge_state":"offline"},{"id":"podcasts","enabled":false,"active":false,"requires_bridge":"podcast","bridge_state":"offline"}]}`
	if got := adminModules(t, b.testEnv, b.adminTok); got != want {
		t.Fatalf("offline:\n got %s\nwant %s", got, want)
	}

	// Off with the requirement met: false.
	c := newBridgeEnv(t)
	c.beatWith(t, true, true, `{"sidecars":{"can_contribute":true}}`)
	if capability(t, c.testEnv) {
		t.Fatal("off, Bridge ready: flag true")
	}
	want = `{"modules":[{"id":"contributions","enabled":false,"active":false,"requires_bridge":"contribute","bridge_state":"ready"},{"id":"podcasts","enabled":false,"active":false,"requires_bridge":"podcast","bridge_state":"no_feature"}]}`
	if got := adminModules(t, c.testEnv, c.adminTok); got != want {
		t.Fatalf("off and ready:\n got %s\nwant %s", got, want)
	}
}

// The shipped registry with no Bridge paired: the plan's JSON exactly, and the
// settings envelope's modules section.
func TestAdminModulesDefault(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok := adminSession(t, e)
	want := `{"modules":[{"id":"contributions","enabled":false,"active":false,"requires_bridge":"contribute","bridge_state":"offline"},{"id":"podcasts","enabled":false,"active":false,"requires_bridge":"podcast","bridge_state":"offline"}]}`
	if got := adminModules(t, e, adminTok); got != want {
		t.Fatalf("GET /admin/modules:\n got %s\nwant %s", got, want)
	}
	_, body := e.do(t, "GET", "/api/v1/admin/settings", adminTok, "")
	var env map[string]json.RawMessage
	decodeInto(t, body, &env)
	if string(env["modules"]) != `{"contributions":false,"podcasts":false}` {
		t.Fatalf("settings modules = %s", env["modules"])
	}
	if capability(t, e) {
		t.Fatal("contributions on by default")
	}

	// An empty registry lists [] (never null), and no module flag.
	empty := newTestEnv(t)
	withModules(t, empty)
	if got := adminModules(t, empty, adminSession(t, empty)); got != `{"modules":[]}` {
		t.Fatalf("empty registry: %s", got)
	}
	if _, body := empty.do(t, "GET", "/api/v1/server", "", ""); strings.Contains(body, `"contributions"`) {
		t.Fatalf("an unregistered module's flag: %s", body)
	}
}

// Security: GET /admin/modules is the admin's. Allowed: an admin's session and
// API key. Denied: no token, a listener's session, a bridge token.
func TestAdminModulesIsAdminOnly(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	for name, tok := range map[string]string{"admin session": b.adminTok, "admin api key": b.adminKey} {
		if resp, body := b.do(t, "GET", "/api/v1/admin/modules", tok, ""); resp.StatusCode != http.StatusOK ||
			!strings.Contains(body, `"id":"contributions"`) {
			t.Errorf("%s: %d %s, want 200 with the module", name, resp.StatusCode, body)
		}
	}
	for name, c := range map[string]struct {
		tok  string
		want int
	}{
		"no token": {"", http.StatusUnauthorized},
		"listener": {b.userTok, http.StatusForbidden},
		"bridge":   {b.bridgeTok, http.StatusUnauthorized},
	} {
		resp, body := b.do(t, "GET", "/api/v1/admin/modules", c.tok, "")
		if resp.StatusCode != c.want || strings.Contains(body, "contributions") {
			t.Errorf("%s: %d %s, want %d and no modules", name, resp.StatusCode, body, c.want)
		}
	}
}

// A module's routes answer 404 module_off while it is off, and serve the
// moment it is turned on, with no restart (the same Handler); their auth comes
// first.
func TestRequireModule(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	e := newTestEnv(t)
	withModules(t, e, modules.Module{ID: "contributions", RequiresBridge: "contribute", Routes: func(r modules.Registrar) {
		r.Admin("GET /api/v1/admin/modtest", ok)
		r.User("POST /api/v1/modtest", ok)
	}})
	adminTok := adminSession(t, e)
	_, userTok := newUserWithSession(t, e, "sam", false)

	off := func(when string) {
		t.Helper()
		for _, r := range []struct{ method, path, tok string }{
			{"GET", "/api/v1/admin/modtest", adminTok},
			{"POST", "/api/v1/modtest", userTok},
			{"POST", "/api/v1/modtest", adminTok},
		} {
			resp, body := e.do(t, r.method, r.path, r.tok, "")
			if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, `"code":"module_off"`) {
				t.Errorf("%s: %s %s = %d %s, want 404 module_off", when, r.method, r.path, resp.StatusCode, body)
			}
		}
	}
	authFirst := func(when string) {
		t.Helper()
		if resp, _ := e.do(t, "GET", "/api/v1/admin/modtest", userTok, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: the admin route as a listener = %d, want 403", when, resp.StatusCode)
		}
		for _, p := range []struct{ method, path string }{{"GET", "/api/v1/admin/modtest"}, {"POST", "/api/v1/modtest"}} {
			if resp, _ := e.do(t, p.method, p.path, "", ""); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s: %s signed out = %d, want 401", when, p.path, resp.StatusCode)
			}
		}
	}
	off("before")
	authFirst("off")

	setModule(t, e, adminTok, true)
	// On: served, though no Bridge is paired (the routes gate on the toggle only).
	for _, r := range []struct{ method, path, tok string }{
		{"GET", "/api/v1/admin/modtest", adminTok},
		{"POST", "/api/v1/modtest", userTok},
	} {
		if resp, body := e.do(t, r.method, r.path, r.tok, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("on: %s %s = %d %s, want 200", r.method, r.path, resp.StatusCode, body)
		}
	}
	authFirst("on")

	setModule(t, e, adminTok, false)
	off("after")
}

func TestModuleRoutePatternMustBeAnAPIRoute(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"/api/v1/x", "GET /x", "get /api/v1/x", " /api/v1/x", "GET api/v1/x"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("pattern %q accepted", p)
				}
			}()
			checkModulePattern("contributions", p)
		}()
	}
	if got := checkModulePattern("contributions", "DELETE /api/v1/x/{id}"); got != "DELETE /api/v1/x/{id}" {
		t.Errorf("good pattern = %q", got)
	}
}

// SetModules refuses a registry modules.Validate refuses, the core capability
// names included.
func TestSetModulesValidates(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	for _, ms := range [][]modules.Module{
		{{ID: "bridge"}},   // a core capability
		{{ID: "metadata"}}, // another
		{{ID: "nope"}},     // no config row
		{{ID: "contributions"}, {ID: "contributions"}},
	} {
		if err := e.api.SetModules(ms); err == nil {
			t.Errorf("SetModules(%+v) accepted", ms)
		}
	}
	// A refused registry leaves the last one in place.
	if len(e.api.modules) != 2 || e.api.modules[0].ID != "contributions" {
		t.Fatalf("modules after refusals = %+v", e.api.modules)
	}
}

// fakeWork is a module's background work that reports each start and stop,
// and holds its return until release is closed once its context ends.
type fakeWork struct {
	started, stopped chan struct{}
	release          chan struct{}
}

func newFakeWork() *fakeWork {
	return &fakeWork{started: make(chan struct{}, 4), stopped: make(chan struct{}, 4), release: make(chan struct{})}
}

func (f *fakeWork) start(ctx context.Context, d modules.Deps) {
	if d.Catalog == nil || d.Bridge == nil || d.Log == nil {
		panic("module deps missing")
	}
	f.started <- struct{}{}
	<-ctx.Done()
	<-f.release
	f.stopped <- struct{}{}
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func never(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s happened", what)
	case <-time.After(150 * time.Millisecond):
	}
}

// A module's background work starts when it is turned on and is cancelled
// when it is turned off, without the settings save waiting for it; turned back
// on meanwhile, the new run waits for the old one to return. The server's
// context ending stops it too.
func TestModuleStartStop(t *testing.T) {
	t.Parallel()
	f := newFakeWork()
	e := newTestEnv(t)
	withModules(t, e, modules.Module{ID: "contributions", Start: f.start})
	adminTok := adminSession(t, e)

	// Before StartModules nothing runs, even on.
	setModule(t, e, adminTok, true)
	never(t, f.started, "a start before StartModules")
	setModule(t, e, adminTok, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.api.StartModules(ctx)
	never(t, f.started, "a start while off")

	setModule(t, e, adminTok, true)
	waitFor(t, f.started, "the start")
	setModule(t, e, adminTok, true) // a save that changes nothing starts nothing
	never(t, f.started, "a second start")

	// Off: the save answers although the run is still returning (release held).
	done := make(chan struct{})
	status := 0
	go func() {
		defer close(done)
		resp, _ := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"modules":{"contributions":false}}`)
		status = resp.StatusCode
	}()
	waitFor(t, done, "the save turning it off")
	if status != http.StatusOK {
		t.Fatalf("turning it off = %d", status)
	}
	never(t, f.stopped, "a stop before release")

	// Back on while the old run is still returning: no overlap.
	setModule(t, e, adminTok, true)
	never(t, f.started, "a start overlapping the old run")
	close(f.release)
	waitFor(t, f.stopped, "the old run's stop")
	waitFor(t, f.started, "the new run's start")

	// The server stopping ends it.
	cancel()
	waitFor(t, f.stopped, "the stop at shutdown")
}

// A module's background work that panics stops that module, not the server.
func TestModulePanicStaysInTheModule(t *testing.T) {
	t.Parallel()
	ran := make(chan struct{})
	e := newTestEnv(t)
	withModules(t, e, modules.Module{ID: "contributions", Start: func(context.Context, modules.Deps) {
		close(ran)
		panic("boom")
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.api.StartModules(ctx)
	setModule(t, e, adminSession(t, e), true)
	waitFor(t, ran, "the start")
	if resp, _ := e.do(t, "GET", "/api/v1/server", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("server after a module's panic: %d", resp.StatusCode)
	}
}

// Turning a module on or off is a settings save: audited like any other.
func TestModuleToggleIsAudited(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok := adminSession(t, e)
	setModule(t, e, adminTok, true)
	_, body := e.do(t, "GET", "/api/v1/admin/audit", adminTok, "")
	if !strings.Contains(body, `"settings.update"`) || !strings.Contains(body, `"setting":"modules.contributions"`) ||
		!strings.Contains(body, `"from":false`) || !strings.Contains(body, `"to":true`) {
		t.Fatalf("audit: %s", body)
	}
	data, err := os.ReadFile(config.Path(e.cfg.DataDir))
	if err != nil || !strings.Contains(string(data), "contributions:\n        enabled: true") {
		t.Fatalf("config.yaml: %v\n%s", err, data)
	}
}

// AUDIOSILO_MODULES_CONTRIBUTIONS_ENABLED turns the module on and locks its
// toggle (409 setting_locked, named in the envelope's locked map). Not
// parallel: it sets the environment.
func TestModuleToggleFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIOSILO_MODULES_CONTRIBUTIONS_ENABLED", "true")
	loaded, _, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEnvWith(t, func(c *config.Config) { *c = *loaded })
	withModules(t, e, modules.Module{ID: "contributions"})
	adminTok := adminSession(t, e)
	if !capability(t, e) {
		t.Fatal("the variable did not turn the module on")
	}
	resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"modules":{"contributions":false}}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"setting_locked"`) {
		t.Fatalf("toggle under the variable = %d %s, want 409 setting_locked", resp.StatusCode, body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/settings", adminTok, "")
	if !strings.Contains(body, `"modules.contributions":"AUDIOSILO_MODULES_CONTRIBUTIONS_ENABLED"`) {
		t.Fatalf("locked map: %s", body)
	}
}

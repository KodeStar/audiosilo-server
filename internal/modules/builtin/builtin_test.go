package builtin

import (
	"testing"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/modules"
)

// The shipped registry checks out against the config's rows; the API checks
// the core capability names too (its SetModules test).
func TestRegistryValidates(t *testing.T) {
	t.Parallel()
	ms := Modules()
	if err := modules.Validate(ms, config.Default(t.TempDir()).HasModule, nil); err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 || ms[0].ID != "contributions" || ms[0].RequiresBridge != bridge.FeatureContribute {
		t.Fatalf("registry = %+v, want contributions first, needing contribute", ms)
	}
	// Contributions' actions are Bridge jobs (bridge.ModuleFor), not routes or
	// background work of its own.
	if ms[0].ID != bridge.ModuleContributions || ms[0].Routes != nil || ms[0].Start != nil {
		t.Fatal("contributions has routes or background work of its own")
	}
}

// Audible (Phase 4a) needs a Bridge offering audible, and its background work
// is the poll scheduler.
func TestRegistryAudible(t *testing.T) {
	t.Parallel()
	for _, m := range Modules() {
		if m.ID == bridge.ModuleAudible {
			if m.RequiresBridge != bridge.FeatureAudible || m.Start == nil || m.Routes != nil {
				t.Fatalf("audible = %+v", m)
			}
			if bridge.ModuleFor(bridge.KindAudiblePoll, nil) != m.ID {
				t.Fatal("audible.poll jobs don't need the audible module")
			}
			return
		}
	}
	t.Fatal("no audible module")
}

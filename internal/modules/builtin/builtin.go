// Package builtin is the server's module registry: the modules every build
// ships, in the order the console lists them. A module's config row (its
// modules.<id>.enabled key) lives in internal/config's settings table; its name
// and blurb are the console's copy (settings.module.<id>.*), so the server
// sends only ids.
package builtin

import (
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/modules"
	"github.com/kodestar/audiosilo-server/internal/podcast"
)

// Modules returns the registry.
func Modules() []modules.Module {
	return []modules.Module{
		// Contributions (BRIDGE-PLAN.md Phase 2b): sending a book's corrections and
		// generated sidecars to the community metadata database through the Bridge,
		// which holds the GitHub sign-in. No routes or background work of its own:
		// its actions are Bridge jobs (sidecars.contribute, meta.correct, and
		// sidecars.generate's contribute "issue"), which the job queue refuses
		// while it is off (bridge.ModuleFor names it).
		{ID: bridge.ModuleContributions, RequiresBridge: bridge.FeatureContribute},
		{ID: bridge.ModulePodcasts, RequiresBridge: bridge.FeaturePodcast, Start: podcast.RunScheduler},
	}
}

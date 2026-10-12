//go:build race

package catalog

// raceEnabled: the race detector slows SQLite (pure Go) several times over, so
// timing checks only time a plain build.
const raceEnabled = true

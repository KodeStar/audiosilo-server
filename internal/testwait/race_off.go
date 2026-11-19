//go:build !race

package testwait

// Race is whether the race detector is on: timing checks only time a plain
// build, and waits are Scaled.
const Race = false

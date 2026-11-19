// Package testwait scales a test's waits to the build it runs in. The race
// detector slows the code several times over (SQLite, being pure Go, most), and
// a loaded machine (CI's shared runners, a gate running every package at once)
// slows it again, so a deadline that only ends a test that has already failed
// is set from a generous base and multiplied under -race (Scaled), rather than
// raised by hand wherever one was missed. Tests only. A deadline that only
// ends a failed test is Scaled; a test of a time bound itself (a budget, a
// look's time) scales all of its times alike, so their ratios, which are what
// it asserts, hold.
package testwait

import "time"

// raceFactor is how many times longer a wait is under the race detector.
const raceFactor = 4

// Scaled is d for this build: d, or raceFactor times d under -race.
func Scaled(d time.Duration) time.Duration {
	if Race {
		return d * raceFactor
	}
	return d
}

package launcher

import (
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/testwait"
)

// startWait is how long a test waits for a start (or a stop) of Run, a scan
// or a sweep it set going: a whole start (migrations from scratch, the first
// admin's hash) takes seconds under -race alone, more on a loaded machine.
// It only ends a test that has already failed.
var startWait = testwait.Scaled(startBase)

// startBase is startWait before it is Scaled.
const startBase = 15 * time.Second

// waitFor polls cond for up to startWait.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	testwait.Eventually(t, what, startBase, cond)
}

// waitRun waits for a stopped Run's result, which must be nil.
func waitRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(startWait):
		t.Fatal("Run did not stop")
	}
}

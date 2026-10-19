package modules

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// overlapWork is a module's background work that counts its concurrent runs
// and holds each return until release is closed.
type overlapWork struct {
	running, peak atomic.Int32
	starts        chan struct{}
	release       chan struct{}
}

func newOverlapWork() *overlapWork {
	return &overlapWork{starts: make(chan struct{}, 8), release: make(chan struct{})}
}

func (w *overlapWork) start(ctx context.Context, _ Deps) {
	n := w.running.Add(1)
	for p := w.peak.Load(); n > p && !w.peak.CompareAndSwap(p, n); p = w.peak.Load() {
	}
	w.starts <- struct{}{}
	<-ctx.Done()
	<-w.release
	w.running.Add(-1)
}

func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func quiet(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s happened", what)
	case <-time.After(100 * time.Millisecond):
	}
}

// Toggled on, off, on, off and on again while the first run is still
// returning, the module's runs still never overlap: the cancelled middle run
// mustn't let the last one past the first.
func TestRunnerNoOverlapAcrossCancelledWaits(t *testing.T) {
	t.Parallel()
	w := newOverlapWork()
	var on atomic.Bool
	isOn := func(string) bool { return on.Load() }
	var r Runner
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx, []Module{{ID: "m", Start: w.start}}, Deps{}, isOn)

	on.Store(true)
	r.Sync(isOn)
	wait(t, w.starts, "the first run")
	for _, v := range []bool{false, true, false, true} {
		on.Store(v)
		r.Sync(isOn)
	}
	quiet(t, w.starts, "a run overlapping the first")
	close(w.release)
	wait(t, w.starts, "the last run")
	if p := w.peak.Load(); p != 1 {
		t.Fatalf("%d runs at once", p)
	}
	r.Stop()
	if n := w.running.Load(); n != 0 {
		t.Fatalf("%d runs left after Stop", n)
	}
}

// A run that ended by itself (a panic, or Start returning early) while its
// module is on is started again by the next Sync.
func TestRunnerRestartsARunThatEnded(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 4)
	first := true
	var mu sync.Mutex
	start := func(ctx context.Context, _ Deps) {
		started <- struct{}{}
		mu.Lock()
		f := first
		first = false
		mu.Unlock()
		if f {
			panic("boom")
		}
		<-ctx.Done()
	}
	var r Runner
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	on := func(string) bool { return true }
	r.Start(ctx, []Module{{ID: "m", Start: start}}, Deps{}, on)
	wait(t, started, "the first run")
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.Sync(on)
		select {
		case <-started:
			r.Stop()
			return
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("a run that ended while on was never started again")
		}
	}
}

// Stop cancels every run and waits for each to return; a Sync after it starts
// nothing, and a second Start is ignored.
func TestRunnerStop(t *testing.T) {
	t.Parallel()
	w := newOverlapWork()
	close(w.release)
	on := func(string) bool { return true }
	var r Runner
	r.Stop() // before Start: nothing to do
	r.Start(context.Background(), []Module{{ID: "m", Start: w.start}}, Deps{}, on)
	wait(t, w.starts, "the run")
	r.Start(context.Background(), []Module{{ID: "m", Start: w.start}}, Deps{}, on)
	quiet(t, w.starts, "a second Start's run")
	r.Stop()
	if n := w.running.Load(); n != 0 {
		t.Fatalf("%d runs left after Stop", n)
	}
	r.Sync(on)
	quiet(t, w.starts, "a run after Stop")
}

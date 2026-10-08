package pool

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestEachStopsWhenAsked(t *testing.T) {
	var ran atomic.Int32
	items := make([]int, 20)
	Each(context.Background(), 2, items, func() bool { return ran.Load() < 5 }, func(*int) { ran.Add(1) })
	// more is asked before each hand-out, so at most the workers' items in flight
	// run past it.
	if n := ran.Load(); n < 5 || n > 7 {
		t.Fatalf("ran %d, want about 5", n)
	}
	ran.Store(0)
	Each(context.Background(), 3, items, func() bool { return true }, func(*int) { ran.Add(1) })
	if n := ran.Load(); n != 20 {
		t.Fatalf("ran %d, want all 20", n)
	}
}

package pool

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestPassRunnerCoalescesKicksAndBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var passes atomic.Int32
		r := NewPassRunner(PassOptions{Interval: time.Hour, Quiet: time.Second, MaxSettle: 2 * time.Second}, func(context.Context) bool {
			return passes.Add(1) == 2
		})
		go r.Run(ctx)
		synctest.Wait()
		if got := passes.Load(); got != 1 {
			t.Fatalf("startup ran %d passes", got)
		}
		for range 4 {
			r.Kick()
			time.Sleep(500 * time.Millisecond)
		}
		synctest.Wait()
		if got := passes.Load(); got != 2 {
			t.Fatalf("continuous kicks ran %d passes, want one after the settle limit", got)
		}
		r.Kick()
		time.Sleep(time.Minute)
		if got := passes.Load(); got != 2 {
			t.Fatal("a kick restarted the failing pass")
		}
		time.Sleep(time.Hour - time.Minute - 2*time.Second)
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if got := passes.Load(); got != 3 {
			t.Fatalf("after the scheduled retry: %d passes", got)
		}
	})
}

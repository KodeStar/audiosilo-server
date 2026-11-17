package pool

import (
	"context"
	"math/rand/v2"
	"time"
)

// WhileOn is ctx, cancelled with cause once on reports false (looked at every
// every); stop releases it. The daily jobs (internal/coverage,
// internal/upcoming) run under it, so a run in flight stops soon after its
// switch goes off.
func WhileOn(ctx context.Context, every time.Duration, on func() bool, cause error) (rctx context.Context, stop func()) {
	rctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-rctx.Done():
				return
			case <-t.C:
				if !on() {
					cancel(cause)
					return
				}
			}
		}
	}()
	return rctx, func() {
		close(done)
		cancel(nil)
	}
}

// Jittered is interval moved by up to jitter either way, at random: a daily
// job's next run, so many servers don't ask the community service at once.
func Jittered(interval, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return interval
	}
	return interval + time.Duration(rand.Int64N(int64(2*jitter))) - jitter //nolint:gosec // schedule jitter, not a secret
}

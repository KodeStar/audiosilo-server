package pool

import (
	"context"
	"sync/atomic"
	"time"
)

// PassOptions configures a PassRunner.
type PassOptions struct {
	// Interval is how often a pass runs without a Kick, and how long a failing
	// pass waits before the next.
	Interval time.Duration
	// Quiet is how long a Kick waits for no other before the pass starts;
	// MaxSettle bounds that wait.
	Quiet, MaxSettle time.Duration
	// SettleOnTick also waits for quiet at startup and on periodic wakes.
	SettleOnTick bool
	// Enabled (nil: always) is asked before each pass; a pass it refuses is skipped.
	Enabled func() bool
	// WaitReady (nil: none) runs before the first enabled pass. False ends the runner.
	WaitReady func(context.Context) bool
}

// PassRunner schedules one background pass at a time. Kicks during settling
// extend the quiet period up to the settle limit. A failing pass waits for the tick.
type PassRunner struct {
	opt  PassOptions
	pass func(context.Context) bool
	kick chan struct{}
}

// NewPassRunner returns a runner of pass, which reports true when it failed.
func NewPassRunner(opt PassOptions, pass func(context.Context) bool) *PassRunner {
	return &PassRunner{opt: opt, pass: pass, kick: make(chan struct{}, 1)}
}

// Kick asks for a pass once the kicks settle. Never blocks.
func (r *PassRunner) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

func (r *PassRunner) on() bool { return r.opt.Enabled == nil || r.opt.Enabled() }

// Run keeps the schedule until ctx ends.
func (r *PassRunner) Run(ctx context.Context) {
	tick := time.NewTicker(r.opt.Interval)
	defer tick.Stop()
	settle := r.opt.SettleOnTick
	waitReady := r.opt.WaitReady
	for ctx.Err() == nil {
		if settle && !Settle(ctx, r.kick, r.opt.Quiet, r.opt.MaxSettle) {
			return
		}
		if waitReady != nil && r.on() {
			if !waitReady(ctx) {
				return
			}
			waitReady = nil
			tick.Reset(r.opt.Interval)
		}
		// This pass covers all changes already queued, including those
		// made while waiting for readiness or for a retry after failure.
		select {
		case <-r.kick:
		default:
		}
		kick := r.kick
		if r.on() && r.pass(ctx) {
			kick = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			settle = r.opt.SettleOnTick
		case <-kick:
			settle = true
		}
	}
}

// Settle waits until no kick has come for quiet, at most maxSettle; false when
// ctx ended first.
func Settle(ctx context.Context, kick <-chan struct{}, quiet, maxSettle time.Duration) bool {
	q := time.NewTimer(quiet)
	defer q.Stop()
	limit := time.NewTimer(maxSettle)
	defer limit.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-q.C:
			return true
		case <-limit.C:
			return true
		case <-kick:
			q.Reset(quiet)
		}
	}
}

// FailureStreak counts consecutive failures across the workers of one pass.
type FailureStreak struct {
	count atomic.Int32
	limit int32
}

func NewFailureStreak(limit int32) *FailureStreak { return &FailureStreak{limit: limit} }
func (s *FailureStreak) Failed()                  { s.count.Add(1) }
func (s *FailureStreak) Succeeded()               { s.count.Store(0) }
func (s *FailureStreak) AtLimit() bool            { return s.count.Load() >= s.limit }

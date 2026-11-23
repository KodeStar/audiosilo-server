package pool

import (
	"context"
	"sync/atomic"
	"time"
)

// PassRunner schedules one background pass at a time. Kicks during settling
// extend the quiet period up to the settle limit. A failing pass waits for the tick.
type PassRunner struct {
	interval, maxSettle time.Duration
	Quiet               time.Duration
	// SettleOnTick also waits for quiet at startup and on periodic wakes.
	SettleOnTick bool
	Enabled      func() bool
	// WaitReady runs before the first enabled pass. False ends the runner.
	WaitReady func(context.Context) bool
	pass      func(context.Context) bool
	kick      chan struct{}
}

func NewPassRunner(interval, quiet, maxSettle time.Duration, pass func(context.Context) bool) *PassRunner {
	return &PassRunner{interval: interval, Quiet: quiet, maxSettle: maxSettle, pass: pass, kick: make(chan struct{}, 1)}
}

func (r *PassRunner) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

func (r *PassRunner) Run(ctx context.Context) {
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	settle := r.SettleOnTick
	waitReady := r.WaitReady
	for ctx.Err() == nil {
		if settle && !r.settled(ctx) {
			return
		}
		if waitReady != nil && (r.Enabled == nil || r.Enabled()) {
			if !waitReady(ctx) {
				return
			}
			waitReady = nil
			tick.Reset(r.interval)
		}
		// This pass covers all changes already queued, including those
		// made while waiting for readiness or for a retry after failure.
		select {
		case <-r.kick:
		default:
		}
		kick := r.kick
		if (r.Enabled == nil || r.Enabled()) && r.pass(ctx) {
			kick = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			settle = r.SettleOnTick
		case <-kick:
			settle = true
		}
	}
}

func (r *PassRunner) settled(ctx context.Context) bool {
	quiet := time.NewTimer(r.Quiet)
	defer quiet.Stop()
	limit := time.NewTimer(r.maxSettle)
	defer limit.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-quiet.C:
			return true
		case <-limit.C:
			return true
		case <-r.kick:
			quiet.Reset(r.Quiet)
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

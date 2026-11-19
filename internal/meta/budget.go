package meta

import (
	"context"
	"sync"
	"time"
)

// A budget bounds one call's community reads (composeBudget for a compose, a match's
// candidates or a batch of work ids; seriesFactsTimeout for a match's series facts).
// Outside a bulk run it is a plain deadline. In a bulk run (a Pacer in the context)
// the time a read sits out the service's Retry-After (getJSON) is not counted: the
// wait is the run's, under its Pacer, and two of them (up to maxRetryAfter each)
// would spend a book's 15 s and record it as "couldn't be checked" though the
// service answered it once asked again (server#447). The budget still bounds the
// reads themselves, and the waits stay bounded by rateLimitRetries.
func withBudget(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if pacerOf(ctx) == nil {
		return context.WithTimeout(ctx, d)
	}
	cctx, cancel := context.WithCancelCause(ctx)
	b := &runBudget{parent: budgetOf(ctx), ends: time.Now().Add(d)}
	b.timer = time.AfterFunc(d, func() { cancel(context.DeadlineExceeded) })
	return context.WithValue(cctx, budgetKey{}, b), func() {
		b.timer.Stop()
		cancel(context.Canceled)
	}
}

type budgetKey struct{}

// runBudget is a bulk run's budget (withBudget): a timer that ends its context,
// stopped while any read under it sits out a wait (holdBudgets).
type runBudget struct {
	// parent is the budget this one is inside (a match's series facts inside its
	// candidates'), held with it.
	parent *runBudget
	timer  *time.Timer

	mu      sync.Mutex
	ends    time.Time     // when it runs out, while its clock runs
	left    time.Duration // what was left when its clock stopped
	holds   int           // the reads sitting out a wait under it
	stopped bool          // the clock was stopped (not already run out)
}

func budgetOf(ctx context.Context) *runBudget {
	b, _ := ctx.Value(budgetKey{}).(*runBudget)
	return b
}

// holdBudgets stops the clock of every bulk-run budget ctx is under (none outside a
// run) until release is called.
func holdBudgets(ctx context.Context) (release func()) {
	var held []*runBudget
	for b := budgetOf(ctx); b != nil; b = b.parent {
		b.hold()
		held = append(held, b)
	}
	return func() {
		for _, b := range held {
			b.release()
		}
	}
}

func (b *runBudget) hold() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.holds++; b.holds == 1 && b.timer.Stop() {
		b.stopped, b.left = true, time.Until(b.ends)
	}
}

func (b *runBudget) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.holds--; b.holds == 0 && b.stopped {
		b.stopped, b.ends = false, time.Now().Add(b.left)
		b.timer.Reset(b.left)
	}
}

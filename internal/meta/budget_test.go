package meta

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// A bulk run's budget runs out on its own like a deadline, its clock stops while a
// read under it (or under a budget inside it) sits out a wait, and outside a run a
// budget is a plain deadline.
func TestBudgetHoldsForWaits(t *testing.T) {
	t.Parallel()
	run := WithPacer(context.Background(), quietPacer(1000))

	ctx, cancel := withBudget(run, 30*time.Millisecond)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("a run's budget has a deadline a wait would be cut short by")
	}
	<-ctx.Done()
	if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		t.Fatalf("cause = %v, want the deadline", context.Cause(ctx))
	}

	outer, cancelOuter := withBudget(run, 60*time.Millisecond)
	defer cancelOuter()
	inner, cancelInner := withBudget(outer, 60*time.Millisecond)
	defer cancelInner()
	release := holdBudgets(inner)
	time.Sleep(120 * time.Millisecond) // twice either budget, all of it a wait
	if outer.Err() != nil || inner.Err() != nil {
		t.Fatalf("a budget ran out during a wait: %v %v", outer.Err(), inner.Err())
	}
	release()
	select {
	case <-inner.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the budget never ran out after the wait")
	}

	plain, cancelPlain := withBudget(context.Background(), time.Hour)
	defer cancelPlain()
	if _, ok := plain.Deadline(); !ok {
		t.Fatal("outside a run a budget is a deadline")
	}
}

// TestRunRateLimitWaitsOutsideTheBudget (server#447): in a bulk run two 429s, each
// waited out for longer than a book's whole budget, still end in the answer; the
// match dialog (no run) is told at once, as before.
func TestRunRateLimitWaitsOutsideTheBudget(t *testing.T) {
	t.Parallel()
	var n atomic.Int32
	m := &structuredMeta{matchCode: http.StatusOK, matchBody: sharpeMatch}
	s := m.service(t)
	upstream := s.client.http.Transport
	s.client.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/works/match" && n.Add(1) <= 2 {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"1"}},
				Body: http.NoBody, Request: r}, nil
		}
		return upstream.RoundTrip(r)
	})
	s.composeBudget = 100 * time.Millisecond
	s.client.sleep = func(ctx context.Context, _ time.Duration) error {
		return sleepCtx(ctx, 80*time.Millisecond) // the pair of waits outlasts the budget
	}

	q := sharpeBook
	q.Limit, q.BestOnly = 2, true
	cands, err := s.Candidates(WithPacer(context.Background(), quietPacer(1000)), q)
	if err != nil || len(cands) == 0 || cands[0].WorkID != "sharpes-eagle" {
		t.Fatalf("a run's book after two 429s = %+v, %v; want the match", cands, err)
	}
	if got := n.Load(); got != 3 {
		t.Fatalf("works/match asked %d times, want 3", got)
	}

	n.Store(0)
	if _, err := s.Candidates(context.Background(), q); err == nil {
		t.Fatal("the dialog under a 429 its budget can't wait out: want the error")
	}
}

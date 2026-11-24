package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	// The budget is for the reads alone (three requests, on a busy test host
	// too); the pair of waits outlasts it.
	s.composeBudget = 300 * time.Millisecond
	s.client.sleep = func(ctx context.Context, _ time.Duration) error {
		return sleepCtx(ctx, 200*time.Millisecond)
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

// TestSharedBudget (server#447): under WithSharedBudget a compose ends with the
// shared budget, however much of its own it has left, so two composes for one
// request can't take the API's whole request budget; and running out of it is
// the upstream's failure, cached as one (the caller is still there).
//
// Nothing here waits on the clock: on a loaded host the shared budget can run
// out before the lookup reaches the server (no lookup at all), so the test
// holds the answer that is cached, not the number of lookups the first call
// made. The upstream never answers (release closes only at cleanup) and the
// client's own per-request timeout is off, so only the shared budget can end
// the first call, and a second call that asked again would never return.
func TestSharedBudget(t *testing.T) {
	t.Parallel()
	var lookups atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	s := NewService(srv.URL, nil)
	s.composeBudget = time.Hour
	s.client.http.Timeout = 0
	enrich := func(ctx context.Context) error {
		done := make(chan error, 1)
		go func() {
			_, err := s.Enrich(ctx, "B00SLOW001", "")
			done <- err
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(time.Minute): // a hang, not a slow host
			t.Fatal("Enrich never ended: the shared budget didn't bound it")
			return nil
		}
	}

	ctx := WithSharedBudget(context.Background(), 50*time.Millisecond)
	first := enrich(ctx)
	if first == nil || errors.Is(first, ErrNotFound) {
		t.Fatalf("Enrich past the shared budget = %v, want a failure", first)
	}
	if ctx.Err() != nil {
		t.Fatal("the caller's context ended")
	}
	if _, hit, err := cacheGet[Enrichment](s.cache, cacheKey("B00SLOW001", "")); !hit || err != first {
		t.Fatalf("cached = %v (hit %v), want the shared budget's failure cached", err, hit)
	}
	if again := enrich(context.Background()); again != first {
		t.Fatalf("again = %v, want the cached failure %v", again, first)
	}
	if n := lookups.Load(); n > 1 {
		t.Fatalf("%d lookups, want at most the first call's", n)
	}
}

// In a run, a book waiting on another worker's read of the same series (which
// may be sitting out a Retry-After) has its budget held for the wait too, so it
// still gets the facts once that read lands (server#447).
func TestSeriesFactsWaiterBudgetHeld(t *testing.T) {
	t.Parallel()
	s := NewService("http://127.0.0.1:1", nil)
	flight := make(chan struct{})
	s.flightMu.Lock()
	s.seriesFlights["s1"] = flight
	s.flightMu.Unlock()

	// Long enough for the waiter to start on a busy test host.
	const budget = 100 * time.Millisecond
	ctx, cancel := withBudget(WithPacer(context.Background(), quietPacer(1000)), budget)
	defer cancel()
	got := make(chan *seriesFacts, 1)
	go func() { got <- s.seriesFacts(ctx, "s1") }()
	time.Sleep(3 * budget) // three times the budget, all of it the other read's
	if ctx.Err() != nil {
		t.Fatal("the waiter's budget ran out while it waited on another worker's read")
	}
	want := &seriesFacts{}
	s.flightMu.Lock()
	cachePut(s.cache, nsSeries.key("s1"), want, positiveTTL)
	delete(s.seriesFlights, "s1")
	s.flightMu.Unlock()
	close(flight)
	if f := <-got; f != want {
		t.Fatalf("facts = %v, want the landed read's", f)
	}
}

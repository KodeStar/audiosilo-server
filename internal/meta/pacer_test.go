package meta

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietPacer(perSecond int) *Pacer {
	return NewPacer(perSecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// recordingPacer is a pacer on a clock that moves only by its sleeps.
func recordingPacer(perSecond int) (*Pacer, *[]time.Duration) {
	p := quietPacer(perSecond)
	clock := time.Unix(1000, 0)
	p.now = func() time.Time { return clock }
	sleeps := &[]time.Duration{}
	p.sleep = func(_ context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		clock = clock.Add(d)
		return nil
	}
	return p, sleeps
}

// Requests are spaced by the rate: none under it, whoever asks, and nothing saved up
// over an idle spell to burst with later.
func TestPacerSpacesRequests(t *testing.T) {
	p, sleeps := recordingPacer(50)
	ctx := context.Background()
	for range 4 {
		if err := p.wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	want := 20 * time.Millisecond
	if len(*sleeps) != 3 || (*sleeps)[0] != want || (*sleeps)[1] != want || (*sleeps)[2] != want {
		t.Fatalf("sleeps = %v, want three of %v (the first request goes at once)", *sleeps, want)
	}

	// An hour idle: the next goes at once, and the one after it still waits.
	clock := p.now().Add(time.Hour)
	p.now = func() time.Time { return clock }
	*sleeps = nil
	_ = p.wait(ctx)
	_ = p.wait(ctx)
	if len(*sleeps) != 1 || (*sleeps)[0] != want {
		t.Fatalf("after idling, sleeps = %v, want one of %v", *sleeps, want)
	}
}

// On the real clock, through the client, with several workers at once: the requests
// that reach the service are never faster than the rate. Only the lower bound is
// asserted, as a loaded machine only makes it slower.
func TestPacerHoldsARunUnderTheRate(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		_, _ = w.Write([]byte(`{"recording_id":"r1"}`))
	}))
	t.Cleanup(srv.Close)
	c := newClient(srv.URL)
	ctx := WithPacer(context.Background(), quietPacer(100))
	const requests = 31
	start := time.Now()
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			for n.Load() < requests {
				if _, err := c.lookup(ctx, "B00TEST", ""); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	got := int(n.Load())
	if floor := time.Duration(got-1) * 10 * time.Millisecond; time.Since(start) < floor {
		t.Fatalf("%d requests in %v, want at least %v", got, time.Since(start), floor)
	}
}

// A cancelled run stops waiting for its slot.
func TestPacerWaitStopsWithTheContext(t *testing.T) {
	p := quietPacer(1)
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.wait(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.wait(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait = %v, want the context's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait outlived its context")
	}
}

// Mirror mode: what the copy answers takes no slot; what it fails over takes one.
func TestPacerOnlyPacesTheRemoteLeg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"remote"}`))
	}))
	t.Cleanup(srv.Close)
	s := NewService(srv.URL, nil)
	var failing atomic.Bool
	s.SetMirror(testMirror{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"id":"local"}`))
	})}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := recordingPacer(10)
	ctx := WithPacer(context.Background(), p)
	for range 3 {
		if d, err := s.client.work(ctx, "w"); err != nil || d.ID != "local" {
			t.Fatalf("work = %+v, %v", d, err)
		}
	}
	if !p.next.IsZero() {
		t.Fatalf("the copy's answers took a slot: next = %v", p.next)
	}
	failing.Store(true)
	if d, err := s.client.work(ctx, "w"); err != nil || d.ID != "remote" {
		t.Fatalf("work = %+v, %v", d, err)
	}
	if p.next.IsZero() {
		t.Fatal("the remote leg took no slot")
	}
}

type testMirror struct{ h http.Handler }

func (m testMirror) Handler() http.Handler { return m.h }
func (testMirror) Ready() bool             { return true }

// answerThen answers code (with Retry-After retryAfter) for the first bad requests,
// then 200.
func answerThen(t *testing.T, code int, bad int32, retryAfter string) (*client, *atomic.Int32, *[]time.Duration) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) <= bad {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write([]byte(`{"recording_id":"r1"}`))
	}))
	t.Cleanup(srv.Close)
	c := newClient(srv.URL)
	waits := &[]time.Duration{}
	c.sleep = func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
	return c, &n, waits
}

// A bulk run waits a 503 out as it does a 429, its Retry-After honoured and the run's
// total kept.
func TestClientRunRetries503(t *testing.T) {
	t.Parallel()
	c, n, waits := answerThen(t, http.StatusServiceUnavailable, 2, "2")
	p := quietPacer(1000)
	got, err := c.lookup(WithPacer(context.Background(), p), "B00TEST", "")
	if err != nil || got.RecordingID != "r1" {
		t.Fatalf("lookup after two 503s = %+v %v, want the answer", got, err)
	}
	if n.Load() != 3 || len(*waits) != 2 || (*waits)[0] != 2*time.Second {
		t.Fatalf("requests %d waits %v, want 3 requests and two 2s waits", n.Load(), *waits)
	}
	if count, total := p.Waits(); count != 2 || total != 4*time.Second {
		t.Fatalf("run total = %d, %v; want 2, 4s", count, total)
	}
}

// Bounded as a 429 is: the retries, each wait capped, then the error.
func TestClientRun503GivesUp(t *testing.T) {
	t.Parallel()
	c, n, waits := answerThen(t, http.StatusServiceUnavailable, 100, "3600")
	_, err := c.lookup(WithPacer(context.Background(), quietPacer(1000)), "B00TEST", "")
	var st *statusError
	if !errors.As(err, &st) || st.code != http.StatusServiceUnavailable {
		t.Fatalf("lookup under 503s = %v, want the 503", err)
	}
	if n.Load() != rateLimitRetries+1 || len(*waits) != rateLimitRetries {
		t.Fatalf("requests %d waits %v, want %d requests", n.Load(), *waits, rateLimitRetries+1)
	}
	for _, d := range *waits {
		if d != maxRetryAfter {
			t.Fatalf("wait %v, want the cap %v", d, maxRetryAfter)
		}
	}
}

// Anyone else (the dialog, a player's lookup) is told about a 503 at once.
func TestClient503OutsideARunIsAnOutage(t *testing.T) {
	t.Parallel()
	c, n, waits := answerThen(t, http.StatusServiceUnavailable, 100, "1")
	if _, err := c.lookup(context.Background(), "B00TEST", ""); err == nil {
		t.Fatal("want an error")
	}
	if n.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("requests %d waits %v, want one request and no wait", n.Load(), *waits)
	}
}

// A 429's waits count in a run's total too, and other failures aren't waited out.
func TestClientRunCountsRateLimitWaits(t *testing.T) {
	t.Parallel()
	c, _, _ := answerThen(t, http.StatusTooManyRequests, 1, "1")
	p := quietPacer(1000)
	if _, err := c.lookup(WithPacer(context.Background(), p), "B00TEST", ""); err != nil {
		t.Fatal(err)
	}
	if count, _ := p.Waits(); count != 1 {
		t.Fatalf("waits = %d, want 1", count)
	}

	c, n, waits := answerThen(t, http.StatusInternalServerError, 100, "1")
	if _, err := c.lookup(WithPacer(context.Background(), quietPacer(1000)), "B00TEST", ""); err == nil {
		t.Fatal("want an error")
	}
	if n.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("a 500: requests %d waits %v, want no retry", n.Load(), *waits)
	}
}

// Every answer leaves its connection reusable: a JSON body with something after the
// value, a failure with a body, a not-found.
func TestClientReusesConnections(t *testing.T) {
	var conns atomic.Int32
	var n atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch n.Add(1) % 3 {
		case 0:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(strings.Repeat("oops", 4096)))
		case 1:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(strings.Repeat("gone", 1024)))
		default:
			w.(http.Flusher).Flush() // chunked: EOF comes only after the value
			_, _ = w.Write([]byte(`{"recording_id":"r1"}`))
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte("\n\n"))
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	c := newClient(srv.URL)
	for range 12 {
		_, _ = c.lookup(context.Background(), "B00TEST", "")
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("%d connections for 12 sequential requests, want 1", got)
	}
}

// Enough idle connections stay for a run's workers over HTTP/1.1: not the default two.
func TestClientKeepsConnectionsForEveryWorker(t *testing.T) {
	var conns atomic.Int32
	release := make(chan struct{})
	var inFlight atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inFlight.Add(1)
		<-release
		_, _ = w.Write([]byte(`{"recording_id":"r1"}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	c := newClient(srv.URL)
	const workers = 6
	round := func() {
		inFlight.Store(0)
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				if _, err := c.lookup(context.Background(), "B00TEST", ""); err != nil {
					t.Error(err)
				}
			})
		}
		for inFlight.Load() < workers {
			time.Sleep(time.Millisecond)
		}
		release <- struct{}{}
		for range workers - 1 {
			release <- struct{}{}
		}
		wg.Wait()
	}
	round()
	first := conns.Load()
	round()
	if got := conns.Load(); got != first {
		t.Fatalf("second round opened %d more connections", got-first)
	}
}

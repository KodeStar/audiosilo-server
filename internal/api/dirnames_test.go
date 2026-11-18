package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// fakeDirs is a dirNames read that counts its reads per folder and answers
// each folder's names or err, after gate when gate is set.
type fakeDirs struct {
	mu    sync.Mutex
	reads map[string]int
	names map[string][]string
	err   error
	gate  chan struct{}
}

func (f *fakeDirs) read(ctx context.Context, dir string, _ time.Duration) ([]string, error) {
	f.mu.Lock()
	f.reads[dir]++
	gate, err := f.gate, f.err
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return f.names[dir], nil
}

func (f *fakeDirs) count(dir string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[dir]
}

// newFakeDirNames is a dirNames over a fakeDirs, its clock at now.
func newFakeDirNames(names map[string][]string, now *time.Time) (*dirNames, *fakeDirs) {
	f := &fakeDirs{reads: map[string]int{}, names: names}
	c := newDirNames(f.read)
	c.now = func() time.Time { return *now }
	return c, f
}

// TestDirNamesKeepsAFolderForItsTTL: a folder is read once while its entry
// lives (dirNamesTTL from its read), then again.
func TestDirNamesKeepsAFolderForItsTTL(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	c, f := newFakeDirNames(map[string][]string{"/r": {"a", "b"}}, &now)
	for range 5 {
		if got, err := c.names(t.Context(), "/r", time.Second); err != nil || !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("names = %v %v", got, err)
		}
	}
	if n := f.count("/r"); n != 1 {
		t.Errorf("read %d times within the TTL, want 1", n)
	}
	now = now.Add(dirNamesTTL)
	if _, err := c.names(t.Context(), "/r", time.Second); err != nil || f.count("/r") != 2 {
		t.Errorf("after the TTL: %v, %d reads, want 2", err, f.count("/r"))
	}
	now = now.Add(2 * dirNamesTTL)
	c.mu.Lock()
	c.expire(now)
	size, held := c.size, c.order.Len()
	c.mu.Unlock()
	if size != 0 || held != 0 {
		t.Errorf("after expiry: %d bytes in %d entries, want none", size, held)
	}
}

// TestDirNamesBounds: at most max folders and cap bytes of names are kept, the
// oldest dropped first; a folder whose names alone pass cap is read for each
// request and never kept.
func TestDirNamesBounds(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	big := make([]string, 100)
	for i := range big {
		big[i] = strings.Repeat("x", 10)
	}
	c, f := newFakeDirNames(map[string][]string{"/a": {"1"}, "/b": {"2"}, "/c": {"3"}, "/big": big}, &now)
	c.max = 2
	c.cap = 3 * (1 + dirNameCost) // three one-letter names
	for _, d := range []string{"/a", "/b", "/c"} {
		if _, err := c.names(t.Context(), d, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	_, kept := c.byDir["/a"]
	held, size := c.order.Len(), c.size
	c.mu.Unlock()
	if kept || held != 2 || size != 2*(1+dirNameCost) {
		t.Errorf("after three folders: /a kept %v, %d entries, %d bytes; want the oldest dropped, 2 entries", kept, held, size)
	}
	for range 3 {
		if got, err := c.names(t.Context(), "/big", time.Second); err != nil || len(got) != 100 {
			t.Fatalf("big: %d names, %v", len(got), err)
		}
	}
	c.mu.Lock()
	_, kept = c.byDir["/big"]
	size = c.size
	c.mu.Unlock()
	if n := f.count("/big"); n != 3 || kept || size > c.cap {
		t.Errorf("a folder past the bound: %d reads, kept %v, %d bytes; want a read per request, never kept", n, kept, size)
	}
	// Bytes over the bound drop the oldest too: a fourth name fits only once
	// one of the three goes.
	c.max = 10
	for _, d := range []string{"/a", "/b", "/c"} {
		if _, err := c.names(t.Context(), d, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	f.names["/d"] = []string{"4"}
	if _, err := c.names(t.Context(), "/d", time.Second); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	held, size = c.order.Len(), c.size
	c.mu.Unlock()
	if held != 3 || size != 3*(1+dirNameCost) {
		t.Errorf("four one-name folders under a three-name bound: %d entries, %d bytes, want 3", held, size)
	}
}

// TestDirNamesOneReadInFlight: requests for a folder being read wait for that
// read (one read for all); a read that fails is never kept, and whoever waited
// on it reads for itself, so a client that left never fails another's check.
func TestDirNamesOneReadInFlight(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	c, f := newFakeDirNames(map[string][]string{"/r": {"a"}}, &now)
	f.gate = make(chan struct{})
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 8 {
		wg.Go(func() {
			if got, err := c.names(t.Context(), "/r", 10*time.Second); err != nil || !slices.Equal(got, []string{"a"}) {
				bad.Add(1)
			}
		})
	}
	for f.count("/r") == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // the others reach the wait
	close(f.gate)
	wg.Wait()
	if n := f.count("/r"); n != 1 || bad.Load() != 0 {
		t.Errorf("8 requests at once: %d reads, %d bad answers, want 1 read", n, bad.Load())
	}

	// The first request's client leaves while its read is in flight: the
	// waiter reads for itself and gets the names; nothing failed is kept.
	f.mu.Lock()
	f.gate, f.reads = make(chan struct{}), map[string]int{}
	f.mu.Unlock()
	f.names["/s"] = []string{"b"}
	leftCtx, leave := context.WithCancel(t.Context())
	leader := make(chan error, 1)
	go func() {
		_, err := c.names(leftCtx, "/s", 10*time.Second)
		leader <- err
	}()
	for f.count("/s") == 0 {
		time.Sleep(time.Millisecond)
	}
	waiter := make(chan []string, 1)
	go func() {
		got, _ := c.names(t.Context(), "/s", 10*time.Second)
		waiter <- got
	}()
	time.Sleep(20 * time.Millisecond)
	leave()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Errorf("the leaving request: %v, want its context's error", err)
	}
	f.mu.Lock()
	close(f.gate)
	f.mu.Unlock()
	if got := <-waiter; !slices.Equal(got, []string{"b"}) {
		t.Errorf("the waiter got %v, want its own read's names", got)
	}
	if n := f.count("/s"); n != 2 {
		t.Errorf("%d reads, want the leader's and the waiter's own", n)
	}

	// A wait that runs out is given up on, as a read would be.
	f.mu.Lock()
	f.gate = make(chan struct{})
	f.mu.Unlock()
	f.names["/t"] = []string{"c"}
	go func() { _, _ = c.names(t.Context(), "/t", 10*time.Second) }()
	for f.count("/t") == 0 {
		time.Sleep(time.Millisecond)
	}
	if _, err := c.names(t.Context(), "/t", 10*time.Millisecond); !metadata.GaveUp(err) {
		t.Errorf("a wait past its timeout: %v, want ErrNotAnswered", err)
	}
	f.mu.Lock()
	close(f.gate)
	f.mu.Unlock()

	// A failed read is not kept.
	f.mu.Lock()
	f.gate, f.err = nil, errors.New("eio")
	f.mu.Unlock()
	for range 2 {
		if _, err := c.names(t.Context(), "/u", time.Second); err == nil {
			t.Fatal("want the read's error")
		}
	}
	if n := f.count("/u"); n != 2 {
		t.Errorf("a failing folder read %d times, want once per request", n)
	}
}

// TestDirNamesSlowReadIsJoined: a read in flight past the TTL is still the
// folder's one read (a request then waits for it rather than reading again),
// and when it fails, the requests that waited on it join one read of their
// own rather than each reading the folder.
func TestDirNamesSlowReadIsJoined(t *testing.T) {
	t.Parallel()
	var (
		clock sync.Mutex
		now   = time.Unix(1000, 0)
	)
	f := &fakeDirs{reads: map[string]int{}, names: map[string][]string{"/r": {"a"}}, gate: make(chan struct{})}
	c := newDirNames(f.read)
	c.now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		return now
	}
	leftCtx, leave := context.WithCancel(t.Context())
	leader := make(chan error, 1)
	go func() {
		_, err := c.names(leftCtx, "/r", 10*time.Second)
		leader <- err
	}()
	for f.count("/r") == 0 {
		time.Sleep(time.Millisecond)
	}
	clock.Lock()
	now = now.Add(2 * dirNamesTTL) // the read is slow: past the TTL, still in flight
	clock.Unlock()
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 4 {
		wg.Go(func() {
			if got, err := c.names(t.Context(), "/r", 10*time.Second); err != nil || !slices.Equal(got, []string{"a"}) {
				bad.Add(1)
			}
		})
	}
	time.Sleep(20 * time.Millisecond) // the four reach the wait
	if n := f.count("/r"); n != 1 {
		t.Errorf("requests during a read past the TTL: %d reads, want the one in flight", n)
	}
	leave() // the leader's client leaves: its read fails, the four join one read
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Errorf("the leaving request: %v, want its context's error", err)
	}
	for f.count("/r") < 2 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // the others reach that read's wait
	f.mu.Lock()
	close(f.gate)
	f.mu.Unlock()
	wg.Wait()
	if n := f.count("/r"); n != 2 || bad.Load() != 0 {
		t.Errorf("four waiters on a failed read: %d reads in all, %d bad answers, want the failed one and one more", n, bad.Load())
	}
}

// TestDirNamesReadPanics: a read that panics is its request's error, and the
// folder is read again by the next request, never left in flight for good.
func TestDirNamesReadPanics(t *testing.T) {
	t.Parallel()
	calls := 0
	c := newDirNames(func(context.Context, string, time.Duration) ([]string, error) {
		if calls++; calls == 1 {
			panic("boom")
		}
		return []string{"a"}, nil
	})
	if _, err := c.names(t.Context(), "/r", time.Second); err == nil {
		t.Fatal("a read that panicked: no error")
	}
	if got, err := c.names(t.Context(), "/r", time.Second); err != nil || !slices.Equal(got, []string{"a"}) {
		t.Errorf("the next request: %v %v, want its own read's names", got, err)
	}
}

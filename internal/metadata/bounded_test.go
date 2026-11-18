package metadata

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A Bounded call answers as the call does; one on a share that stopped
// answering (a held path) is given up on at its time, counted on the path's
// mount until it returns, what it returns late dropped; at the mount's cap
// further calls there are refused at once, without being made, while a path
// elsewhere answers as ever. Not parallel: HoldOpensForTests swaps the mount
// lookup.
func TestBoundedGivesUpAndCounts(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	path := filepath.Join(dir, "cover.jpg")
	if err := os.WriteFile(path, []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := StatBounded(t.Context(), path, time.Minute); err != nil || info.Size() != 3 {
		t.Fatalf("a stat that answers = %v, %v; want the file's", info, err)
	}
	if _, err := LstatBounded(t.Context(), filepath.Join(dir, "none"), time.Minute); !errors.Is(err, fs.ErrNotExist) || GaveUp(err) {
		t.Fatalf("a stat of nothing = %v, want its own error, not given up", err)
	}

	release := HoldOpensForTests(dir)
	t.Cleanup(release)
	hung := stuckByMount.For(path)
	var dropped atomic.Int32
	start := time.Now()
	_, err := Bounded(t.Context(), path, 20*time.Millisecond, func() (*os.File, error) { return os.Open(path) },
		func(f *os.File) { dropped.Add(1); _ = f.Close() })
	if !errors.Is(err, ErrNotAnswered) || !GaveUp(err) {
		t.Fatalf("a held call = %v, want ErrNotAnswered", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the held call took %v, want it given up at its time", d)
	}
	if n := hung.n.Load(); n != 1 {
		t.Errorf("abandoned reads = %d, want the held call counted", n)
	}
	for hung.n.Load() < maxAbandonedReads {
		_, _ = ReadDirBounded(t.Context(), dir, time.Millisecond)
	}
	made := false
	if _, err := Bounded(t.Context(), path, time.Minute, func() (int, error) { made = true; return 0, nil }, nil); !errors.Is(err, ErrReadRefused) || made {
		t.Errorf("a call at the cap = %v (made %v), want it refused without being made", err, made)
	}
	if _, err := StatBounded(t.Context(), elsewhere, time.Minute); err != nil {
		t.Errorf("a stat on another mount at the held one's cap = %v, want it answered", err)
	}
	release()
	waitUncounted(t, hung)
	if dropped.Load() != 1 {
		t.Errorf("late results dropped = %d, want the held open's file closed", dropped.Load())
	}
}

// A Bounded call whose caller's context ends is left at once with ctx's error,
// not a give-up (the caller stopped, the share may be fine), and one whose ctx
// ended already is never made. A panic in the call is its error.
func TestBoundedContextAndPanic(t *testing.T) {
	dir := t.TempDir()
	release := HoldOpensForTests(dir)
	t.Cleanup(release)
	hung := stuckByMount.For(dir)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := StatBounded(ctx, dir, time.Minute); !errors.Is(err, context.Canceled) || GaveUp(err) {
		t.Errorf("a held call whose context ended = %v, want context.Canceled", err)
	}
	made := false
	if _, err := Bounded(ctx, dir, time.Minute, func() (int, error) { made = true; return 0, nil }, nil); !errors.Is(err, context.Canceled) || made {
		t.Errorf("a call on an ended context = %v (made %v), want it not made", err, made)
	}
	release()
	waitUncounted(t, hung)

	if _, err := Bounded(t.Context(), t.TempDir(), time.Minute, func() (int, error) { panic("boom") }, nil); err == nil || GaveUp(err) {
		t.Errorf("a call that panicked = %v, want its panic as the error", err)
	}
}

// A burst of Bounded calls on a share that just stopped answering (a page of
// covers at once) makes no more of them than the mount's cap, rather than
// every one, each counted when given up on: the rest wait for a slot within
// their time and are given up on unmade. Once the share answers, every slot
// is free again and calls there are made as ever. Not parallel:
// HoldOpensForTests swaps the mount lookup.
func TestBoundedBurstKeepsToTheCap(t *testing.T) {
	dir := t.TempDir()
	release := HoldOpensForTests(dir)
	t.Cleanup(release)
	hung := stuckByMount.For(dir)
	const burst = 3 * maxAbandonedReads
	var made atomic.Int32
	errs := make(chan error, burst)
	start := time.Now()
	for range burst {
		go func() {
			_, err := Bounded(t.Context(), dir, 100*time.Millisecond, func() (int, error) { made.Add(1); return 0, nil }, nil)
			errs <- err
		}()
	}
	for range burst {
		if err := <-errs; !GaveUp(err) {
			t.Errorf("a call in the burst = %v, want it given up on", err)
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the burst took %v, want each call given up on at its time", d)
	}
	if n := hung.n.Load(); n > maxAbandonedReads {
		t.Errorf("abandoned calls on the held mount = %d, want at most the cap (%d)", n, maxAbandonedReads)
	}
	release()
	waitUncounted(t, hung)
	if n := made.Load(); n > maxAbandonedReads {
		t.Errorf("calls made in the burst = %d, want at most the cap (%d)", n, maxAbandonedReads)
	}
	if len(hung.calls) != 0 {
		t.Errorf("slots held once the share answers = %d, want none", len(hung.calls))
	}
	if _, err := StatBounded(t.Context(), dir, time.Minute); err != nil {
		t.Errorf("a stat once the share answers = %v, want it made", err)
	}
}

// TestNamesListed: each name must be in its folder's listing byte for byte (a
// disk that ignores case finds "a" as "A" but lists only "A"); need limits the
// names looked at; a listing that fails reads as not there, one given up on is
// the error.
func TestNamesListed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Author", "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	list := func(ctx context.Context, dir string) ([]string, error) {
		return ReadDirNamesBounded(ctx, dir, time.Second)
	}
	for _, tc := range []struct {
		rel  string
		need func(string) bool
		want bool
	}{
		{"Author/Book", nil, true},
		{"author/Book", nil, false},
		{"Author/book", nil, false},
		{"Author/Gone", nil, false},
		{"Gone/Book", nil, false},
		{"Author/book", func(name string) bool { return name != "book" }, true},
	} {
		got, err := NamesListed(ctx, root, tc.rel, tc.need, list)
		if err != nil || got != tc.want {
			t.Errorf("NamesListed(%q) = %v %v, want %v", tc.rel, got, err, tc.want)
		}
	}
	refused := func(context.Context, string) ([]string, error) { return nil, ErrNotAnswered }
	if got, err := NamesListed(ctx, root, "Author/Book", nil, refused); got || !GaveUp(err) {
		t.Errorf("a listing given up on = %v %v, want false and the error", got, err)
	}
}

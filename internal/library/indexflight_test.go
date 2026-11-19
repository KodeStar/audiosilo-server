package library

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/testwait"
)

// TestIndexPathSharesOneRead: two requests that index the same book on demand at
// once (a book page asks for the item and its chapters together) share one read
// of it, a part's path and the book's own alike, and each gets the stored book.
func TestIndexPathSharesOneRead(t *testing.T) {
	t.Parallel()
	s, lib, ctx, count, hold := newProbeEnv(t, 0)

	// One read of a book like it, alone: the probes a read makes.
	if _, err := s.IndexPath(ctx, lib, "A/Twin"); err != nil {
		t.Fatal(err)
	}
	once := count()
	if once == 0 {
		t.Fatal("the stand-in ffprobe never ran")
	}

	// The read's ffprobe runs are held until the second request has joined
	// it, however long either took to resolve its path on a loaded run.
	release := hold()
	var wg sync.WaitGroup
	books := make([]*catalog.Book, 2)
	errs := make([]error, 2)
	for i, p := range []string{"A/Book", "A/Book/02.mp3"} {
		wg.Go(func() { books[i], errs[i] = s.IndexPath(ctx, lib, p) })
	}
	waitJoined(t, s, lib, "A/Book", 1)
	release()
	wg.Wait()
	for i := range books {
		if errs[i] != nil || books[i] == nil || books[i].RelPath != "A/Book" {
			t.Fatalf("request %d: %+v, %v", i, books[i], errs[i])
		}
	}
	if books[0].ID != books[1].ID {
		t.Errorf("two rows for one book: %d and %d", books[0].ID, books[1].ID)
	}
	if books[0] == books[1] {
		t.Error("the two requests share one *Book; each should get its own")
	}
	if got := count() - once; got != once {
		t.Errorf("the two requests probed %d times; one read probes %d", got, once)
	}
}

// TestIndexPathLeaderCancelled: when the request that began a shared read ends
// before the book is stored, it gets its context error, but the read goes on and
// is stored (its probes are kept, server#211): a request waiting on it gets the
// book, and nothing is read twice.
func TestIndexPathLeaderCancelled(t *testing.T) {
	t.Parallel()
	s, lib, ctx, count, hold := newProbeEnv(t, 0)
	if _, err := s.IndexPath(ctx, lib, "A/Twin"); err != nil {
		t.Fatal(err)
	}
	once := count()

	// The read's ffprobe runs are held until the leader has been cancelled, so
	// the waiter has joined a read still under way.
	release := hold()
	leadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type answer struct {
		b   *catalog.Book
		err error
	}
	lead := make(chan answer, 1)
	go func() {
		b, err := s.IndexPath(leadCtx, lib, "A/Book")
		lead <- answer{b, err}
	}()
	// The leader is reading: join it, then end the leader's request.
	waitFlight(t, s, lib, "A/Book", true)
	wait := make(chan answer, 1)
	go func() {
		b, err := s.IndexPath(ctx, lib, "A/Book/02.mp3")
		wait <- answer{b, err}
	}()
	waitJoined(t, s, lib, "A/Book", 1)
	cancel()

	if a := <-lead; !errors.Is(a.err, context.Canceled) {
		t.Errorf("leader: %+v, %v, want context.Canceled", a.b, a.err)
	}
	release()
	if a := <-wait; a.err != nil || a.b == nil || a.b.RelPath != "A/Book" {
		t.Errorf("waiter: %+v, %v", a.b, a.err)
	}
	if got := count() - once; got != once {
		t.Errorf("the book was probed %d times; one read probes %d", got, once)
	}
}

// TestIndexMovedSharesTheFlight: the Bridge's IndexMoved and an on-demand index
// of the same book never read it side by side (server#211): an on-demand index
// asked for while IndexMoved reads waits for its write, and IndexMoved asked for
// while an on-demand index reads waits for it to end.
func TestIndexMovedSharesTheFlight(t *testing.T) {
	t.Parallel()
	s, lib, ctx, count, hold := newProbeEnv(t, 0)
	if _, err := s.IndexPath(ctx, lib, "A/Twin"); err != nil {
		t.Fatal(err)
	}
	once := count()

	// IndexMoved's ffprobe runs are held until the on-demand index has joined
	// its read.
	release := hold()
	moved := make(chan error, 1)
	go func() {
		_, err := s.IndexMoved(ctx, lib, "A/Book", "")
		moved <- err
	}()
	waitFlight(t, s, lib, "A/Book", true)
	type answer struct {
		b   *catalog.Book
		err error
	}
	asked := make(chan answer, 1)
	go func() {
		b, err := s.IndexPath(ctx, lib, "A/Book/02.mp3")
		asked <- answer{b, err}
	}()
	waitJoined(t, s, lib, "A/Book", 1)
	release()
	if a := <-asked; a.err != nil || a.b == nil || a.b.RelPath != "A/Book" {
		t.Fatalf("on-demand index beside IndexMoved: %+v, %v", a.b, a.err)
	}
	if err := <-moved; err != nil {
		t.Fatal(err)
	}
	if got := count() - once; got != once {
		t.Errorf("the book was probed %d times; one read probes %d", got, once)
	}

	// The other way round: IndexMoved reads after the on-demand read ends.
	go func() {
		_, err := s.IndexPath(ctx, lib, "A/Twin/01.mp3")
		moved <- err
	}()
	waitFlight(t, s, lib, "A/Twin", true)
	if _, err := s.IndexMoved(ctx, lib, "A/Twin", ""); err != nil {
		t.Fatal(err)
	}
	if err := <-moved; err != nil {
		t.Fatal(err)
	}
}

// TestIndexPathAfterADroppedRead: an on-demand index waiting on a read that is
// given up (a scan that stopped, or failed to write the book: errReadDropped)
// reads the book itself, once, and gets it; one waiting on a read that failed
// gets that failure.
func TestIndexPathAfterADroppedRead(t *testing.T) {
	t.Parallel()
	s, lib, ctx, count := newSlowProbeEnv(t)
	if _, err := s.IndexPath(ctx, lib, "A/Twin"); err != nil {
		t.Fatal(err)
	}
	once := count()

	// A scan's read of the book under way (Scan takes it so), then dropped.
	f, err := s.startFlight(ctx, bookKey{lib.ID, "A/Book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		b   *catalog.Book
		err error
	}
	got := make(chan answer, 1)
	// The index has joined the flight before it ends: ending it before would
	// leave the index to lead a read of its own.
	index := func(rel string) {
		t.Helper()
		go func() {
			b, err := s.IndexPath(ctx, lib, rel)
			got <- answer{b, err}
		}()
		waitJoined(t, s, lib, rel, 1)
	}
	index("A/Book")
	if n := count(); n != once {
		t.Fatalf("the on-demand index read beside the scan's read: %d probes, want %d", n, once)
	}
	s.endFlight(f, errReadDropped)
	if a := <-got; a.err != nil || a.b == nil || a.b.RelPath != "A/Book" {
		t.Fatalf("on-demand index after the dropped read: %+v, %v", a.b, a.err)
	}
	if n := count() - once; n != once {
		t.Errorf("the book was probed %d times after the drop; one read probes %d", n, once)
	}

	// A read that failed is that failure for its waiters, not a read of their own.
	failed := errors.New("write failed")
	f, err = s.startFlight(ctx, bookKey{lib.ID, "A/Twin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	index("A/Twin")
	s.endFlight(f, failed)
	if a := <-got; !errors.Is(a.err, failed) {
		t.Fatalf("waiter on a failed read: %+v, %v, want the failure", a.b, a.err)
	}
}

// TestReindexPathReadsAfresh: an explicit re-read asked for while another read
// of the book is under way (a scan's, held until its write) waits for it, then
// reads the book itself: what the earlier read stored may be older than the
// change the re-read was asked for.
func TestReindexPathReadsAfresh(t *testing.T) {
	t.Parallel()
	s, lib, ctx, count := newSlowProbeEnv(t)
	if _, err := s.IndexPath(ctx, lib, "A/Book"); err != nil {
		t.Fatal(err)
	}
	once := count()
	f, err := s.startFlight(ctx, bookKey{lib.ID, "A/Book"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		_, err := s.ReindexPath(ctx, lib, "A/Book")
		got <- err
	}()
	waitJoined(t, s, lib, "A/Book", 1) // the re-read waits on the flight
	if n := count(); n != once {
		t.Fatalf("the re-read read beside the read under way: %d probes, want %d", n, once)
	}
	s.endFlight(f, nil) // that read's book is written
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if n := count() - once; n != once {
		t.Fatalf("the re-read probed %d times; a read of its own probes %d", n, once)
	}
}

// TestIndexPathEndsWithTheServer: a read that outlives its request still ends
// with the server (SetBaseContext): stopped, it writes nothing.
func TestIndexPathEndsWithTheServer(t *testing.T) {
	t.Parallel()
	s, lib, ctx, count := newSlowProbeEnv(t)
	base, stop := context.WithCancel(ctx)
	s.SetBaseContext(base)
	reqCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := s.IndexPath(reqCtx, lib, "A/Book")
		done <- err
	}()
	waitFlight(t, s, lib, "A/Book", true)
	for deadline := time.Now().Add(10 * time.Second); count() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stand-in ffprobe never ran")
		}
	}
	cancel() // the request goes; the read goes on
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("IndexPath: %v, want context.Canceled", err)
	}
	stop() // then the server stops
	waitFlight(t, s, lib, "A/Book", false)
	if _, err := s.cat.GetBookByPath(ctx, lib.ID, "A/Book"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("a read the server stopped was written: %v", err)
	}
	// A request still live once the server has stopped gets the stop, rather
	// than beginning read after read until it ends.
	late, cancelLate := context.WithTimeout(ctx, 5*time.Second)
	defer cancelLate()
	if _, err := s.IndexPath(late, lib, "A/Book"); !errors.Is(err, context.Canceled) {
		t.Fatalf("IndexPath after the stop: %v, want context.Canceled", err)
	}
}

// TestScanSharesTheFlight: a scan and an on-demand index of the same book never
// read it side by side (server#211). An on-demand index asked for while the scan
// reads the book waits for the scan's write and loads it; a scan reaching a book
// an on-demand index is reading waits for that read to end.
func TestScanSharesTheFlight(t *testing.T) {
	t.Parallel()
	t.Run("on demand during the scan", func(t *testing.T) {
		t.Parallel()
		s, lib, ctx, count, hold := newProbeEnv(t, 0)
		// The scan's ffprobe runs are held until the on-demand index has
		// joined its read of A/Book.
		release := hold()
		scanned := make(chan error, 1)
		go func() {
			_, err := s.Scan(ctx, lib)
			scanned <- err
		}()
		waitFlight(t, s, lib, "A/Book", true)
		type answer struct {
			b   *catalog.Book
			err error
		}
		asked := make(chan answer, 1)
		go func() {
			b, err := s.IndexPath(ctx, lib, "A/Book")
			asked <- answer{b, err}
		}()
		waitJoined(t, s, lib, "A/Book", 1)
		release()
		if a := <-asked; a.err != nil || a.b == nil || a.b.RelPath != "A/Book" {
			t.Fatalf("on-demand index during the scan: %+v, %v", a.b, a.err)
		}
		if err := <-scanned; err != nil {
			t.Fatal(err)
		}
		// The scan reads both books; the on-demand index read nothing of its own.
		// Counted once the scan has ended, as its read of A/Twin may still be
		// starting probes when A/Book's write ends the on-demand wait.
		before := count()
		if _, err := s.IndexPath(ctx, lib, "A/Twin"); err != nil {
			t.Fatal(err)
		}
		if after := count(); 3*(after-before) != after {
			t.Errorf("probes: %d, of which the last read of a book took %d; the scan's two books and one more read should be 3 reads", after, after-before)
		}
	})
	t.Run("scan during an on-demand index", func(t *testing.T) {
		t.Parallel()
		s, lib, ctx, _ := newSlowProbeEnv(t)
		read := make(chan error, 1)
		go func() {
			_, err := s.IndexPath(ctx, lib, "A/Book")
			read <- err
		}()
		waitFlight(t, s, lib, "A/Book", true)
		if _, err := s.Scan(ctx, lib); err != nil {
			t.Fatal(err)
		}
		if err := <-read; err != nil {
			t.Fatal(err)
		}
	})
}

// waitFlight waits until a read of the book at rel is under way (underway) or
// until none is.
func waitFlight(t *testing.T, s *Scanner, lib catalog.Library, rel string, underway bool) {
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("a read of %s under way to be %v", rel, underway), 20*time.Second, func() bool {
		return (flightOf(s, lib, rel) != nil) == underway
	})
}

// waitJoined waits until n callers have joined the read of the book at rel
// under way (indexFlight.joined).
func waitJoined(t *testing.T, s *Scanner, lib catalog.Library, rel string, n int32) {
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("%d callers to join the read of %s", n, rel), 20*time.Second, func() bool {
		f := flightOf(s, lib, rel)
		return f != nil && f.joined.Load() >= n
	})
}

// flightOf is the read of the book at rel under way, or nil.
func flightOf(s *Scanner, lib catalog.Library, rel string) *indexFlight {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	return s.flights[bookKey{lib.ID, rel}]
}

// newSlowProbeEnv is a scanner whose stand-in ffprobe takes half a second and
// notes each run (count), so on-demand indexes overlap, over a library holding
// two two-file books, A/Book and A/Twin. The test fails when two runs probed
// one file at once.
func newSlowProbeEnv(t *testing.T) (*Scanner, catalog.Library, context.Context, func() int) {
	t.Helper()
	return newSlowProbeEnvOf(t, 500*time.Millisecond)
}

// newSlowProbeEnvOf is newSlowProbeEnv with each ffprobe run taking took.
func newSlowProbeEnvOf(t *testing.T, took time.Duration) (*Scanner, catalog.Library, context.Context, func() int) {
	t.Helper()
	s, lib, ctx, count, _ := newProbeEnv(t, took)
	return s, lib, ctx, count
}

// newProbeEnv is newSlowProbeEnvOf's scanner, whose ffprobe runs also wait
// while the test holds them: hold holds every run from then on, and its
// release (idempotent, and run at the test's end) lets them end. A test then
// proves what overlaps by holding the read until it has, not by its timing.
func newProbeEnv(t *testing.T, took time.Duration) (*Scanner, catalog.Library, context.Context, func() int, func() (release func())) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in ffprobe is a shell script")
	}
	cat, _, ctx := newScanEnv(t)
	root := t.TempDir()
	for _, f := range []string{"A/Book/01.mp3", "A/Book/02.mp3", "A/Twin/01.mp3", "A/Twin/02.mp3"} {
		writeFile(t, root, f, []byte("x"))
	}
	runs := filepath.Join(t.TempDir(), "runs")
	locks := t.TempDir()
	gate := filepath.Join(t.TempDir(), "gate")
	probe := filepath.Join(t.TempDir(), "ffprobe")
	// A lock per file probed (its path is the last argument): a second run on it
	// while one holds it notes the overlap.
	script := "#!/bin/sh\nfor a; do f=$a; done\nl='" + locks + "'/$(printf %s \"$f\" | cksum | cut -d' ' -f1)\n" +
		"mkdir \"$l\" 2>/dev/null || echo overlap >> '" + runs + "'\n" +
		"echo run >> '" + runs + "'\n"
	if took > 0 {
		script += "sleep " + strconv.FormatFloat(took.Seconds(), 'f', 3, 64) + "\n"
	}
	script += "while [ -e '" + gate + "' ]; do sleep 0.01; done\nrmdir \"$l\" 2>/dev/null\necho '{}'\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata.ForceFFprobeForTests(probe)
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		b, err := os.ReadFile(runs)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return bytes.Count(b, []byte("run\n"))
	}
	t.Cleanup(func() {
		if b, _ := os.ReadFile(runs); bytes.Contains(b, []byte("overlap\n")) {
			t.Error("one file was probed by two reads at once")
		}
	})
	hold := func() func() {
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		release := sync.OnceFunc(func() {
			if err := os.Remove(gate); err != nil {
				t.Error(err)
			}
		})
		t.Cleanup(release)
		return release
	}
	return NewScanner(cat, probe, nil), *lib, ctx, count, hold
}

// TestWaitFlightBoundsOnlyTheRead: a wait on another's read gives up once a
// look's time has passed with the read not returned, but a scan's read that
// has returned is waited on until the scan writes its book (its batch write
// can come later than a look's time), and one a stopped scan left is given up
// on at once.
func TestWaitFlightBoundsOnlyTheRead(t *testing.T) {
	t.Parallel()
	s := &Scanner{probeTimeout: 50 * time.Millisecond}
	flight := func(read bool) *indexFlight {
		r := &bookRead{done: make(chan struct{})}
		if read {
			close(r.done)
		}
		return &indexFlight{done: make(chan struct{}), scan: r, progress: metadata.NewProgress()}
	}

	if err := s.waitFlight(t.Context(), flight(false)); !errors.Is(err, errReadLeft) {
		t.Errorf("a read not returned: %v, want errReadLeft", err)
	}
	if err := s.waitFlight(t.Context(), &indexFlight{done: make(chan struct{}), progress: metadata.NewProgress()}); !errors.Is(err, errReadLeft) {
		t.Errorf("an on-demand read not returned: %v, want errReadLeft", err)
	}
	left := flight(false)
	left.scan.left.Store(true)
	began := time.Now()
	if err := s.waitFlight(t.Context(), left); !errors.Is(err, errReadLeft) || time.Since(began) >= s.probeTimeout {
		t.Errorf("a read a stopped scan left: %v after %v, want errReadLeft at once", err, time.Since(began))
	}

	written := flight(true)
	time.AfterFunc(4*s.probeTimeout, func() { close(written.done) })
	if err := s.waitFlight(t.Context(), written); err != nil {
		t.Errorf("a scan's read waiting for its write: %v, want it waited for", err)
	}

	// One marked stuck before it returned (it waited that long for a read
	// slot) is waited on for its write all the same.
	late := flight(true)
	late.stuck.Store(true)
	time.AfterFunc(2*s.probeTimeout, func() { close(late.done) })
	if err := s.waitFlight(t.Context(), late); err != nil {
		t.Errorf("a scan's read marked stuck, then returned: %v, want it waited for", err)
	}
}

// TestWaitFlightWaitsOnAReadThatMovesOn: a wait on another's read gives up on
// one that has not moved on for a look's time, not on one that is slow: a read
// whose calls keep returning (a big book on a slow share that answers) is
// waited on past a look's time until it ends (server#360). One that then stops
// moving on for the probe's time is given up on and marked stuck, so a later
// waiter gives up on it at once.
func TestWaitFlightWaitsOnAReadThatMovesOn(t *testing.T) {
	t.Parallel()
	// A look's time well past the read's progress tick, so a loaded test
	// machine's late tick is still progress within it.
	s := &Scanner{probeTimeout: 250 * time.Millisecond}
	f := &indexFlight{done: make(chan struct{}), progress: metadata.NewProgress()}
	stop := make(chan struct{})
	go func() { // the read: progress every 20 ms, for five looks' time
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for end := time.After(5 * s.probeTimeout); ; {
			select {
			case <-tick.C:
				f.progress.Note()
			case <-end:
				close(f.done)
				return
			case <-stop:
				return
			}
		}
	}()
	began := time.Now()
	if err := s.waitFlight(t.Context(), f); err != nil || time.Since(began) < 4*s.probeTimeout {
		t.Errorf("a slow read moving on: %v after %v, want it waited for until it ended", err, time.Since(began))
	}

	stalled := &indexFlight{done: make(chan struct{}), progress: metadata.NewProgress()}
	if err := s.waitFlight(t.Context(), stalled); !errors.Is(err, errReadLeft) || !stalled.stuck.Load() {
		t.Errorf("a read that stopped moving on: %v, stuck %v; want errReadLeft, marked stuck", err, stalled.stuck.Load())
	}
	stalled.progress.Note() // it moves on again: a waiter already told it is stuck
	began = time.Now()
	if err := s.waitFlight(t.Context(), stalled); !errors.Is(err, errReadLeft) || time.Since(began) >= s.probeTimeout {
		t.Errorf("a read marked stuck: %v after %v, want errReadLeft at once", err, time.Since(began))
	}
	close(stop)
}

// TestWaitFlightGivesARunningProbeItsTime: ffprobe reports nothing until it
// returns, so a request's wait on a read that is running one is not given up
// on after its own short look's time (a healthy probe of a big file on a slow
// share would be a false 503 share_not_answering): the probe gets its own
// time, from the read's last progress, and the read is given up on once that
// has passed (server#360).
func TestWaitFlightGivesARunningProbeItsTime(t *testing.T) {
	t.Parallel()
	// The probe's time well past the run's, so a loaded test machine's late
	// timer is still within it.
	s := &Scanner{probeTimeout: 1500 * time.Millisecond}
	asked := WithLookTimeout(t.Context(), 30*time.Millisecond)

	probed := &indexFlight{done: make(chan struct{}), progress: metadata.NewProgress()}
	done := probed.progress.Probe()
	time.AfterFunc(200*time.Millisecond, func() { // well past the look's time
		done()
		close(probed.done)
	})
	if err := s.waitFlight(asked, probed); err != nil {
		t.Errorf("a read running ffprobe past a look's time: %v, want it waited for", err)
	}

	// The same wait on a read that runs no probe is a look's.
	idle := &indexFlight{done: make(chan struct{}), progress: metadata.NewProgress()}
	began := time.Now()
	if err := s.waitFlight(asked, idle); !errors.Is(err, errReadLeft) || time.Since(began) >= s.probeTimeout {
		t.Errorf("a read not moving on, no probe running: %v after %v, want errReadLeft after a look's time", err, time.Since(began))
	}

	// A probe that outlasts its time is given up on, and its read marked stuck.
	hung := &indexFlight{done: make(chan struct{}), progress: metadata.NewProgress()}
	defer hung.progress.Probe()()
	began = time.Now()
	if err := s.waitFlight(asked, hung); !errors.Is(err, errReadLeft) || time.Since(began) < s.probeTimeout || !hung.stuck.Load() {
		t.Errorf("a probe past its time: %v after %v, stuck %v; want errReadLeft once the probe's time has passed, marked stuck", err, time.Since(began), hung.stuck.Load())
	}
}

// TestWaitFlightCapsARequestsWait: a request (an on-demand read given a look
// time) waits on a read that keeps moving on for the probe's time in all, then
// gives up on it as a look not answered, the read not marked stuck: one that
// trickles on never holds a request for its whole time. A scan's wait has no
// such cap (TestWaitFlightWaitsOnAReadThatMovesOn).
func TestWaitFlightCapsARequestsWait(t *testing.T) {
	t.Parallel()
	s := &Scanner{probeTimeout: 500 * time.Millisecond}
	f := &indexFlight{done: make(chan struct{}), progress: metadata.NewProgress()}
	stop := make(chan struct{})
	defer close(stop)
	go func() { // the read: progress every 20 ms, for six times the probe's time
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for end := time.After(6 * s.probeTimeout); ; {
			select {
			case <-tick.C:
				f.progress.Note()
			case <-end:
				close(f.done)
				return
			case <-stop:
				return
			}
		}
	}()
	began := time.Now()
	// A look's time well past the progress tick (a late one on a loaded test
	// machine is still progress within it).
	err := s.waitFlight(WithLookTimeout(t.Context(), 250*time.Millisecond), f)
	if took := time.Since(began); !errors.Is(err, errReadLeft) || took < s.probeTimeout || took >= 5*s.probeTimeout {
		t.Errorf("a request's wait on a read that trickles on: %v after %v, want errReadLeft at the probe's time", err, took)
	}
	if f.stuck.Load() {
		t.Error("a read still moving on was marked stuck")
	}
}

// TestWaitFlightEndsAtTheRequestsWaitCap: a request whose waits are capped
// ahead of its own timeout (WithWaitCap, the API's flightWaitEnd) gives up on
// a read that trickles on, or one running ffprobe past the cap, at the cap,
// not the probe's time: its 503 share_not_answering with Retry-After is then
// written before the request timeout's bare 503. A cap past the probe's time
// changes nothing, and a scan's wait (no look time) stays uncapped.
func TestWaitFlightEndsAtTheRequestsWaitCap(t *testing.T) {
	t.Parallel()
	s := &Scanner{probeTimeout: 3 * time.Second}
	capped := func(d time.Duration) context.Context {
		return WithWaitCap(WithLookTimeout(t.Context(), 100*time.Millisecond), time.Now().Add(d))
	}
	if d, ok := s.flightWaitCap(capped(400 * time.Millisecond)); !ok || d <= 0 || d > 400*time.Millisecond {
		t.Errorf("flightWaitCap with a cap under the probe's time = %v, %v; want the time left to it", d, ok)
	}
	if d, ok := s.flightWaitCap(capped(-time.Second)); !ok || d != 0 {
		t.Errorf("flightWaitCap with a cap passed = %v, %v; want none left", d, ok)
	}
	if d, ok := s.flightWaitCap(capped(time.Minute)); !ok || d != s.probeTimeout {
		t.Errorf("flightWaitCap with a cap past the probe's time = %v, %v; want the probe's time", d, ok)
	}
	if _, ok := s.flightWaitCap(WithWaitCap(t.Context(), time.Now().Add(time.Second))); ok {
		t.Error("a scan's wait (no look time) is capped")
	}

	stop := make(chan struct{})
	defer close(stop)
	trickling := &indexFlight{done: make(chan struct{}), progress: metadata.NewProgress()}
	go func() { // progress every 20 ms until the test ends
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				trickling.progress.Note()
			case <-stop:
				return
			}
		}
	}()
	probing := &indexFlight{done: make(chan struct{}), progress: metadata.NewProgress()}
	defer probing.progress.Probe()()
	for _, c := range []struct {
		name string
		f    *indexFlight
	}{{"a read that trickles on", trickling}, {"a read running ffprobe", probing}} {
		began := time.Now()
		err := s.waitFlight(capped(400*time.Millisecond), c.f)
		if took := time.Since(began); !errors.Is(err, errReadLeft) || took < 400*time.Millisecond || took >= s.probeTimeout {
			t.Errorf("%s, the wait capped at 400ms: %v after %v, want errReadLeft at the cap", c.name, err, took)
		}
		if c.f.stuck.Load() {
			t.Errorf("%s: marked stuck at the cap, before the probe's time", c.name)
		}
	}
}

// TestRequestWaitsOutAnotherReadsProbe: a request joining a read of its book
// whose ffprobe runs take longer than the request's look time gets the book
// once the read ends, not a look given up on: the runs are marked on the
// read's progress (metadata.Progress.Probe), each given the probe's own time.
//
// The look time must outlast the read's steps between its ffprobe runs, which
// a loaded -race run can stretch past a fixed quarter second, and each run
// must outlast the look time: both are scaled to the build.
func TestRequestWaitsOutAnotherReadsProbe(t *testing.T) {
	t.Parallel()
	look := testwait.Scaled(500 * time.Millisecond)
	s, lib, ctx, _ := newSlowProbeEnvOf(t, 2*look) // each ffprobe run takes twice the look time
	read := make(chan error, 1)
	go func() {
		_, err := s.IndexPath(ctx, lib, "A/Book")
		read <- err
	}()
	waitFlight(t, s, lib, "A/Book", true)
	b, err := s.IndexPath(WithLookTimeout(ctx, look), lib, "A/Book")
	if err != nil || b == nil || b.RelPath != "A/Book" {
		t.Errorf("a request joining a read that is running ffprobe: %+v, %v; want the book it stored", b, err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
}

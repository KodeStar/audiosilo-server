package library

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// TestIndexPathSharesOneRead: two requests that index the same book on demand at
// once (a book page asks for the item and its chapters together) share one read
// of it, a part's path and the book's own alike, and each gets the stored book.
func TestIndexPathSharesOneRead(t *testing.T) {
	t.Parallel()
	s, lib, ctx, count := newSlowProbeEnv(t)

	// One read of a book like it, alone: the probes a read makes.
	if _, err := s.IndexPath(ctx, lib, "A/Twin"); err != nil {
		t.Fatal(err)
	}
	once := count()
	if once == 0 {
		t.Fatal("the stand-in ffprobe never ran")
	}

	var wg sync.WaitGroup
	books := make([]*catalog.Book, 2)
	errs := make([]error, 2)
	for i, p := range []string{"A/Book", "A/Book/02.mp3"} {
		wg.Go(func() { books[i], errs[i] = s.IndexPath(ctx, lib, p) })
	}
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
	s, lib, ctx, count := newSlowProbeEnv(t)
	if _, err := s.IndexPath(ctx, lib, "A/Twin"); err != nil {
		t.Fatal(err)
	}
	once := count()

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
	// The leader is probing: join it, then end the leader's request.
	waitFlight(t, s, lib, "A/Book", true)
	wait := make(chan answer, 1)
	go func() {
		b, err := s.IndexPath(ctx, lib, "A/Book/02.mp3")
		wait <- answer{b, err}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()

	if a := <-lead; !errors.Is(a.err, context.Canceled) {
		t.Errorf("leader: %+v, %v, want context.Canceled", a.b, a.err)
	}
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
	s, lib, ctx, count := newSlowProbeEnv(t)
	if _, err := s.IndexPath(ctx, lib, "A/Twin"); err != nil {
		t.Fatal(err)
	}
	once := count()

	moved := make(chan error, 1)
	go func() {
		_, err := s.IndexMoved(ctx, lib, "A/Book", "")
		moved <- err
	}()
	waitFlight(t, s, lib, "A/Book", true)
	b, err := s.IndexPath(ctx, lib, "A/Book/02.mp3")
	if err != nil || b == nil || b.RelPath != "A/Book" {
		t.Fatalf("on-demand index beside IndexMoved: %+v, %v", b, err)
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
	f, err := s.startFlight(ctx, bookKey{lib.ID, "A/Book"})
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		b   *catalog.Book
		err error
	}
	got := make(chan answer, 1)
	go func() {
		b, err := s.IndexPath(ctx, lib, "A/Book")
		got <- answer{b, err}
	}()
	time.Sleep(100 * time.Millisecond) // the on-demand index waits on the flight
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
	f, err = s.startFlight(ctx, bookKey{lib.ID, "A/Twin"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		b, err := s.IndexPath(ctx, lib, "A/Twin")
		got <- answer{b, err}
	}()
	time.Sleep(100 * time.Millisecond)
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
	f, err := s.startFlight(ctx, bookKey{lib.ID, "A/Book"})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		_, err := s.ReindexPath(ctx, lib, "A/Book")
		got <- err
	}()
	time.Sleep(100 * time.Millisecond) // the re-read waits on the flight
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
		s, lib, ctx, count := newSlowProbeEnv(t)
		scanned := make(chan error, 1)
		go func() {
			_, err := s.Scan(ctx, lib)
			scanned <- err
		}()
		waitFlight(t, s, lib, "A/Book", true)
		b, err := s.IndexPath(ctx, lib, "A/Book")
		if err != nil || b == nil || b.RelPath != "A/Book" {
			t.Fatalf("on-demand index during the scan: %+v, %v", b, err)
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
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s.flightMu.Lock()
		_, ok := s.flights[bookKey{lib.ID, rel}]
		s.flightMu.Unlock()
		if ok == underway {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for a read of %s under way to be %v", rel, underway)
		}
	}
}

// newSlowProbeEnv is a scanner whose stand-in ffprobe takes half a second and
// notes each run (count), so on-demand indexes overlap, over a library holding
// two two-file books, A/Book and A/Twin. The test fails when two runs probed
// one file at once.
func newSlowProbeEnv(t *testing.T) (*Scanner, catalog.Library, context.Context, func() int) {
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
	probe := filepath.Join(t.TempDir(), "ffprobe")
	// A lock per file probed (its path is the last argument): a second run on it
	// while one holds it notes the overlap.
	script := "#!/bin/sh\nfor a; do f=$a; done\nl='" + locks + "'/$(printf %s \"$f\" | cksum | cut -d' ' -f1)\n" +
		"mkdir \"$l\" 2>/dev/null || echo overlap >> '" + runs + "'\n" +
		"echo run >> '" + runs + "'\nsleep 0.5\nrmdir \"$l\" 2>/dev/null\necho '{}'\n"
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
	return NewScanner(cat, probe, nil), *lib, ctx, count
}

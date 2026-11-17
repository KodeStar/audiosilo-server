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
	waitFlight(t, s, lib, "A/Book")
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
	waitFlight(t, s, lib, "A/Book")
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
	waitFlight(t, s, lib, "A/Twin")
	if _, err := s.IndexMoved(ctx, lib, "A/Twin", ""); err != nil {
		t.Fatal(err)
	}
	if err := <-moved; err != nil {
		t.Fatal(err)
	}
	noOverlap(t, count)
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
		waitFlight(t, s, lib, "A/Book")
		b, err := s.IndexPath(ctx, lib, "A/Book")
		if err != nil || b == nil || b.RelPath != "A/Book" {
			t.Fatalf("on-demand index during the scan: %+v, %v", b, err)
		}
		before := count()
		if err := <-scanned; err != nil {
			t.Fatal(err)
		}
		// The scan reads both books; the on-demand index read nothing of its own.
		if _, err := s.IndexPath(ctx, lib, "A/Twin"); err != nil {
			t.Fatal(err)
		}
		if after := count(); 3*(after-before) != after {
			t.Errorf("probes: %d, of which the last read of a book took %d; the scan's two books and one more read should be 3 reads", after, after-before)
		}
		noOverlap(t, count)
	})
	t.Run("scan during an on-demand index", func(t *testing.T) {
		t.Parallel()
		s, lib, ctx, count := newSlowProbeEnv(t)
		read := make(chan error, 1)
		go func() {
			_, err := s.IndexPath(ctx, lib, "A/Book")
			read <- err
		}()
		waitFlight(t, s, lib, "A/Book")
		if _, err := s.Scan(ctx, lib); err != nil {
			t.Fatal(err)
		}
		if err := <-read; err != nil {
			t.Fatal(err)
		}
		noOverlap(t, count)
	})
}

// waitFlight waits until a read of the book at rel is under way.
func waitFlight(t *testing.T, s *Scanner, lib catalog.Library, rel string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s.flightMu.Lock()
		_, ok := s.flights[bookKey{lib.ID, rel}]
		s.flightMu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no read of %s began", rel)
		}
	}
}

// waitNoFlight waits until no read of the book at rel is under way.
func waitNoFlight(t *testing.T, s *Scanner, lib catalog.Library, rel string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s.flightMu.Lock()
		_, ok := s.flights[bookKey{lib.ID, rel}]
		s.flightMu.Unlock()
		if !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the read of %s never ended", rel)
		}
	}
}

// noOverlap fails when the stand-in ffprobe ran twice on one file at once.
func noOverlap(t *testing.T, count func() int) {
	t.Helper()
	if count() < 0 {
		t.Error("one file was probed by two reads at once")
	}
}

// newSlowProbeEnv is a scanner whose stand-in ffprobe takes half a second and
// notes each run (count), so on-demand indexes overlap, over a library holding
// two two-file books, A/Book and A/Twin. count is negative once two runs probed
// one file at once (noOverlap).
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
		if bytes.Contains(b, []byte("overlap\n")) {
			return -1
		}
		return bytes.Count(b, []byte("run\n"))
	}
	return NewScanner(cat, probe, nil), *lib, ctx, count
}

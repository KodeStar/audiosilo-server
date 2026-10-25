package library

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// TestIndexPathSharesOneRead: two requests that index the same book on demand at
// once (a book page asks for the item and its chapters together) share one read
// of it, a part's path and the book's own alike, and each gets the stored book.
func TestIndexPathSharesOneRead(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in ffprobe is a shell script")
	}
	cat, _, ctx := newScanEnv(t)
	root := t.TempDir()
	for _, f := range []string{"A/Book/01.mp3", "A/Book/02.mp3", "A/Twin/01.mp3", "A/Twin/02.mp3"} {
		writeFile(t, root, f, []byte("x"))
	}
	// A slow ffprobe that notes each run, so the two requests overlap.
	runs := filepath.Join(t.TempDir(), "runs")
	probe := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\necho run >> '" + runs + "'\nsleep 0.5\necho '{}'\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewScanner(cat, probe, nil)
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

	// One read of a book like it, alone: the probes a read makes.
	if _, err := s.IndexPath(ctx, *lib, "A/Twin"); err != nil {
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
		wg.Go(func() { books[i], errs[i] = s.IndexPath(ctx, *lib, p) })
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

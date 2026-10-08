package covercolors

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// fakeColorer answers a book by its path: a colour for the paths in colors,
// an error for those in failing, ErrArtMissing for those in missing, else no
// colour (a zero record). It counts the calls per path.
type fakeColorer struct {
	mu      sync.Mutex
	colors  map[string]string
	failing map[string]bool
	missing map[string]bool
	calls   map[string]int
}

func (f *fakeColorer) color(_ context.Context, lib *catalog.Library, d catalog.CoverColorDue) (catalog.CoverColorRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[d.Path]++
	rec := catalog.CoverColorRecord{LibraryID: lib.ID, Path: d.Path, Art: d.Source.Art}
	if f.failing[d.Path] {
		return rec, errors.New("unreadable")
	}
	if f.missing[d.Path] {
		return catalog.CoverColorRecord{}, ErrArtMissing
	}
	rec.Color.Bg = f.colors[d.Path]
	return rec, nil
}

func (f *fakeColorer) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

type env struct {
	cat *catalog.Catalog
	lib *catalog.Library
	fc  *fakeColorer
	r   *Runner
}

func newEnv(t *testing.T, paths ...string) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "audiosilo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cat := catalog.New(db, time.Now)
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	for _, p := range paths {
		if _, err := cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: p, Title: p,
			HasCover: &yes, AddedAt: "2020-01-01T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	fc := &fakeColorer{colors: map[string]string{}, failing: map[string]bool{}, missing: map[string]bool{},
		calls: map[string]int{}}
	return &env{cat: cat, lib: lib, fc: fc, r: New(cat, fc.color, nil)}
}

func (e *env) colour(t *testing.T, path string) string {
	t.Helper()
	b, err := e.cat.GetBookByPath(context.Background(), e.lib.ID, path)
	if err != nil {
		t.Fatal(err)
	}
	if b.CoverColor == nil {
		return ""
	}
	return b.CoverColor.Bg
}

// TestPass: a pass records the colours it reads; a book with no colour to read
// is recorded as such and not read again while its art is the same, by this
// runner or a new one (a restart), and is once its art changes; a book whose art
// failed to read, or whose art files are missing, is tried again next pass.
func TestPass(t *testing.T) {
	e := newEnv(t, "a.m4b", "none.m4b", "broken.m4b", "gone.m4b")
	e.fc.colors["a.m4b"] = "#112233"
	e.fc.failing["broken.m4b"] = true
	e.fc.missing["gone.m4b"] = true
	ctx := context.Background()
	if e.r.pass(ctx) {
		t.Fatal("pass reported failing on one bad book")
	}
	if got := e.colour(t, "a.m4b"); got != "#112233" {
		t.Fatalf("a.m4b colour = %q, want #112233", got)
	}
	New(e.cat, e.fc.color, nil).pass(ctx)
	for path, want := range map[string]int{"a.m4b": 1, "none.m4b": 1, "broken.m4b": 2, "gone.m4b": 2} {
		if got := e.fc.count(path); got != want {
			t.Errorf("%s read %d times over two passes, want %d", path, got, want)
		}
	}
	// New art: the book is read again.
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	if err := e.cat.SetCover(ctx, e.lib.ID, "none.m4b", png, 0, catalog.SourceEdited); err != nil {
		t.Fatal(err)
	}
	e.r.pass(ctx)
	if got := e.fc.count("none.m4b"); got != 2 {
		t.Fatalf("none.m4b read %d times after its cover changed, want 2", got)
	}
}

// TestPassPages: a pass reads every due book, across the index's pages.
func TestPassPages(t *testing.T) {
	var paths []string
	for i := range batch*2 + 5 {
		paths = append(paths, fmt.Sprintf("b%03d.m4b", i))
	}
	e := newEnv(t, paths...)
	for _, p := range paths {
		e.fc.colors[p] = "#000000"
	}
	e.r.pass(context.Background())
	for _, p := range paths {
		if e.fc.count(p) != 1 || e.colour(t, p) == "" {
			t.Fatalf("%s: read %d times, colour %q; want read once and coloured", p, e.fc.count(p), e.colour(t, p))
		}
	}
}

// TestPassStopsOnFailures: art that keeps failing to read ends the pass early,
// reported, rather than reading the whole library.
func TestPassStopsOnFailures(t *testing.T) {
	var paths []string
	for i := range 40 {
		p := fmt.Sprintf("b%02d.m4b", i)
		paths = append(paths, p)
	}
	e := newEnv(t, paths...)
	for _, p := range paths {
		e.fc.failing[p] = true
	}
	if !e.r.pass(context.Background()) {
		t.Fatal("pass did not report the failures")
	}
	total := 0
	for _, p := range paths {
		total += e.fc.count(p)
	}
	if total != maxFailStreak {
		t.Fatalf("read %d books before stopping, want %d", total, maxFailStreak)
	}
}

// TestPassMissingArtIsNoFailure: books whose art files are missing (an unmounted
// share) don't end the pass, so the books after them are still read.
func TestPassMissingArtIsNoFailure(t *testing.T) {
	var paths []string
	for i := range maxFailStreak * 2 {
		p := fmt.Sprintf("b%02d.m4b", i)
		paths = append(paths, p)
	}
	paths = append(paths, "last.m4b")
	e := newEnv(t, paths...)
	for _, p := range paths[:len(paths)-1] {
		e.fc.missing[p] = true
	}
	e.fc.colors["last.m4b"] = "#445566"
	if e.r.pass(context.Background()) {
		t.Fatal("pass reported failing on missing art")
	}
	if got := e.colour(t, "last.m4b"); got != "#445566" {
		t.Fatalf("last.m4b colour = %q, want #445566", got)
	}
}

// TestRunKick: Run's first pass comes once the start settles; kicks hold the
// next pass back until they stop coming (a scan), then bring it.
func TestRunKick(t *testing.T) {
	e := newEnv(t, "a.m4b")
	e.fc.failing["a.m4b"] = true // read again on every pass
	e.r.settle = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.r.Run(ctx)
		close(done)
	}()
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for e.fc.count("a.m4b") < n {
			if time.Now().After(deadline) {
				t.Fatalf("waited for pass %d; %d so far", n, e.fc.count("a.m4b"))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor(1)
	// A burst of kicks, each sooner than the settle: no pass until it ends.
	for range 6 {
		e.r.Kick()
		time.Sleep(20 * time.Millisecond)
	}
	if got := e.fc.count("a.m4b"); got != 1 {
		t.Fatalf("a pass ran during the burst of kicks (%d passes)", got)
	}
	waitFor(2)
	cancel()
	<-done
}

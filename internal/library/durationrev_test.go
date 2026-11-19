package library

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// headerlessSecs is the length ffmpeg decodes lame-vbr-noxing.mp3 to (metadata's
// countedFixtures).
const headerlessSecs = 5.328980

// headerlessBook is a library holding one book, a header-less VBR MP3 (lame
// -t), and a scanner with no ffprobe, not yet scanned.
func headerlessBook(t *testing.T) (*store.DB, *catalog.Catalog, *catalog.Library, *Scanner) {
	t.Helper()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	copyFixture(t, filepath.Join("..", "metadata", "testdata", "probe", "lame-vbr-noxing.mp3"), filepath.Join(root, "Author", "Book.mp3"))
	lib, err := cat.CreateLibrary(t.Context(), catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return db, cat, lib, NewScanner(cat, "", slog.Default())
}

// bookDuration is the one book's stored duration.
func bookDuration(t *testing.T, db *store.DB) float64 {
	t.Helper()
	var d float64
	if err := db.QueryRowContext(t.Context(), `SELECT duration FROM books`).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// A book holding an MP3 that an older server indexed (duration_rev 0, its VBR
// file's length a bitrate's estimate) is indexed once more by the next scan,
// its files unchanged, so the frames are counted (SRV-16); the scan after
// leaves it be.
func TestScanRereadsLengthsOfAnOlderRevision(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, _, lib, s := headerlessBook(t)
	if res, err := s.Scan(ctx, *lib); err != nil || res.Added != 1 {
		t.Fatalf("first scan = %+v, %v", res, err)
	}
	if d := bookDuration(t, db); d < headerlessSecs-0.001 || d > headerlessSecs+0.001 {
		t.Fatalf("duration %v, want the frames counted, %v", d, headerlessSecs)
	}

	// As an older server left it: the estimate, and no revision.
	if _, err := db.ExecContext(ctx, `UPDATE books SET duration = 5.0799, duration_rev = 0`); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 1 {
		t.Fatalf("rescan = %+v, %v; want the book read again", res, err)
	}
	if d := bookDuration(t, db); d < headerlessSecs-0.001 || d > headerlessSecs+0.001 {
		t.Errorf("after the rescan: duration %v, want %v", d, headerlessSecs)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 0 {
		t.Errorf("third scan = %+v, %v; want the book left be", res, err)
	}
}

// A header-less VBR MP3 whose frames could not be counted (the count passed
// its deadline, and ffprobe's estimate was stored) leaves the book's length
// revision unstamped, so the next scan reads it again, its files unchanged;
// once that read counts the frames the revision is stamped and the scan after
// leaves the book be (SRV-16).
func TestScanRereadsAnUncountedLength(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, cat, lib, s := headerlessBook(t)
	const estimate = 5.0799
	// The count out of time, as on a slow disk: ffprobe's estimate stands.
	s.readFile = func(ctx context.Context, root, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		md := extractFile(ctx, root, p, ffprobe, timeout)
		md.Duration = estimate
		return metadata.UncountedForTests(md)
	}
	book := func() (d float64, stale bool) {
		t.Helper()
		d = bookDuration(t, db)
		sigs, err := cat.Signatures(ctx, lib.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d, sigs["Author"].DurationStale
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Added != 1 {
		t.Fatalf("first scan = %+v, %v", res, err)
	}
	if d, stale := book(); d != estimate || !stale {
		t.Fatalf("uncounted: duration %v stale %t, want the estimate, its revision unstamped", d, stale)
	}
	// Rescanned with the count out of time again: still the estimate, still stale.
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 1 {
		t.Fatalf("rescan, uncounted = %+v, %v; want the book read again", res, err)
	}
	if _, stale := book(); !stale {
		t.Fatal("uncounted twice: revision stamped")
	}

	s.readFile = nil
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 1 {
		t.Fatalf("rescan = %+v, %v; want the book read again", res, err)
	}
	if d, stale := book(); d < headerlessSecs-0.001 || d > headerlessSecs+0.001 || stale {
		t.Errorf("counted: duration %v stale %t, want %v, the revision stamped", d, stale, headerlessSecs)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 0 {
		t.Errorf("fourth scan = %+v, %v; want the book left be", res, err)
	}
}

// readCounter wraps a scanner's file reads: it counts each path's reads, and
// cuts short (metadata.UnreadForTests) those of the paths in cut while it holds
// them.
type readCounter struct {
	mu    sync.Mutex
	reads map[string]int
	cut   map[string]bool
}

func (c *readCounter) read(ctx context.Context, root, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
	c.mu.Lock()
	c.reads[filepath.Base(p)]++
	cut := c.cut[filepath.Base(p)]
	c.mu.Unlock()
	if cut {
		return metadata.UnreadForTests(p, false)
	}
	return extractFile(ctx, root, p, ffprobe, timeout)
}

func (c *readCounter) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[name]
}

func (c *readCounter) setCut(name string, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cut[name] = on
}

// A book of an older length revision whose read is cut short (a part on a
// share that stopped answering) is kept as stored, its problem recorded, and
// read again only once that part answers (problemCleared), not every part on
// every scan for the revision (server#452); once it answers the book is read
// and its revision recorded, and the scan after leaves it be.
func TestStaleCutShortBookWaitsForItsProblem(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	fixture := filepath.Join("..", "metadata", "testdata", "probe", "lame-vbr-noxing.mp3")
	copyFixture(t, fixture, filepath.Join(root, "Author", "Book", "01.mp3"))
	copyFixture(t, fixture, filepath.Join(root, "Author", "Book", "02.mp3"))
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s := NewScanner(cat, "", slog.Default())
	c := &readCounter{reads: map[string]int{}, cut: map[string]bool{}}
	s.readFile = c.read
	if res, err := s.Scan(ctx, *lib); err != nil || res.Added != 1 {
		t.Fatalf("first scan = %+v, %v", res, err)
	}
	stale := func() bool {
		t.Helper()
		sigs, err := cat.Signatures(ctx, lib.ID)
		if err != nil {
			t.Fatal(err)
		}
		return sigs["Author/Book"].DurationStale
	}

	// As an older server left it, and its second part's reads now hang.
	if _, err := db.ExecContext(ctx, `UPDATE books SET duration_rev = 0`); err != nil {
		t.Fatal(err)
	}
	c.setCut("02.mp3", true)
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	var problem string
	if err := db.QueryRowContext(ctx, `SELECT scan_error FROM books`).Scan(&problem); err != nil || problem != problemUnreadable {
		t.Fatalf("problem %q, %v; want the cut-short part's", problem, err)
	}
	if !stale() {
		t.Fatal("a read cut short recorded the revision")
	}
	first := c.count("01.mp3")
	for range 2 {
		if _, err := s.Scan(ctx, *lib); err != nil {
			t.Fatal(err)
		}
	}
	if n := c.count("01.mp3"); n != first {
		t.Errorf("the first part read %d more times while the second still hangs, want none: only the problem's part is looked at", n-first)
	}

	c.setCut("02.mp3", false)
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 1 {
		t.Fatalf("scan once the part answers = %+v, %v; want the book read", res, err)
	}
	if stale() {
		t.Error("read whole: revision not recorded")
	}
	first = c.count("01.mp3")
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 0 || c.count("01.mp3") != first {
		t.Errorf("scan after = %+v, %v; want the book left be", res, err)
	}
}

// A scan's tag backfill of a book it found unchanged (here its tag hints)
// records no length, so it counts no frames of a header-less VBR MP3, which
// reads the whole file (metadata.WithoutFrameCount, server#452): its read
// gives the bitrate's estimate, where the index counted them.
func TestTagBackfillCountsNoFrames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, _, lib, s := headerlessBook(t)
	var mu sync.Mutex
	var lengths []float64
	s.readFile = func(ctx context.Context, root, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		md := extractFile(ctx, root, p, ffprobe, timeout)
		mu.Lock()
		lengths = append(lengths, md.Duration)
		mu.Unlock()
		return md
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE books SET hints_checked = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lengths) != 2 {
		t.Fatalf("%d reads, want the index's and the backfill's", len(lengths))
	}
	if d := lengths[0]; d < headerlessSecs-0.001 || d > headerlessSecs+0.001 {
		t.Errorf("the index read %v, want the frames counted, %v", d, headerlessSecs)
	}
	if d := lengths[1]; d <= 0 || (d > headerlessSecs-0.001 && d < headerlessSecs+0.001) {
		t.Errorf("the backfill read %v, want the bitrate's estimate, no count", d)
	}
	if d := bookDuration(t, db); d < headerlessSecs-0.001 || d > headerlessSecs+0.001 {
		t.Errorf("stored duration %v after the backfill, want the counted %v kept", d, headerlessSecs)
	}
}

package library

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// Phase 3: ignore rules, scan schedules, read problems, suspect folders and the
// job queue.

func newHealthCatalog(t *testing.T) (*catalog.Catalog, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return catalog.New(db, time.Now), ctx
}

func writeFile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeIgnore(t *testing.T) {
	got, err := NormalizeIgnore([]string{"  *.sample.mp3 ", "", "# extras", "Podcasts/*", "Extras/"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "*.sample.mp3|# extras|Podcasts/*|Extras/" {
		t.Fatalf("normalized = %q", got)
	}
	tooMany := make([]string, maxIgnorePatterns+1)
	for i := range tooMany {
		tooMany[i] = "x" + strings.Repeat("y", i%5)
	}
	for name, lines := range map[string][]string{
		"bad wildcard": {"[abc"},
		"only slashes": {"//"},
		"too long":     {strings.Repeat("a", maxIgnorePattern+1)},
		"too many":     tooMany,
	} {
		if _, err := NormalizeIgnore(lines); !errors.Is(err, ErrInvalidIgnore) {
			t.Errorf("%s: err = %v, want ErrInvalidIgnore", name, err)
		}
	}
}

func TestIgnoreMatch(t *testing.T) {
	ig := ParseIgnore([]string{"*.sample.mp3", "# Extras", "podcasts/*", "Extras/", "[bad"})
	for _, tc := range []struct {
		rel   string
		dir   bool
		want  bool
		cover bool
	}{
		{"Author/Book/Intro.Sample.mp3", false, true, true}, // name pattern, any depth, any case
		{"Author/Book/01.mp3", false, false, false},
		{"Podcasts/Feed", true, true, true},             // anchored
		{"Kids/Podcasts/Feed", true, false, false},      // anchored to the root only
		{"Author/Extras", true, true, true},             // folder-only pattern
		{"Author/Extras", false, false, false},          // ... not a file of that name
		{"Author/Extras/bonus.mp3", false, false, true}, // inside an ignored folder
		{"Podcasts/Feed/ep1.mp3", false, false, true},   // under an anchored match
		{"# Extras", true, false, false},                // comments are not patterns
	} {
		if got := ig.Match(tc.rel, tc.dir); got != tc.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", tc.rel, tc.dir, got, tc.want)
		}
		if got := ig.Covers(tc.rel, tc.dir); got != tc.cover {
			t.Errorf("Covers(%q, dir=%v) = %v, want %v", tc.rel, tc.dir, got, tc.cover)
		}
	}
	var none *Ignore
	if none.Match("a", false) || none.Covers("a/b", false) || !none.Empty() {
		t.Error("a nil Ignore ignores something")
	}
}

func TestParseSchedule(t *testing.T) {
	for _, ok := range []string{"", "every:1h", "every:6h", "every:24h", "daily:03:00", "daily:23:59"} {
		if _, err := ParseSchedule(ok); err != nil {
			t.Errorf("ParseSchedule(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"daily", "every:5h", "every:6", "daily:3:00", "daily:24:00", "daily:12:60", "weekly:mon", "every:-1h"} {
		if _, err := ParseSchedule(bad); !errors.Is(err, ErrInvalidSchedule) {
			t.Errorf("ParseSchedule(%q) = %v, want ErrInvalidSchedule", bad, err)
		}
	}
}

func TestScheduleNext(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, loc)
	every, _ := ParseSchedule("every:6h")
	if got := every.Next(now.Add(-2*time.Hour), now); !got.Equal(now.Add(4 * time.Hour)) {
		t.Errorf("every 6h after a scan 2h ago = %v", got)
	}
	if got := every.Next(time.Time{}, now); !got.Equal(now.Add(6 * time.Hour)) {
		t.Errorf("every 6h, never scanned = %v", got)
	}
	daily, _ := ParseSchedule("daily:03:00")
	// Last scan yesterday 03:05: today's 03:00 is due (the server was off at 03:00).
	if got := daily.Next(time.Date(2026, 10, 3, 3, 5, 0, 0, loc), now); !got.Equal(time.Date(2026, 10, 4, 3, 0, 0, 0, loc)) {
		t.Errorf("daily after yesterday's scan = %v", got)
	}
	// Last scan today 03:00 exactly: next is tomorrow.
	if got := daily.Next(time.Date(2026, 10, 4, 3, 0, 0, 0, loc), now); !got.Equal(time.Date(2026, 10, 5, 3, 0, 0, 0, loc)) {
		t.Errorf("daily after today's scan = %v", got)
	}
	if off, _ := ParseSchedule(""); !off.Off() || !off.Next(now, now).IsZero() {
		t.Error("an empty schedule runs something")
	}
}

func TestSuspectParts(t *testing.T) {
	long := float64(minSuspectPart)
	for _, tc := range []struct {
		name  string
		parts []partFacts
		want  int
	}{
		{"one book in parts", []partFacts{{"The Way of Kings", long}, {"The Way of Kings", long}}, 0},
		{"numbered parts", []partFacts{{"Part 1", long}, {"Part 2", long}, {"Part 3 of 3", long}}, 0},
		{"three books", []partFacts{{"Book 1 - Elantris", long}, {"Warbreaker", long}, {"Tress", long}}, 3},
		{"short parts", []partFacts{{"Chapter One", 1800}, {"Chapter Two", 1800}}, 0},
		{"unknown length", []partFacts{{"Elantris", 0}, {"Warbreaker", long}}, 0},
		{"one part", []partFacts{{"Elantris", long}}, 0},
	} {
		if got := suspectParts(tc.parts); got != tc.want {
			t.Errorf("%s: suspectParts = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A 0-byte part is recorded on the book as a read problem; ignored files and
// folders are skipped by the scan, the browse view and on-demand indexing.
func TestScanRecordsProblemsAndHonoursIgnore(t *testing.T) {
	cat, ctx := newHealthCatalog(t)
	root := t.TempDir()
	audio, err := os.ReadFile(filepath.Join(testdataRoot(t), "Brandon Sanderson", "Mistborn", "01 - The Final Empire.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Author/Broken/01.mp3", audio)
	writeFile(t, root, "Author/Broken/02.mp3", nil) // 0 bytes
	writeFile(t, root, "Author/Good/book.m4b", audio)
	writeFile(t, root, "Author/Good/intro.sample.m4b", audio)
	writeFile(t, root, "Extras/bonus/bonus.m4b", audio)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root,
		IgnorePatterns: []string{"*.sample.m4b", "Extras/"}})
	scanner := NewScanner(cat, "", slog.Default())
	res, err := scanner.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	if res.Books != 2 || res.Errors != 1 {
		t.Fatalf("scan = %+v, want 2 books (the Extras folder ignored) and 1 problem", res.ScanCounts)
	}
	page, err := cat.ListAdminBooks(ctx, catalog.AdminListOptions{Filter: catalog.BookFilter{Issue: catalog.IssueScanError}})
	if err != nil || len(page.Books) != 1 {
		t.Fatalf("scan_error books = %+v (err %v)", page, err)
	}
	if b := page.Books[0]; b.Path != "Author/Broken" || b.ScanError != problemEmptyFile || b.ScanErrorFile != "Author/Broken/02.mp3" {
		t.Fatalf("problem = %+v", b)
	}
	good, err := cat.GetBookByPath(ctx, lib.ID, "Author/Good")
	if err != nil || len(good.Files) != 1 {
		t.Fatalf("Author/Good should hold only book.m4b (the sample ignored): %+v (err %v)", good, err)
	}
	var logged bool
	for _, e := range res.Log {
		logged = logged || (e.Kind == "problem" && e.Code == problemEmptyFile && e.Path == "Author/Broken/02.mp3")
	}
	if !logged {
		t.Fatalf("the problem isn't in the run log: %+v", res.Log)
	}

	listing, err := BrowseFS(root, "", 0, 100, nil, ParseIgnore(lib.IgnorePatterns))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range listing.Entries {
		if e.Name == "Extras" {
			t.Fatal("an ignored folder shows in the browse view")
		}
	}
	if _, err := scanner.IndexPath(ctx, *lib, "Extras/bonus"); !errors.Is(err, ErrNotIndexable) {
		t.Fatalf("indexing an ignored folder = %v, want ErrNotIndexable", err)
	}

	// The part is filled in: a rescan clears the problem.
	writeFile(t, root, "Author/Broken/02.mp3", audio)
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(root, "Author", "Broken", "02.mp3"), later, later)
	if _, err := scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if page, _ := cat.ListAdminBooks(ctx, catalog.AdminListOptions{Filter: catalog.BookFilter{Issue: catalog.IssueScanError}}); len(page.Books) != 0 {
		t.Fatalf("the fixed book still has a problem: %+v", page.Books)
	}
}

// ffprobe's own message is what a probe failure records (needs ffprobe).
func TestProbeFailureRecorded(t *testing.T) {
	ffprobe := lookFFprobe(t)
	cat, ctx := newHealthCatalog(t)
	root := t.TempDir()
	writeFile(t, root, "Author/Truncated.m4b", []byte("not really an m4b file at all"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := NewScanner(cat, ffprobe, slog.Default()).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	page, _ := cat.ListAdminBooks(ctx, catalog.AdminListOptions{Filter: catalog.BookFilter{Issue: catalog.IssueScanError}})
	if len(page.Books) != 1 || page.Books[0].ScanError != problemProbe || page.Books[0].ScanErrorDetail == "" ||
		strings.Contains(page.Books[0].ScanErrorDetail, root) {
		t.Fatalf("probe problem = %+v, want probe_failed with ffprobe's message and no absolute path", page.Books)
	}
}

func lookFFprobe(t *testing.T) string {
	t.Helper()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if p := filepath.Join(dir, "ffprobe"); fileExists(p) {
			return p
		}
	}
	t.Skip("ffprobe not installed")
	return ""
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// noteProblem keeps the first problem and strips the absolute path from an OS error.
func TestNoteProblem(t *testing.T) {
	b := &catalog.Book{}
	_, openErr := os.Open(filepath.Join(t.TempDir(), "gone.mp3"))
	noteProblem(b, "A/01.mp3", 10, &metadata.Metadata{OpenErr: openErr})
	noteProblem(b, "A/02.mp3", 0, nil)
	if b.ScanError != problemUnreadable || b.ScanErrorFile != "A/01.mp3" || strings.Contains(b.ScanErrorDetail, "/") {
		t.Fatalf("problem = %q %q %q", b.ScanError, b.ScanErrorFile, b.ScanErrorDetail)
	}
}

// The queue's coalescing: a schedule or startup ask while the library scans adds
// nothing; a manual or change ask queues one follow-up; cancelling a queued job
// drops it.
func TestEnqueueCoalescing(t *testing.T) {
	cat, ctx := newHealthCatalog(t)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: t.TempDir()})
	other, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "O", Root: t.TempDir()})
	s := NewScanner(cat, "", slog.Default())
	// Pretend lib's scan is running (the worker isn't started).
	s.jobs.running = &Job{ID: 99, LibraryID: lib.ID}

	if j := s.Enqueue(*lib, TriggerSchedule, nil); j.ID != 99 {
		t.Fatalf("a schedule ask while running queued job %d", j.ID)
	}
	if j := s.Enqueue(*lib, TriggerStartup, nil); j.ID != 99 {
		t.Fatalf("a startup ask while running queued job %d", j.ID)
	}
	follow := s.Enqueue(*lib, TriggerChange, nil)
	if follow.ID == 99 || !s.Progress(lib.ID).Queued {
		t.Fatalf("a change while running didn't queue a follow-up: %+v", follow)
	}
	if again := s.Enqueue(*lib, TriggerManual, nil); again.ID != follow.ID {
		t.Fatalf("a second ask queued another follow-up (%d, want %d)", again.ID, follow.ID)
	}
	o := s.Enqueue(*other, TriggerManual, nil)
	if _, queued := s.Jobs(); len(queued) != 2 {
		t.Fatalf("queued = %+v, want the follow-up and the other library", queued)
	}
	if !s.Cancel(follow.ID) || s.Progress(lib.ID).Queued {
		t.Fatal("cancelling the follow-up didn't clear it")
	}
	if s.Cancel(follow.ID) {
		t.Fatal("cancelling it twice succeeded")
	}
	if _, queued := s.Jobs(); len(queued) != 1 || queued[0].ID != o.ID {
		t.Fatalf("queued = %+v, want only the other library", queued)
	}
}

// Cancelling the running scan stops it before it prunes, recorded as cancelled.
func TestCancelRunningScan(t *testing.T) {
	cat, ctx := newHealthCatalog(t)
	root, _ := filepath.Abs(testdataRoot(t))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	jctx, cancel := context.WithCancel(ctx)
	j := &Job{ID: 1, LibraryID: lib.ID, Trigger: TriggerManual, cancel: cancel}
	s.jobs.running = j
	if !s.Cancel(1) {
		t.Fatal("cancel of the running job failed")
	}
	s.run(ctx, jctx, j)
	runs, _ := cat.ListScanRuns(ctx, lib.ID, 0, 5)
	if len(runs) != 1 || runs[0].Status != catalog.RunCancelled {
		t.Fatalf("runs = %+v, want one cancelled", runs)
	}
	if n, _ := cat.CountBooksByLibrary(ctx); n[lib.ID] != 0 {
		t.Fatalf("a cancelled scan indexed %d books", n[lib.ID])
	}
}

// A due schedule queues a scan; one not yet due doesn't.
func TestQueueDue(t *testing.T) {
	cat, ctx := newHealthCatalog(t)
	due, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Due", Root: t.TempDir(), ScanSchedule: "every:1h"})
	later, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Later", Root: t.TempDir(), ScanSchedule: "every:24h"})
	_, _ = cat.CreateLibrary(ctx, catalog.Library{Name: "Off", Root: t.TempDir()})
	for _, l := range []*catalog.Library{due, later} {
		id, err := cat.StartScanRun(ctx, l.ID, TriggerManual, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = cat.FinishScanRun(ctx, id, catalog.RunOK, catalog.ScanCounts{}, nil)
	}
	s := NewScanner(cat, "", slog.Default())
	s.queueDue(ctx, time.Now().Add(2*time.Hour))
	_, queued := s.Jobs()
	if len(queued) != 1 || queued[0].LibraryID != due.ID || queued[0].Trigger != TriggerSchedule {
		t.Fatalf("queued = %+v, want only the due library, by schedule", queued)
	}
	next, _ := s.NextScans(ctx)
	if _, ok := next[due.ID]; !ok || len(next) != 2 {
		t.Fatalf("next scans = %v, want the two scheduled libraries", next)
	}
}

// A folder book indexed before suspect detection is checked on the next scan with
// a tag read, without re-indexing it.
func TestSuspectBackfill(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	// Two untagged parts named like two books.
	writeFile(t, root, "Series/Elantris.mp3", []byte("x"))
	writeFile(t, root, "Series/Warbreaker.mp3", []byte("y"))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	// Indexed without ffprobe the lengths are unknown, so it isn't suspect. Make it
	// a row from before 0017, which the migration leaves unchecked when every part
	// is an hour or longer.
	if _, err := db.ExecContext(ctx, `UPDATE books SET suspect_parts = NULL WHERE rel_path = 'Series'`); err != nil {
		t.Fatal(err)
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil || res.Updated != 0 {
		t.Fatalf("rescan = %+v (err %v), want nothing re-indexed", res.ScanCounts, err)
	}
	page, _ := cat.ListAdminBooks(ctx, catalog.AdminListOptions{Filter: catalog.BookFilter{Issue: catalog.IssueSuspect}})
	if len(page.Books) != 1 || page.Books[0].SuspectParts != 2 {
		t.Fatalf("suspect books = %+v, want Series as 2 books", page.Books)
	}
}

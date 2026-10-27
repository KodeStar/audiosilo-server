package library

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// copyFixture copies a testdata file to dst (its folders made).
func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Dir(dst), filepath.Base(dst), data)
}

// mixedLibrary builds a library of every shape a scan reads, a few times over:
// multi-part folder books, a chaptered single file in its folder, loose
// single-file books, a book joined from disc folders, and a book with a file
// that isn't audio and an empty one (read problems).
func mixedLibrary(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join("..", "..", "testdata")
	mp3s := []string{
		filepath.Join(repo, "multifile", "Jim Butcher", "The Dresden Files", "01 - Storm Front", "01 - Part One.mp3"),
		filepath.Join(repo, "multifile", "Jim Butcher", "The Dresden Files", "01 - Storm Front", "02 - Part Two.mp3"),
	}
	m4bs := []string{
		filepath.Join(repo, "library", "Will Wight", "Cradle", "01 - Unsouled.m4b"),
		filepath.Join(repo, "library", "Will Wight", "Cradle", "02 - Soulsmith.m4b"),
		filepath.Join(repo, "library", "Brandon Sanderson", "Mistborn", "01 - The Final Empire.m4b"),
	}
	for n := range 4 {
		author := filepath.Join(root, fmt.Sprintf("Author %d", n))
		for p := range 5 + n {
			copyFixture(t, mp3s[p%2], filepath.Join(author, "Series", fmt.Sprintf("Book %d", n), fmt.Sprintf("%02d - Part.mp3", p+1)))
		}
		copyFixture(t, filepath.Join(repo, "chaptered.m4b"), filepath.Join(author, "Chaptered", "Chaptered.m4b"))
		for i, src := range m4bs {
			copyFixture(t, src, filepath.Join(author, "Loose", fmt.Sprintf("Loose %d.m4b", i+1)))
		}
		for d := range 2 {
			for p := range 3 {
				copyFixture(t, mp3s[p%2], filepath.Join(author, "Discs", fmt.Sprintf("CD%d", d+1), fmt.Sprintf("%02d.mp3", p+1)))
			}
		}
		broken := filepath.Join(author, "Broken")
		copyFixture(t, mp3s[0], filepath.Join(broken, "01.mp3"))
		writeFile(t, broken, "02.mp3", []byte(strings.Repeat("not audio ", 400)))
		writeFile(t, broken, "03.mp3", nil)
		copyFixture(t, mp3s[1], filepath.Join(broken, "04.mp3"))
	}
	return root
}

// fixedClock is a catalog clock that never moves, so two scans' rows compare.
func fixedClock() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) }

// scanWith scans root into a fresh index with concurrency readers and returns
// the result, the catalog and the library.
func scanWith(t *testing.T, root, ffprobe string, concurrency int) (*ScanResult, *catalog.Catalog, *catalog.Library, *store.DB) {
	t.Helper()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, fixedClock)
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Mixed", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s := NewScanner(cat, ffprobe, slog.New(slog.DiscardHandler))
	s.SetConcurrency(concurrency)
	res, err := s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	return res, cat, lib, db
}

// dumpIndex is every row of the scan's tables, one line each, sorted: a file's
// and a chapter's book named by its path, not its id (ids follow discovery's
// order, which is the walk's map order, the same for a serial scan run twice).
func dumpIndex(t *testing.T, db *store.DB) string {
	t.Helper()
	queries := map[string]string{
		"books":      "SELECT * FROM books",
		"book_files": "SELECT b.rel_path AS book, f.* FROM book_files f JOIN books b ON b.id = f.book_id",
		"chapters":   "SELECT b.rel_path AS book, c.* FROM chapters c JOIN books b ON b.id = c.book_id",
	}
	var lines []string
	for table, q := range queries {
		rows, err := db.QueryContext(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			b.WriteString(table + ":")
			for i, v := range vals {
				if cols[i] == "id" || cols[i] == "book_id" {
					continue
				}
				if raw, ok := v.([]byte); ok {
					v = string(raw)
				}
				fmt.Fprintf(&b, " %s=%v", cols[i], v)
			}
			lines = append(lines, b.String())
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(lines)
	// ffprobe names its decoder by address in a message ("[mp3 @ 0xaac820000]"),
	// different on every run.
	return ffprobeAddr.ReplaceAllString(strings.Join(lines, "\n"), "@ 0x")
}

var ffprobeAddr = regexp.MustCompile(`@ 0x[0-9a-f]+`)

// A scan reading many files at once writes exactly the index a scan reading one
// at a time writes: the same rows, file order, durations, chapter offsets and
// read problems, and the same counts.
func TestScanConcurrentMatchesSerial(t *testing.T) {
	t.Parallel()
	probes := []string{""}
	if metadata.HasFFprobe("ffprobe") {
		probes = append(probes, "ffprobe")
	}
	root := mixedLibrary(t)
	for _, ffprobe := range probes {
		serial, _, _, serialDB := scanWith(t, root, ffprobe, 1)
		parallel, _, _, parallelDB := scanWith(t, root, ffprobe, 8)
		want, got := dumpIndex(t, serialDB), dumpIndex(t, parallelDB)
		if want == "" {
			t.Fatal("the serial scan indexed nothing")
		}
		if got != want {
			t.Fatalf("ffprobe %q: the concurrent scan's index differs from the serial one's:\nserial:\n%s\nconcurrent:\n%s", ffprobe, want, got)
		}
		if serial.ScanCounts != parallel.ScanCounts {
			t.Fatalf("ffprobe %q: counts differ: serial %+v, concurrent %+v", ffprobe, serial.ScanCounts, parallel.ScanCounts)
		}
	}
}

// A file that can't be read is that book's problem: the scan reads on and
// indexes every other book, and the broken book too, with its problem recorded.
func TestScanConcurrentReadProblemDoesNotStopScan(t *testing.T) {
	t.Parallel()
	root := mixedLibrary(t)
	res, cat, lib, db := scanWith(t, root, "", 8)
	page, err := cat.ListBooks(context.Background(), catalog.ListOptions{LibraryID: lib.ID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Books) != res.Books || res.Added != res.Books {
		t.Fatalf("indexed %d of %d books (added %d)", len(page.Books), res.Books, res.Added)
	}
	for n := range 4 {
		rel := fmt.Sprintf("Author %d/Broken", n)
		var problem, file string
		var files int
		if err := db.QueryRowContext(context.Background(), `SELECT scan_error, scan_error_file,
			(SELECT COUNT(*) FROM book_files f WHERE f.book_id = b.id) FROM books b WHERE library_id = ? AND rel_path = ?`,
			lib.ID, rel).Scan(&problem, &file, &files); err != nil {
			t.Fatal(err)
		}
		if problem != problemEmptyFile || file != rel+"/03.mp3" || files != 4 {
			t.Fatalf("%s: problem %q in %q, %d files", rel, problem, file, files)
		}
	}
	if res.Errors != 4 {
		t.Fatalf("errors = %d, want the 4 broken books", res.Errors)
	}
}

// stubProbe writes an executable ffprobe stand-in running script (sh) and
// returns its path.
func stubProbe(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the ffprobe stand-in is a shell script")
	}
	p := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil { //nolint:gosec // G306: the stand-in must be executable
		t.Fatal(err)
	}
	return p
}

// No more files are read at once than the concurrency allows, and more than
// one are (each read is ffprobe's run here: the stand-in records how many are
// under way as it starts).
func TestScanReadsBoundedConcurrently(t *testing.T) {
	t.Parallel()
	running, log := t.TempDir(), filepath.Join(t.TempDir(), "log")
	probe := stubProbe(t, fmt.Sprintf(`touch %q/$$; ls %q | wc -l >> %q; sleep 0.2; rm %q/$$; echo '{}'`,
		running, running, log, running))
	root := mixedLibrary(t)
	const limit = 3
	res, _, _, _ := scanWith(t, root, probe, limit)
	if res.Added != res.Books {
		t.Fatalf("added %d of %d", res.Added, res.Books)
	}
	f, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	most, runs := 0, 0
	for sc := bufio.NewScanner(f); sc.Scan(); {
		n, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
		if err != nil {
			t.Fatal(err)
		}
		most, runs = max(most, n), runs+1
	}
	if most > limit {
		t.Fatalf("%d files read at once, the limit is %d", most, limit)
	}
	if most < 2 {
		t.Fatalf("at most %d file read at once over %d reads: not concurrent", most, runs)
	}
}

// A Stop ends the reads under way (ffprobe killed, not waited out), writes none
// of them (a reading cut short would record a false problem), and a later scan
// indexes the library in full.
func TestScanConcurrentStop(t *testing.T) {
	t.Parallel()
	probe := stubProbe(t, "exec sleep 30")
	root := mixedLibrary(t)
	ctx, cancel := context.WithCancel(context.Background())
	db := storetest.Open(t)
	cat := catalog.New(db, fixedClock)
	lib, err := cat.CreateLibrary(context.Background(), catalog.Library{Name: "Mixed", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s := NewScanner(cat, probe, slog.New(slog.DiscardHandler))
	s.SetConcurrency(4)
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	if _, err := s.Scan(ctx, *lib); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped scan: err = %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the stop waited %v for the reads under way", took)
	}
	page, err := cat.ListBooks(context.Background(), catalog.ListOptions{LibraryID: lib.ID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Books) != 0 {
		t.Fatalf("a stopped scan wrote %d books read with ffprobe cut short", len(page.Books))
	}

	s.ffprobePath = ""
	res, err := s.Scan(context.Background(), *lib)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != res.Books || res.Books == 0 {
		t.Fatalf("rescan added %d of %d", res.Added, res.Books)
	}
}

// readFiles hands back a folder book's readings in file order, the first the
// one passed in, however many are read at once.
func TestReadFilesKeepsOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var files []catalog.BookFile
	for i := range 20 {
		rel := fmt.Sprintf("Book/%02d.mp3", i)
		writeFile(t, root, rel, nil)
		files = append(files, catalog.BookFile{RelPath: rel})
	}
	s := NewScanner(nil, "", slog.New(slog.DiscardHandler))
	lib := catalog.Library{Root: root}
	first := &metadata.Metadata{Title: "first"}
	for _, slots := range []readSlots{newReadSlots(1), newReadSlots(3), newReadSlots(64)} {
		mds := s.readFiles(context.Background(), lib, files, first, slots)
		if len(mds) != len(files) || mds[0] != first {
			t.Fatalf("slots %d: %d readings, first %v", cap(slots), len(mds), mds[0])
		}
		for i, md := range mds[1:] {
			if md == nil || md.Format != "mp3" || md.OpenErr != nil {
				t.Fatalf("slots %d: file %d read as %+v", cap(slots), i+1, md)
			}
		}
	}
	if got := s.readFiles(context.Background(), lib, nil, first, newReadSlots(2)); len(got) != 0 {
		t.Fatalf("no files read as %v", got)
	}
}

// The default reads one file per CPU, up to DefaultScanReaders; a set
// concurrency is used as set.
func TestScanReaders(t *testing.T) {
	t.Parallel()
	s := NewScanner(nil, "", nil)
	if got, want := s.readers(), min(runtime.GOMAXPROCS(0), DefaultScanReaders); got != want {
		t.Fatalf("default readers = %d, want %d", got, want)
	}
	s.SetConcurrency(20)
	if got := s.readers(); got != 20 {
		t.Fatalf("readers = %d, want 20", got)
	}
	s.SetConcurrency(-3)
	if got := s.readers(); got < 1 || got > DefaultScanReaders {
		t.Fatalf("readers for -3 = %d", got)
	}
}

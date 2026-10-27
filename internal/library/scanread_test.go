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
	s, cat, lib, db := newTestScanner(t, root, ffprobe, concurrency)
	res, err := s.Scan(context.Background(), *lib)
	if err != nil {
		t.Fatal(err)
	}
	return res, cat, lib, db
}

// newTestScanner is a scanner with concurrency readers of root, a library in a
// fresh index.
func newTestScanner(t *testing.T, root, ffprobe string, concurrency int) (*Scanner, *catalog.Catalog, *catalog.Library, *store.DB) {
	t.Helper()
	db := storetest.Open(t)
	cat := catalog.New(db, fixedClock)
	lib, err := cat.CreateLibrary(context.Background(), catalog.Library{Name: "Mixed", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s := NewScanner(cat, ffprobe, slog.New(slog.DiscardHandler))
	s.SetConcurrency(concurrency)
	return s, cat, lib, db
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
// under way as it starts; its first run waits up to 3 s for a second to start,
// so a busy machine starting them slowly still shows them side by side).
func TestScanReadsBoundedConcurrently(t *testing.T) {
	t.Parallel()
	running, log := t.TempDir(), filepath.Join(t.TempDir(), "log")
	probe := stubProbe(t, fmt.Sprintf(`touch %[1]q/$$; ls %[1]q | wc -l >> %[2]q
if mkdir %[1]q/.first 2>/dev/null; then
  i=0; while [ "$(ls %[1]q | wc -l)" -lt 2 ] && [ $i -lt 60 ]; do sleep 0.05; i=$((i+1)); done
fi
sleep 0.2; rm %[1]q/$$; echo '{}'`, running, log))
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
	for _, slots := range []*readSlots{newReadSlots(1, time.Second), newReadSlots(3, time.Second), newReadSlots(64, time.Second)} {
		mds := s.readFiles(context.Background(), lib, files, first, slots)
		if len(mds) != len(files) || mds[0] != first {
			t.Fatalf("slots %d: %d readings, first %v", slots.size(), len(mds), mds[0])
		}
		for i, md := range mds[1:] {
			if md == nil || md.Format != "mp3" || md.OpenErr != nil {
				t.Fatalf("slots %d: file %d read as %+v", slots.size(), i+1, md)
			}
		}
	}
	if got := s.readFiles(context.Background(), lib, nil, first, newReadSlots(2, time.Second)); len(got) != 0 {
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

// smallLibrary is books folder books of files MP3 parts each.
func smallLibrary(t *testing.T, books, files int) string {
	t.Helper()
	root := t.TempDir()
	mp3 := filepath.Join("..", "..", "testdata", "multifile", "Jim Butcher", "The Dresden Files", "01 - Storm Front", "01 - Part One.mp3")
	for b := range books {
		for f := range files {
			copyFixture(t, mp3, filepath.Join(root, "Author", fmt.Sprintf("Book %d", b), fmt.Sprintf("%02d.mp3", f+1)))
		}
	}
	return root
}

// timeoutScanner is a scanner of root whose ffprobe runs time out after timeout.
func timeoutScanner(t *testing.T, root, probe string, concurrency int, timeout time.Duration) (*Scanner, *catalog.Library, *store.DB) {
	t.Helper()
	s, _, lib, db := newTestScanner(t, root, probe, concurrency)
	s.probeTimeout = timeout
	return s, lib, db
}

// probeAnswer is the stand-in's ffprobe output for a readable file.
const probeAnswer = `{"format":{"duration":"1.500000"},"streams":[{"codec_name":"mp3","codec_type":"audio"}]}`

// A file whose ffprobe runs out of time while others are read beside it (a slow
// disk shared by several reads) is read again on its own, and a healthy file
// read in time then is no read problem. The stand-in hangs while another of its
// runs is alive and answers when it runs alone; its first run waits for a
// second to start (up to 3 s), so two always overlap however busy the machine
// is. Nothing turns on how fast a run starts: the time-out (5 s) is only ever
// reached by a run that hangs.
func TestScanRetriesTimedOutProbeAlone(t *testing.T) {
	t.Parallel()
	running, log := t.TempDir(), filepath.Join(t.TempDir(), "log")
	probe := stubProbe(t, fmt.Sprintf(`touch %[1]q/$$
alive() { n=0; for f in %[1]q/*; do kill -0 "${f##*/}" 2>/dev/null && n=$((n+1)); done; echo $n; }
if [ "$(alive)" -gt 1 ]; then echo hung >> %[2]q; exec sleep 30; fi
if mkdir %[1]q/.first 2>/dev/null; then
  i=0; while [ "$(ls %[1]q | wc -l)" -lt 2 ] && [ $i -lt 60 ]; do sleep 0.05; i=$((i+1)); done
fi
rm -f %[1]q/$$
echo '%[3]s'`, running, log, probeAnswer))
	s, lib, db := timeoutScanner(t, smallLibrary(t, 2, 2), probe, 4, 5*time.Second)
	res, err := s.Scan(context.Background(), *lib)
	if err != nil {
		t.Fatal(err)
	}
	if hung, _ := os.ReadFile(log); len(hung) == 0 {
		t.Fatal("no ffprobe run timed out: the scan never read files side by side")
	}
	if res.Errors != 0 || res.Added != 2 {
		t.Fatalf("added %d, %d read problems", res.Added, res.Errors)
	}
	var problems, short int
	if err := db.QueryRowContext(context.Background(), `SELECT
		(SELECT COUNT(*) FROM books WHERE scan_error <> ''),
		(SELECT COUNT(*) FROM book_files WHERE duration <> 1.5)`).Scan(&problems, &short); err != nil {
		t.Fatal(err)
	}
	if problems != 0 || short != 0 {
		t.Fatalf("%d books with a read problem, %d files without their duration", problems, short)
	}
}

// A file whose ffprobe times out again when read alone is the file's problem:
// recorded, as a time-out.
func TestScanRecordsProbeTimingOutAlone(t *testing.T) {
	t.Parallel()
	s, lib, db := timeoutScanner(t, smallLibrary(t, 1, 3), stubProbe(t, "exec sleep 10"), 3, 300*time.Millisecond)
	res, err := s.Scan(context.Background(), *lib)
	if err != nil {
		t.Fatal(err)
	}
	var problem, detail string
	if err := db.QueryRowContext(context.Background(), `SELECT scan_error, scan_error_detail FROM books`).Scan(&problem, &detail); err != nil {
		t.Fatal(err)
	}
	if res.Errors != 1 || problem != problemProbe || !strings.Contains(detail, "timed out") {
		t.Fatalf("errors %d, problem %q (%q)", res.Errors, problem, detail)
	}
}

// A file whose ffprobe times out with nothing read beside it is not read again,
// however many slots the scan has: the time-out is already the file's own.
func TestScanSkipsRetryOfLoneTimeout(t *testing.T) {
	t.Parallel()
	runs := filepath.Join(t.TempDir(), "runs")
	probe := stubProbe(t, fmt.Sprintf("echo run >> %q; exec sleep 10", runs))
	// Time for the stand-in to start and say so on a busy machine: the run hangs anyway.
	s, lib, db := timeoutScanner(t, smallLibrary(t, 1, 1), probe, 4, 3*time.Second)
	res, err := s.Scan(context.Background(), *lib)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(runs)
	if n := strings.Count(string(got), "run"); n != 1 {
		t.Fatalf("ffprobe ran %d times for a lone file, want 1", n)
	}
	var problem string
	if err := db.QueryRowContext(context.Background(), `SELECT scan_error FROM books`).Scan(&problem); err != nil {
		t.Fatal(err)
	}
	if res.Errors != 1 || problem != problemProbe {
		t.Fatalf("errors %d, problem %q", res.Errors, problem)
	}
}

// A Stop ends a read retried on its own too, and the retry waiting its turn.
// The stop comes once a run has started past the first round (the stand-in logs
// each start): a retry, or a read held back for one. Unstopped, the 20 files'
// retries alone would take 20 s.
func TestScanStopEndsProbeRetry(t *testing.T) {
	t.Parallel()
	starts := filepath.Join(t.TempDir(), "starts")
	probe := stubProbe(t, fmt.Sprintf("echo start >> %q; exec sleep 30", starts))
	const slots = 4
	s, lib, db := timeoutScanner(t, smallLibrary(t, 4, 5), probe, slots, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan time.Time, 1)
	go func() {
		for ctx.Err() == nil {
			if got, _ := os.ReadFile(starts); strings.Count(string(got), "start") > slots {
				stopped <- time.Now()
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	_, err := s.Scan(ctx, *lib)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped scan: err = %v", err)
	}
	if took := time.Since(<-stopped); took > 5*time.Second {
		t.Fatalf("the stop waited %v for the retries", took)
	}
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM books`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a stopped scan wrote %d books (%v)", n, err)
	}
}

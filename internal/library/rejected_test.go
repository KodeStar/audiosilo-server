package library

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// rejectingProbe is an ffprobe stand-in that writes each file it is run on to
// a log, rejects the data of a file named broken* (ffmpeg's invalid-data words)
// unless a "<file>.ok" sits beside it, fails to read a file named flaky* (an
// I/O error, which may pass), and reads anything else.
func rejectingProbe(t *testing.T) (probe, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "log")
	probe = stubProbe(t, fmt.Sprintf(`for f; do :; done
echo "$f" >> %[1]q
case "${f##*/}" in
broken*) if [ ! -e "$f.ok" ]; then echo "[mov @ 0x1] moov atom not found" >&2; echo "$f: Invalid data found when processing input" >&2; exit 1; fi ;;
flaky*) echo "$f: Input/output error" >&2; exit 1 ;;
esac
echo '%[2]s'`, log, probeAnswer))
	return probe, log
}

// probedRuns counts the stand-in's runs on files whose name holds name.
func probedRuns(t *testing.T, log, name string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	n := 0
	for _, l := range strings.Split(string(data), "\n") {
		if strings.Contains(filepath.Base(l), name) {
			n++
		}
	}
	return n
}

// A book whose one file ffprobe rejected is found unchanged by the next scan,
// not read again (nor written, nor counted again as a problem), and its problem
// stays listed. This server runs no ffprobe on the file again while it and its
// book are as they were; a restarted server checks it once more. A rejection
// fixed in place is found by the scan after the file changes; an I/O failure,
// which may pass, is checked by every scan.
func TestRescanTakesARejectedFileAsRead(t *testing.T) {
	t.Parallel()
	probe, log := rejectingProbe(t)
	root := t.TempDir()
	audio := testAudio(t)
	writeFile(t, root, "A/Good/01.m4b", audio)
	writeFile(t, root, "A/Broken/broken.m4b", audio)
	writeFile(t, root, "broken loose.m4b", audio)
	writeFile(t, root, "A/Flaky/flaky.m4b", audio)
	cat, ctx := newHealthCatalog(t)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	problems := func() map[string]string {
		page, err := cat.ListAdminBooks(ctx, catalog.AdminListOptions{Filter: catalog.BookFilter{Issue: catalog.IssueScanError}})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, b := range page.Books {
			out[b.Path] = b.ScanError
		}
		return out
	}
	scan := func(s *Scanner) catalog.ScanCounts {
		t.Helper()
		res, err := s.Scan(ctx, *lib)
		if err != nil {
			t.Fatal(err)
		}
		return res.ScanCounts
	}
	s := NewScanner(cat, probe, slog.New(slog.DiscardHandler))
	if got := scan(s); got.Added != 4 || got.Errors != 3 {
		t.Fatalf("first scan = %+v, want 4 added, 3 problems", got)
	}
	want := map[string]string{"A/Broken": problemProbe, "broken loose.m4b": problemProbe, "A/Flaky": problemProbe}
	if got := problems(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("problems = %v, want %v", got, want)
	}

	for i := range 2 {
		if got := scan(s); got.Updated != 0 || got.Errors != 0 {
			t.Fatalf("rescan %d = %+v, want nothing read again", i, got)
		}
	}
	if n := probedRuns(t, log, "broken"); n != 2 {
		t.Fatalf("rejected files probed %d times over three scans, want once each", n)
	}
	if n := probedRuns(t, log, "flaky"); n != 3 {
		t.Fatalf("the I/O failure probed %d times over three scans, want every scan", n)
	}
	if got := problems(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("problems after the rescans = %v, want %v", got, want)
	}

	restarted := NewScanner(cat, probe, slog.New(slog.DiscardHandler))
	scan(restarted)
	scan(restarted)
	if n := probedRuns(t, log, "broken"); n != 4 {
		t.Fatalf("rejected files probed %d times, want once more each after a restart", n)
	}

	// The file is mended (here: the stand-in now reads it) and rewritten.
	broken := filepath.Join(root, "A", "Broken", "broken.m4b")
	writeFile(t, root, "A/Broken/broken.m4b.ok", nil)
	writeFile(t, root, "A/Broken/broken.m4b", append(audio, 0))
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(broken, later, later); err != nil {
		t.Fatal(err)
	}
	if got := scan(restarted); got.Updated != 1 {
		t.Fatalf("scan after the fix = %+v, want the book read again", got)
	}
	delete(want, "A/Broken")
	if got := problems(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("problems after the fix = %v, want %v", got, want)
	}
}

// Without the memory of a rejection, a problem cleared in place (no change to
// the file) is still found: the server that never saw ffprobe reject it checks.
func TestRejectedFileCheckedByAServerThatDidNotSeeIt(t *testing.T) {
	t.Parallel()
	probe, _ := rejectingProbe(t)
	root := t.TempDir()
	writeFile(t, root, "A/Broken/broken.m4b", testAudio(t))
	cat, ctx := newHealthCatalog(t)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if _, err := NewScanner(cat, probe, slog.New(slog.DiscardHandler)).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "A/Broken/broken.m4b.ok", nil) // ffprobe reads it now; the file itself is as it was
	res, err := NewScanner(cat, probe, slog.New(slog.DiscardHandler)).Scan(context.Background(), *lib)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 1 {
		t.Fatalf("scan = %+v, want the cleared book read again", res.ScanCounts)
	}
}

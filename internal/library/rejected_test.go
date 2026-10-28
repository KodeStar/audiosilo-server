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
// unless a "<file>.ok" sits beside it, fails to read a file named flaky* or one
// with a "<file>.flaky" beside it (an I/O error, which may pass), and reads
// anything else.
func rejectingProbe(t *testing.T) (probe, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "log")
	probe = stubProbe(t, fmt.Sprintf(`for f; do :; done
echo "$f" >> %[1]q
if [ -e "$f.flaky" ]; then echo "$f: Input/output error" >&2; exit 1; fi
case "${f##*/}" in
broken*) if [ ! -e "$f.ok" ]; then echo "[mov @ 0x1] moov atom not found" >&2; echo "$f: Invalid data found when processing input" >&2; exit 1; fi ;;
flaky*) echo "$f: Input/output error" >&2; exit 1 ;;
esac
echo '%[2]s'`, log, probeAnswer))
	return probe, log
}

// readProblems is the read problem of each book that has one, by path.
func readProblems(t *testing.T, cat *catalog.Catalog) map[string]string {
	t.Helper()
	page, err := cat.ListAdminBooks(t.Context(), catalog.AdminListOptions{Filter: catalog.BookFilter{Issue: catalog.IssueScanError}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, b := range page.Books {
		out[b.Path] = b.ScanError
	}
	return out
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
	problems := func() map[string]string { return readProblems(t, cat) }
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

// What a server remembers of a rejection goes once it reads the file otherwise.
// A rejected file mended in place (here: the stand-in reads it now, the file as
// it was) is read by its book's rescan, then fails to read on a flaky share: the
// scans after check it again, so its problem goes when the share comes back. For
// a book's one file and for a later part.
func TestRejectionForgottenOnceReadOtherwise(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, file string
		files      []string
	}{
		{"one file", "A/Book/broken.m4b", []string{"A/Book/broken.m4b"}},
		{"later part", "A/Book/broken02.m4b", []string{"A/Book/01.m4b", "A/Book/broken02.m4b", "A/Book/03.m4b"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			probe, log := rejectingProbe(t)
			root := t.TempDir()
			audio := testAudio(t)
			for _, f := range c.files {
				writeFile(t, root, f, audio)
			}
			cat, ctx := newHealthCatalog(t)
			lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
			s := NewScanner(cat, probe, slog.New(slog.DiscardHandler))
			problem := func(when string, want string) {
				t.Helper()
				if got := readProblems(t, cat)["A/Book"]; got != want {
					t.Fatalf("%s: problem %q, want %q", when, got, want)
				}
			}
			for range 3 {
				if _, err := s.Scan(ctx, *lib); err != nil {
					t.Fatal(err)
				}
			}
			problem("rejected", problemProbe)
			if n := probedRuns(t, log, "broken"); n > 2 {
				t.Fatalf("the rejected file probed %d times over three scans: not remembered", n)
			}

			writeFile(t, root, c.file+".ok", nil)
			if _, err := s.IndexPath(ctx, *lib, "A/Book"); err != nil {
				t.Fatal(err)
			}
			problem("read by the book's rescan", "")
			writeFile(t, root, c.file+".flaky", nil)
			if _, err := s.IndexPath(ctx, *lib, "A/Book"); err != nil {
				t.Fatal(err)
			}
			problem("read on a flaky share", problemProbe)

			if err := os.Remove(filepath.Join(root, filepath.FromSlash(c.file+".flaky"))); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			problem("the share back", "")
		})
	}
}

// A rejection is remembered by the file's absolute path and its own mtime and
// size, not by its book's: a later part put back from a backup (its own mtime
// back to what it was, the book's newest part and total size as they were), and
// the library pointed at a restored copy (the same names, sizes and mtimes), are
// each read again by the same server's next scan.
func TestRejectionRememberedByTheFileItself(t *testing.T) {
	t.Parallel()
	audio := testAudio(t)
	setup := func(t *testing.T, root string, files ...string) (*Scanner, *catalog.Catalog, *catalog.Library, string) {
		t.Helper()
		probe, log := rejectingProbe(t)
		cat, ctx := newHealthCatalog(t)
		lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
		s := NewScanner(cat, probe, slog.New(slog.DiscardHandler))
		for range 3 {
			if _, err := s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
		}
		if n := probedRuns(t, log, "broken"); n > 2 {
			t.Fatalf("the rejected file probed %d times over three scans: not remembered", n)
		}
		return s, cat, lib, log
	}
	rescan := func(t *testing.T, s *Scanner, cat *catalog.Catalog, lib catalog.Library) {
		t.Helper()
		res, err := s.Scan(t.Context(), lib)
		if err != nil {
			t.Fatal(err)
		}
		if got := readProblems(t, cat); res.Updated != 1 || len(got) != 0 {
			t.Fatalf("scan = %+v, problems %v: want the book read again, its problem gone", res.ScanCounts, got)
		}
	}

	t.Run("part put back", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		newest := time.Now().Add(-time.Minute).Truncate(time.Second)
		was := newest.Add(-72 * time.Hour)
		writeAt(t, root, "A/Book/01.m4b", string(audio), was)
		writeAt(t, root, "A/Book/broken02.m4b", string(audio), newest.Add(-24*time.Hour)) // damaged a day ago
		writeAt(t, root, "A/Book/03.m4b", string(audio), newest)
		s, cat, lib, _ := setup(t, root)
		writeFile(t, root, "A/Book/broken02.m4b.ok", nil)
		if err := os.Chtimes(filepath.Join(root, "A", "Book", "broken02.m4b"), was, was); err != nil {
			t.Fatal(err)
		}
		rescan(t, s, cat, *lib)
	})

	t.Run("restored copy", func(t *testing.T) {
		t.Parallel()
		root, copied := t.TempDir(), t.TempDir()
		at := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
		writeAt(t, root, "A/Book/broken.m4b", string(audio), at)
		writeAt(t, copied, "A/Book/broken.m4b", string(audio), at)
		writeFile(t, copied, "A/Book/broken.m4b.ok", nil) // the copy's bytes read
		s, cat, lib, _ := setup(t, root)
		if _, _, err := cat.UpdateLibrary(t.Context(), lib.ID, catalog.LibraryPatch{Name: lib.Name, Root: copied, DefaultView: lib.DefaultView}); err != nil {
			t.Fatal(err)
		}
		moved, err := cat.GetLibrary(t.Context(), lib.ID)
		if err != nil {
			t.Fatal(err)
		}
		rescan(t, s, cat, *moved)
	})
}

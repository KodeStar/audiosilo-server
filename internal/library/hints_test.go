package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A scan reads a book's ASIN, ISBN and language tags as hints (books.tag_*):
// never its identifiers, so the book is still unmatched (a match run's), and
// the hints never reach the wire. A book indexed before migration 0067 has
// them read once by the next scan, without a re-index.
func TestScanReadsTagHints(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	silence(t, root, "Herbert/Dune/01.mp3", 1, "title=Dune", "ASIN=B0TAGASIN1", "ISBN=9780441013593", "language=eng")
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	hints := func() catalog.TagHints {
		t.Helper()
		var h catalog.TagHints
		if err := db.QueryRowContext(ctx, `SELECT tag_asin, tag_isbn, tag_language FROM books WHERE rel_path = 'Herbert/Dune'`).
			Scan(&h.ASIN, &h.ISBN, &h.Language); err != nil {
			t.Fatal(err)
		}
		return h
	}
	want := catalog.TagHints{ASIN: "B0TAGASIN1", ISBN: "9780441013593", Language: "eng"}
	if got := hints(); got != want {
		t.Fatalf("hints = %+v, want %+v", got, want)
	}

	// Not matched by them: no ASIN or ISBN, still a match run's book, and the
	// player's book carries none of them.
	unmatched, err := cat.UnmatchedBooks(ctx, lib.ID)
	if err != nil || len(unmatched) != 1 {
		t.Fatalf("unmatched = %+v, %v; want the tagged book", unmatched, err)
	}
	b := unmatched[0]
	if b.ASIN != "" || b.ISBN != "" || b.TagASIN != want.ASIN || b.TagISBN != want.ISBN || b.TagLanguage != want.Language {
		t.Fatalf("match subject = asin %q isbn %q hints %q %q %q", b.ASIN, b.ISBN, b.TagASIN, b.TagISBN, b.TagLanguage)
	}
	full, err := cat.GetBookByPath(ctx, lib.ID, "Herbert/Dune")
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(full)
	if full.TagASIN != want.ASIN || strings.Contains(string(wire), "B0TAGASIN1") || strings.Contains(string(wire), "9780441013593") {
		t.Fatalf("book hint %q, wire %s: want the hint read and never sent", full.TagASIN, wire)
	}

	// Rows from before 0067: no hints read yet. The next scan reads them
	// without re-indexing the (unchanged) book, then leaves it alone.
	if _, err := db.ExecContext(ctx, `UPDATE books SET tag_asin = '', tag_isbn = '', tag_language = '', hints_checked = 0`); err != nil {
		t.Fatal(err)
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil || res.Updated != 0 {
		t.Fatalf("rescan = %+v (err %v), want the hints backfilled, nothing re-indexed", res.ScanCounts, err)
	}
	if got := hints(); got != want {
		t.Fatalf("after the backfill hints = %+v, want %+v", got, want)
	}
	var unchecked int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE hints_checked = 0`).Scan(&unchecked); err != nil || unchecked != 0 {
		t.Fatalf("unchecked after the backfill = %d (err %v), want 0", unchecked, err)
	}

	// A re-index reads them again and marks them checked, a tag gone included.
	if _, err := db.ExecContext(ctx, `UPDATE books SET hints_checked = 0`); err != nil {
		t.Fatal(err)
	}
	silence(t, root, "Herbert/Dune/01.mp3", 2, "title=Dune", "ASIN=B0TAGASIN2")
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 1 {
		t.Fatalf("scan of the changed file = %+v (err %v), want it re-indexed", res.ScanCounts, err)
	}
	if got, want := hints(), (catalog.TagHints{ASIN: "B0TAGASIN2"}); got != want {
		t.Fatalf("after the re-index hints = %+v, want %+v", got, want)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE hints_checked = 0`).Scan(&unchecked); err != nil || unchecked != 0 {
		t.Fatalf("unchecked after the re-index = %d (err %v), want 0", unchecked, err)
	}
}

// uncheckedHints indexes the books (one file each, ASIN tag B0TAGASIN1) in a new
// library with a working ffprobe stand-in, which it returns, then forgets their
// hints as a row from before migration 0067 has none.
func uncheckedHints(t *testing.T, titles ...string) (*catalog.Catalog, *catalog.Library, *store.DB, string) {
	t.Helper()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	for _, title := range titles {
		silence(t, root, "Herbert/"+title+"/01.mp3", 1, "title="+title, "ASIN=B0TAGASIN1")
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	healthy := stubProbe(t, "echo '"+probeAnswer+"'")
	if _, err := NewScanner(cat, healthy, slog.Default()).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE books SET tag_asin = '', hints_checked = 0`); err != nil {
		t.Fatal(err)
	}
	return cat, lib, db, healthy
}

// countHinted is how many books have the tag's hint recorded, marked checked.
func countHinted(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM books WHERE hints_checked = 1 AND tag_asin = 'B0TAGASIN1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A scan stopped part way through the backfill keeps the hints it read, as it
// keeps a book's genres: the second read waits for the Stop, the third book is
// never read, and the next scan reads what is left.
func TestStoppedScanKeepsTagHints(t *testing.T) {
	t.Parallel()
	cat, lib, db, healthy := uncheckedHints(t, "One", "Two", "Three")
	dir := t.TempDir()
	starts, stop := filepath.Join(dir, "starts"), filepath.Join(dir, "stop")
	probe := stubProbe(t, fmt.Sprintf(`echo start >> %[1]q
if [ $(wc -l < %[1]q) -gt 1 ]; then i=0; while [ ! -e %[2]q ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done; fi
echo '%[3]s'`, starts, stop, probeAnswer))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if got, _ := os.ReadFile(starts); strings.Count(string(got), "start") > 1 {
				cancel()
				_ = os.WriteFile(stop, nil, 0o600)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if _, err := NewScanner(cat, probe, slog.Default()).Scan(ctx, *lib); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped scan: err = %v", err)
	}
	if n := countHinted(t, db); n != 2 {
		t.Fatalf("a stopped scan kept %d books' hints, want the 2 it read", n)
	}
	if _, err := NewScanner(cat, healthy, slog.Default()).Scan(t.Context(), *lib); err != nil {
		t.Fatal(err)
	}
	if n := countHinted(t, db); n != 3 {
		t.Fatalf("the next scan left %d books' hints, want 3", n)
	}
}

// While ffprobe fails, a book's hints wait for the next scan: what only ffprobe
// reads (an M4B's mdta items) would be recorded as none.
func TestTagHintsWaitForAFailingProbe(t *testing.T) {
	t.Parallel()
	cat, lib, db, healthy := uncheckedHints(t, "One")
	failing := stubProbe(t, `echo "Input/output error" >&2; exit 1`)
	if _, err := NewScanner(cat, failing, slog.Default()).Scan(t.Context(), *lib); err != nil {
		t.Fatal(err)
	}
	var unchecked int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM books WHERE hints_checked = 0 AND tag_asin = ''`).Scan(&unchecked); err != nil || unchecked != 1 {
		t.Fatalf("unchecked with ffprobe failing = %d (err %v), want the book left for the next scan", unchecked, err)
	}
	if _, err := NewScanner(cat, healthy, slog.Default()).Scan(t.Context(), *lib); err != nil {
		t.Fatal(err)
	}
	if n := countHinted(t, db); n != 1 {
		t.Fatalf("hints after the next scan: %d books, want the book's", n)
	}
}

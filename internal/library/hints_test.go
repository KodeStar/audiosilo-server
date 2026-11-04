package library

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
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
}

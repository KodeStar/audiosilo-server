package library

import (
	"log/slog"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A scan reads a book's date tag into Released and leaves Published (the work's
// first publication: an edit or a community match) alone; a book indexed before
// migration 0037 has its date read once by the next scan, without a re-index.
func TestScanReadsReleaseDate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	silence(t, root, "Herbert/Dune/01.mp3", 1, "date=2007-03-06")
	silence(t, root, "Herbert/Messiah/01.mp3", 1, "date=0000")
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if err := cat.EditBook(ctx, lib.ID, "Herbert/Dune",
		catalog.BookEdit{Set: map[string]string{catalog.FieldPublished: "1965"}}); err != nil {
		t.Fatal(err)
	}
	dates := func() map[string][2]string {
		t.Helper()
		page, err := cat.ListAdminBooks(ctx, catalog.AdminListOptions{Sort: "published"})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][2]string{}
		for _, b := range page.Books {
			out[b.Path] = [2]string{b.Published, b.Released}
		}
		return out
	}
	want := map[string][2]string{"Herbert/Dune": {"1965", "2007-03-06"}, "Herbert/Messiah": {"", ""}}
	if got := dates(); got["Herbert/Dune"] != want["Herbert/Dune"] || got["Herbert/Messiah"] != want["Herbert/Messiah"] {
		t.Fatalf("dates = %v, want %v (a placeholder tag is no date)", got, want)
	}

	// Rows from before 0037: no release date read yet. The next scan reads it
	// without re-indexing the (unchanged) books.
	if _, err := db.ExecContext(ctx, `UPDATE books SET released = '', released_checked = 0`); err != nil {
		t.Fatal(err)
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil || res.Updated != 0 {
		t.Fatalf("rescan = %+v (err %v), want the dates backfilled, nothing re-indexed", res.ScanCounts, err)
	}
	if got := dates(); got["Herbert/Dune"] != want["Herbert/Dune"] {
		t.Fatalf("after the backfill Dune = %v, want %v (the edit kept, the tag's date back)", got["Herbert/Dune"], want["Herbert/Dune"])
	}
	var unchecked int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE released_checked = 0`).Scan(&unchecked); err != nil || unchecked != 0 {
		t.Fatalf("unchecked after the backfill = %d (err %v), want 0", unchecked, err)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 0 {
		t.Fatalf("third scan = %+v (err %v), want nothing re-indexed once checked", res.ScanCounts, err)
	}
}

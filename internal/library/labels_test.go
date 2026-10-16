package library

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A scan reads a book's genre tag (placeholders dropped, several in one tag
// split) and an Audiobookshelf metadata.json's subtitle, publisher, language,
// abridged, genres and tags; a book indexed before migration 0049 gets both on
// the next scan, its genre tag read once and its ABS file again, without a
// re-index, and an edit made meanwhile is kept.
func TestScanReadsLabels(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	silence(t, root, "Herbert/Dune/01.mp3", 1, "genre=Audiobook;Science Fiction")
	silence(t, root, "Pratchett/Mort/01.mp3", 1, "genre=Fantasy")
	if err := os.WriteFile(filepath.Join(root, "Pratchett", "Mort", "metadata.json"), []byte(`{
		"subtitle": "A Discworld Novel", "publisher": "Corgi", "language": "English", "abridged": true,
		"genres": ["Fantasy", "Humour"], "tags": ["Owned", "Favourite"]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		dune, err := cat.GetBookByPath(ctx, lib.ID, "Herbert/Dune")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(dune.Genres, []string{"Science Fiction"}) {
			t.Errorf("%s: Dune genres = %q, want [Science Fiction]", when, dune.Genres)
		}
		mort, err := cat.GetBookByPath(ctx, lib.ID, "Pratchett/Mort")
		if err != nil {
			t.Fatal(err)
		}
		if mort.Subtitle != "A Discworld Novel" || mort.Publisher != "Corgi" || mort.Language != "English" ||
			mort.Abridged == nil || !*mort.Abridged || !slices.Equal(mort.Genres, []string{"Fantasy", "Humour"}) ||
			!slices.Equal(mort.Tags, []string{"Mine"}) {
			t.Errorf("%s: Mort = subtitle %q publisher %q language %q abridged %v genres %q tags %q", when,
				mort.Subtitle, mort.Publisher, mort.Language, mort.Abridged, mort.Genres, mort.Tags)
		}
	}
	if err := cat.EditBook(ctx, lib.ID, "Pratchett/Mort",
		catalog.BookEdit{Set: map[string]string{catalog.FieldTags: `["Mine"]`}}); err != nil {
		t.Fatal(err)
	}
	check("indexed")

	// Rows from before 0049: no genre read, an ABS reading without the new fields
	// (and an effective row without them). The next scan puts them back without
	// re-indexing the (unchanged) books.
	if _, err := db.ExecContext(ctx, `UPDATE books SET genres_checked = 0, abs_indexed_at = NULL,
		abs_file = CASE WHEN abs_file = '' THEN '' ELSE
		    json_remove(abs_file, '$.subtitle', '$.publisher', '$.language', '$.abridged', '$.genres', '$.tags') END,
		scanned = json_remove(scanned, '$.genres'), subtitle = '', publisher = '', language = '', abridged = NULL,
		genres = '', tags = ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM book_labels`); err != nil {
		t.Fatal(err)
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil || res.Updated != 0 {
		t.Fatalf("rescan = %+v (err %v), want the labels backfilled, nothing re-indexed", res.ScanCounts, err)
	}
	check("backfilled")
	var unchecked int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE genres_checked = 0`).Scan(&unchecked); err != nil || unchecked != 0 {
		t.Fatalf("unchecked after the backfill = %d (err %v), want 0", unchecked, err)
	}
	page, err := cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID, Genre: "science fiction"})
	if err != nil || len(page.Books) != 1 || page.Books[0].RelPath != "Herbert/Dune" {
		t.Fatalf("genre filter after the backfill = %+v (err %v)", page, err)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 0 {
		t.Fatalf("third scan = %+v (err %v), want nothing re-indexed once checked", res.ScanCounts, err)
	}
}

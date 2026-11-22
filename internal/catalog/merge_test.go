package catalog

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A failure on the second book, after the first one's effective row has
// changed, rolls back both the index and the overrides used by future scans.
func TestMergeSpellingsRollback(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"A", "B"} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, path)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.ExecContext(ctx, `CREATE TRIGGER refuse_merge BEFORE INSERT ON book_overrides
		WHEN NEW.path = 'B' AND EXISTS(SELECT 1 FROM books WHERE rel_path = 'A' AND author = 'New Author')
		BEGIN SELECT RAISE(ABORT, 'second book refused'); END`); err != nil {
		t.Fatal(err)
	}
	changes := 0
	c.OnBookChange(func() { changes++ })
	books, err := c.MergeSpellings(ctx, lib.ID, FieldAuthor, []string{"Tag Author"}, "New Author", 0)
	if err == nil || !strings.Contains(err.Error(), "second book refused") || books != nil || changes != 0 {
		t.Fatalf("failed merge returned %v, %v and notified %d times", books, err, changes)
	}
	for _, path := range []string{"A", "B"} {
		if b := mustBook(t, c, ctx, lib.ID, path); b.Author != "Tag Author" {
			t.Fatalf("author after rollback at %s = %q", path, b.Author)
		}
		if overrides, err := queryOverrides(ctx, c.db, lib.ID, path); err != nil || len(overrides) != 0 {
			t.Fatalf("overrides after rollback at %s = %v, %v", path, overrides, err)
		}
	}
}

func TestMergeSpellingsScope(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	library := func(name, kind string) int64 {
		t.Helper()
		lib, err := c.CreateLibrary(ctx, Library{Name: name, Root: t.TempDir(), Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		for path, credit := range map[string]string{"Solo": "Reader", "Duo": "Reader & Friend", "Target": "R. Reader"} {
			b := scannedBook(lib.ID, path)
			b.Narrator = credit
			if _, err := c.UpsertBook(ctx, b); err != nil {
				t.Fatal(err)
			}
		}
		return lib.ID
	}
	a, b, pods := library("A", KindBooks), library("B", KindBooks), library("Podcasts", KindPodcasts)
	changes := 0
	c.OnBookChange(func() { changes++ })
	merge := func(scope int64) []MergedBook {
		t.Helper()
		books, err := c.MergeSpellings(ctx, scope, FieldNarrator, []string{"Reader", "R. Reader"}, "R. Reader", 0)
		if err != nil {
			t.Fatal(err)
		}
		return books
	}
	if got := merge(a); len(got) != 1 || got[0].Ref != (Ref{a, "Solo"}) {
		t.Fatalf("scoped merge = %+v", got)
	}
	if got := merge(0); len(got) != 1 || got[0].Ref != (Ref{b, "Solo"}) {
		t.Fatalf("all-libraries merge = %+v", got)
	}
	if got := merge(0); len(got) != 0 || changes != 2 {
		t.Fatalf("repeat merge = %+v, change notifications = %d", got, changes)
	}
	for _, lib := range []int64{a, b} {
		if got := mustBook(t, c, ctx, lib, "Duo").Narrator; got != "Reader & Friend" {
			t.Fatalf("co-credit = %q", got)
		}
	}
	if got := mustBook(t, c, ctx, pods, "Solo").Narrator; got != "Reader" {
		t.Fatalf("podcast narrator = %q", got)
	}
	// A whole co-credit is a spelling too, matched as written (json.Marshal sends
	// its "&" escaped).
	if got, err := c.MergeSpellings(ctx, a, FieldNarrator, []string{"Reader & Friend"}, "R. Reader & Friend", 0); err != nil ||
		len(got) != 1 || got[0].Ref != (Ref{a, "Duo"}) {
		t.Fatalf("co-credit merge = %+v, %v", got, err)
	}
	if got := mustBook(t, c, ctx, a, "Duo").Narrator; got != "R. Reader & Friend" {
		t.Fatalf("merged co-credit = %q", got)
	}
}

// A suggestion can cover more than the bulk endpoint's 1,000 explicit paths.
func TestMergeSpellingsLargeLibrary(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const count = 1001
	if _, err := c.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS
		(SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
		INSERT INTO books(library_id, rel_path, title, author, indexed_at)
		SELECT ?, 'A/' || i, 'Book ' || i, 'Weir, Andy', '2024-01-01T00:00:00Z' FROM n`, count, lib.ID); err != nil {
		t.Fatal(err)
	}
	books, err := c.MergeSpellings(ctx, lib.ID, FieldAuthor, []string{"Weir, Andy"}, "Andy Weir", 0)
	if err != nil || len(books) != count {
		t.Fatalf("merged %d/%d books, %v", len(books), count, err)
	}
	var updated int
	if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM books WHERE author='Andy Weir'`).Scan(&updated); err != nil || updated != count {
		t.Fatalf("updated %d/%d effective credits, %v", updated, count, err)
	}
}

// A merge names an existing books library, and stops before writing anything
// past MaxMergeBooks books.
func TestMergeSpellingsBounds(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	pods, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	for scope, want := range map[int64]error{lib.ID + pods.ID + 1: ErrNotFound, pods.ID: ErrNotBookLibrary} {
		if _, err := c.MergeSpellings(ctx, scope, FieldAuthor, []string{"Weir, Andy"}, "Andy Weir", 0); !errors.Is(err, want) {
			t.Fatalf("merge in library %d = %v, want %v", scope, err, want)
		}
	}
	if _, err := c.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS
		(SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
		INSERT INTO books(library_id, rel_path, title, author, indexed_at)
		SELECT ?, 'A/' || i, 'Book ' || i, 'Weir, Andy', '2024-01-01T00:00:00Z' FROM n`, MaxMergeBooks+1, lib.ID); err != nil {
		t.Fatal(err)
	}
	if books, err := c.MergeSpellings(ctx, lib.ID, FieldAuthor, []string{"Weir, Andy"}, "Andy Weir", 0); !errors.Is(err, ErrMergeTooLarge) || books != nil {
		t.Fatalf("oversized merge = %d books, %v", len(books), err)
	}
	var overrides int
	if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM book_overrides`).Scan(&overrides); err != nil || overrides != 0 {
		t.Fatalf("overrides after a refused merge = %d, %v", overrides, err)
	}
}

// Undo puts back every book still carrying the merge, each with its own
// previous override and source, and leaves (and names) a book gone or edited
// since.
func TestUndoMerge(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"Tags", "Community", "Edited", "Gone"} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EditBook(ctx, lib.ID, "Community", BookEdit{Set: map[string]string{FieldAuthor: "T. Author"}, Source: SourceCommunity}); err != nil {
		t.Fatal(err)
	}
	merged, err := c.MergeSpellings(ctx, lib.ID, FieldAuthor, []string{"Tag Author", "T. Author"}, "New Author", 0)
	if err != nil || len(merged) != 4 {
		t.Fatalf("merge = %+v, %v", merged, err)
	}
	if err := c.EditBook(ctx, lib.ID, "Edited", BookEdit{Set: map[string]string{FieldAuthor: "Hand Edit"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `DELETE FROM books WHERE library_id = ? AND rel_path = 'Gone'`, lib.ID); err != nil {
		t.Fatal(err)
	}
	changes := 0
	c.OnBookChange(func() { changes++ })
	done, err := c.UndoMerge(ctx, FieldAuthor, " New Author ", append(merged, merged[0]), 0)
	want := UndoneMerge{Restored: 2, Skipped: []UndoneSkip{
		{Ref{lib.ID, "Edited"}, UndoChanged}, {Ref{lib.ID, "Gone"}, UndoGone},
	}}
	if err != nil || !reflect.DeepEqual(done, want) || changes != 1 {
		t.Fatalf("undo = %+v, %v (%d notifications), want %+v", done, err, changes, want)
	}
	for path, author := range map[string]string{"Tags": "Tag Author", "Community": "T. Author", "Edited": "Hand Edit"} {
		if got := mustBook(t, c, ctx, lib.ID, path).Author; got != author {
			t.Fatalf("author at %s after undo = %q, want %q", path, got, author)
		}
	}
	if o, err := queryOverrides(ctx, c.db, lib.ID, "Tags"); err != nil || len(o) != 0 {
		t.Fatalf("overrides at Tags after undo = %v, %v", o, err)
	}
	if o, err := queryOverrides(ctx, c.db, lib.ID, "Community"); err != nil || o[FieldAuthor].Source != SourceCommunity {
		t.Fatalf("overrides at Community after undo = %+v, %v", o, err)
	}

	// A book that refuses its edit undoes nothing at all, not even the book put
	// back before it.
	if again, err := c.MergeSpellings(ctx, lib.ID, FieldAuthor, []string{"Tag Author", "T. Author"}, "New Author", 0); err != nil || len(again) != 2 {
		t.Fatalf("second merge = %+v, %v", again, err)
	}
	if _, err := c.db.ExecContext(ctx, `CREATE TRIGGER refuse_undo BEFORE DELETE ON book_overrides
		WHEN OLD.path = 'Tags' BEGIN SELECT RAISE(ABORT, 'undo refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UndoMerge(ctx, FieldAuthor, "New Author", merged, 0); err == nil || !strings.Contains(err.Error(), "undo refused") {
		t.Fatalf("refused undo = %v", err)
	}
	for _, path := range []string{"Community", "Tags"} {
		if got := mustBook(t, c, ctx, lib.ID, path).Author; got != "New Author" {
			t.Fatalf("author at %s after a refused undo = %q", path, got)
		}
	}
}

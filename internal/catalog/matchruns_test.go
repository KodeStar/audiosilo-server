package catalog

import (
	"errors"
	"strings"
	"testing"
)

// TestRecordMatchItemDeletedLibrary: a library deleted while a run over every
// library works leaves its books' items out instead of failing the run, and a run
// whose own library was deleted (and the run with it) answers ErrNotFound.
func TestRecordMatchItemDeletedLibrary(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	keep, _ := c.CreateLibrary(ctx, Library{Name: "Keep", Root: "/tmp/keep"})
	gone, _ := c.CreateLibrary(ctx, Library{Name: "Gone", Root: "/tmp/gone"})
	all, err := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, Total: 2})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, LibraryID: &gone.ID, Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteLibrary(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}

	for _, lib := range []int64{keep.ID, gone.ID} {
		item := &MatchRunItem{LibraryID: lib, Path: "A/B", Outcome: OutcomeNone}
		if err := c.RecordMatchItem(ctx, all, item); err != nil {
			t.Fatalf("record an item of library %d: %v", lib, err)
		}
	}
	run, err := c.GetMatchRun(ctx, all)
	if err != nil || run.Done != 2 || run.Counts.None != 1 {
		t.Fatalf("run = %+v %v, want both books done and one item", run, err)
	}

	err = c.RecordMatchItem(ctx, scoped, &MatchRunItem{LibraryID: gone.ID, Path: "A/B", Outcome: OutcomeNone})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("record into a deleted run = %v, want ErrNotFound", err)
	}
}

// TestClearCommunityMatches: clearing one library removes its community overrides
// and covers and rebuilds its books (the scanned values back, an admin's own edit,
// upload and the manager's enrichment kept), drops its match runs and its books'
// items in a run over every library, and leaves the other library alone; clearing
// every library then takes the rest.
func TestClearCommunityMatches(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	one, _ := c.CreateLibrary(ctx, Library{Name: "One", Root: "/tmp/one"})
	two, _ := c.CreateLibrary(ctx, Library{Name: "Two", Root: "/tmp/two"})
	for _, ref := range []Ref{{one.ID, "A/Matched"}, {one.ID, "A/Uploaded"}, {one.ID, "A/Enriched"}, {two.ID, "B/Other"}} {
		if _, err := c.UpsertBook(ctx, scannedBook(ref.LibraryID, ref.Path)); err != nil {
			t.Fatal(err)
		}
	}
	community := func(libID int64, path string, set map[string]string) {
		t.Helper()
		if err := c.EditBook(ctx, libID, path, BookEdit{Set: set, Source: SourceCommunity}); err != nil {
			t.Fatal(err)
		}
	}
	community(one.ID, "A/Matched", map[string]string{FieldASIN: "B000000001", FieldTitle: "Community Title"})
	if err := c.EditBook(ctx, one.ID, "A/Matched", BookEdit{Set: map[string]string{FieldNarrator: "My Reader"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCover(ctx, one.ID, "A/Matched", pngBytes, 0, SourceCommunity); err != nil {
		t.Fatal(err)
	}
	community(one.ID, "A/Uploaded", map[string]string{FieldISBN: "9780000000002"})
	if err := c.SetCover(ctx, one.ID, "A/Uploaded", pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	if err := c.SetEnrichment(ctx, one.ID, "A/Enriched", "B000000003", ""); err != nil {
		t.Fatal(err)
	}
	community(two.ID, "B/Other", map[string]string{FieldASIN: "B000000004"})
	// A community value left at a path with no book indexed now.
	if _, err := c.db.ExecContext(ctx, `INSERT INTO book_overrides(library_id, path, field, value, source, updated_at)
		VALUES(?, 'A/Gone', 'asin', 'B000000005', 'community', 't')`, one.ID); err != nil {
		t.Fatal(err)
	}
	oneRun, _ := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, LibraryID: &one.ID, Total: 1})
	twoRun, _ := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, LibraryID: &two.ID, Total: 1})
	allRun, _ := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, Total: 2})
	for _, ref := range []Ref{{one.ID, "A/Matched"}, {two.ID, "B/Other"}} {
		if err := c.RecordMatchItem(ctx, allRun, &MatchRunItem{LibraryID: ref.LibraryID, Path: ref.Path, Outcome: OutcomeNone}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := c.ClearCommunityMatches(ctx, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ClearedMatches{Books: 3, Covers: 1, Runs: 1}); *got != want {
		t.Fatalf("cleared = %+v, want %+v", *got, want)
	}
	b := mustBook(t, c, ctx, one.ID, "A/Matched")
	if b.ASIN != "" || b.Title != "Original Name" || b.Narrator != "My Reader" {
		t.Errorf("matched book = asin %q title %q narrator %q, want the scan's values and the edit kept", b.ASIN, b.Title, b.Narrator)
	}
	if _, err := c.Cover(ctx, one.ID, "A/Matched"); !errors.Is(err, ErrNotFound) {
		t.Errorf("community cover = %v, want removed", err)
	}
	var art string
	if err := c.db.QueryRowContext(ctx, `SELECT cover_art FROM books WHERE library_id = ? AND rel_path = 'A/Matched'`,
		one.ID).Scan(&art); err != nil || strings.HasPrefix(art, "c") {
		t.Errorf("cover art = %q %v, want the book's own art again", art, err)
	}
	if titles := searchTitles(t, c, ctx, one.ID, "Community"); len(titles) != 0 {
		t.Errorf("search still finds the community title: %v", titles)
	}
	if b := mustBook(t, c, ctx, one.ID, "A/Uploaded"); b.ISBN != "" {
		t.Errorf("isbn = %q, want the community one cleared", b.ISBN)
	}
	if _, err := c.Cover(ctx, one.ID, "A/Uploaded"); err != nil {
		t.Errorf("uploaded cover = %v, want kept", err)
	}
	if b := mustBook(t, c, ctx, one.ID, "A/Enriched"); b.ASIN != "B000000003" {
		t.Errorf("enriched asin = %q, want the manager's kept", b.ASIN)
	}
	if b := mustBook(t, c, ctx, two.ID, "B/Other"); b.ASIN != "B000000004" {
		t.Errorf("other library's asin = %q, want untouched", b.ASIN)
	}
	unmatched, err := c.UnmatchedBooks(ctx, one.ID)
	if err != nil || len(unmatched) != 2 {
		t.Errorf("unmatched = %d books %v, want the two cleared ones back", len(unmatched), err)
	}
	if _, err := c.GetMatchRun(ctx, oneRun); !errors.Is(err, ErrNotFound) {
		t.Errorf("the library's run = %v, want removed", err)
	}
	if _, err := c.GetMatchRun(ctx, twoRun); err != nil {
		t.Errorf("the other library's run = %v, want kept", err)
	}
	if run, err := c.GetMatchRun(ctx, allRun); err != nil || run.Counts.None != 1 {
		t.Errorf("the run over every library = %+v %v, want only the other library's item left", run, err)
	}

	got, err = c.ClearCommunityMatches(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ClearedMatches{Books: 1, Covers: 0, Runs: 2}); *got != want {
		t.Fatalf("cleared everything = %+v, want %+v", *got, want)
	}
	if b := mustBook(t, c, ctx, two.ID, "B/Other"); b.ASIN != "" {
		t.Errorf("other library's asin = %q, want cleared", b.ASIN)
	}
	if runs, err := c.ListMatchRuns(ctx); err != nil || len(runs) != 0 {
		t.Errorf("runs = %d %v, want none", len(runs), err)
	}
}

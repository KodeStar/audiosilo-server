package catalog

import (
	"errors"
	"fmt"
	"maps"
	"slices"
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

// TestRecordMatchItemKeepsTheContentGeneration: a run recording its books is
// user state (server#307), so the list caches stay warm through it; starting
// and finishing the run move the generation as before.
func TestRecordMatchItemKeepsTheContentGeneration(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "One", Root: "/tmp/one"})
	gen := func() uint64 {
		t.Helper()
		g, ok := c.ContentGeneration()
		if !ok {
			t.Fatal("the test store counts no content generation")
		}
		return g
	}
	before := gen()
	run, err := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, Total: 3})
	if err != nil {
		t.Fatal(err)
	}
	if gen() == before {
		t.Fatal("starting a run left the content generation")
	}
	started := gen()
	for i, item := range []*MatchRunItem{
		{LibraryID: lib.ID, Path: "A/1", Outcome: OutcomeAuto, Score: 96},
		{LibraryID: lib.ID, Path: "A/2", Outcome: OutcomeNone},
		nil,
	} {
		if err := c.RecordMatchItem(ctx, run, item); err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	if gen() != started {
		t.Error("recording a run's books moved the content generation")
	}
	if r, err := c.GetMatchRun(ctx, run); err != nil || r.Done != 3 || r.Counts.Auto != 1 {
		t.Fatalf("run = %+v %v", r, err)
	}
	if err := c.FinishMatching(ctx, run, MatchReady, ""); err != nil {
		t.Fatal(err)
	}
	if gen() == started {
		t.Error("finishing the run left the content generation")
	}
}

// TestListMatchRunItemsBestFirst: a run's items page best match first (score
// descending, a tie in the order they were recorded) across pages, so a review read
// a page at a time sees one order; an outcome reads only its own items, and an
// `after` from another run reads nothing.
func TestListMatchRunItemsBestFirst(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "One", Root: "/tmp/one"})
	run, err := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, Total: 7})
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i, score := range []int{70, 95, 70, 60, 95, 80} {
		item := &MatchRunItem{LibraryID: lib.ID, Path: fmt.Sprintf("A/%d-%d", i, score), Outcome: OutcomeReview, Score: score}
		if err := c.RecordMatchItem(ctx, run, item); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.RecordMatchItem(ctx, run, &MatchRunItem{LibraryID: lib.ID, Path: "A/none", Outcome: OutcomeNone}); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordMatchItem(ctx, other, &MatchRunItem{LibraryID: lib.ID, Path: "B/other", Outcome: OutcomeReview, Score: 99}); err != nil {
		t.Fatal(err)
	}

	var got []string
	var after int64
	for range 10 {
		items, next, err := c.ListMatchRunItems(ctx, run, OutcomeReview, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			got = append(got, it.Path)
		}
		if after = next; after == 0 {
			break
		}
	}
	if want := "A/1-95,A/4-95,A/5-80,A/0-70,A/2-70,A/3-60"; strings.Join(got, ",") != want {
		t.Errorf("pages = %s, want %s", strings.Join(got, ","), want)
	}

	foreign, _, err := c.ListMatchRunItems(ctx, other, OutcomeReview, 0, 10)
	if err != nil || len(foreign) != 1 {
		t.Fatalf("other run = %d items %v, want its one", len(foreign), err)
	}
	if items, next, err := c.ListMatchRunItems(ctx, run, OutcomeReview, foreign[0].ID, 10); err != nil || len(items) != 0 || next != 0 {
		t.Errorf("after another run's item = %d items next %d %v, want none", len(items), next, err)
	}
}

// TestMarkStillToApplyKeepsTheHolds (server#352): a mark that leaves an item
// still to apply (a stop cut its cover short) keeps every reason the run held it
// for behind its own detail, a lone one too and through a second such mark, so
// the review goes on saying them and the item is still taken; an item held for
// nothing gains none, and a final mark's detail replaces them.
func TestMarkStillToApplyKeepsTheHolds(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "One", Root: "/tmp/one"})
	run, err := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, Total: 3})
	if err != nil {
		t.Fatal(err)
	}
	both := []string{"identifier_disagrees", "length_differs"}
	for _, it := range []MatchRunItem{
		{LibraryID: lib.ID, Path: "A/Both", Outcome: OutcomeReview, Detail: both[0], Holds: both},
		{LibraryID: lib.ID, Path: "A/One", Outcome: OutcomeReview, Detail: "length_differs"},
		{LibraryID: lib.ID, Path: "A/Sure", Outcome: OutcomeAuto},
	} {
		if err := c.RecordMatchItem(ctx, run, &it); err != nil {
			t.Fatal(err)
		}
	}
	items := func() map[string]MatchRunItem {
		t.Helper()
		got, _, err := c.ListMatchRunItems(ctx, run, "", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]MatchRunItem{}
		for _, it := range got {
			out[it.Path] = it
		}
		return out
	}
	recorded := items()
	ids := []int64{recorded["A/Both"].ID, recorded["A/One"].ID, recorded["A/Sure"].ID}
	want := map[string][]string{"A/Both": both, "A/One": {"length_differs"}, "A/Sure": nil}
	for range 2 { // a second stop finds the first one's mark
		for _, id := range ids {
			if err := c.MarkMatchItem(ctx, run, id, "", "cover_pending"); err != nil {
				t.Fatal(err)
			}
		}
		for p, it := range items() {
			if it.Applied != "" || it.Detail != "cover_pending" || !slices.Equal(it.Holds, want[p]) {
				t.Errorf("%s = %q %q %v, want cover_pending still to apply, held for %v", p, it.Applied, it.Detail, it.Holds, want[p])
			}
		}
	}
	if err := c.FinishMatching(ctx, run, MatchReady, ""); err != nil {
		t.Fatal(err)
	}
	if again, err := c.MatchItemsToApply(ctx, run, "fill", ids[:2], nil); err != nil || len(again) != 3 {
		t.Fatalf("to apply = %d items %v, want all three again", len(again), err)
	}
	if err := c.MarkMatchItem(ctx, run, ids[0], ItemApplied, "cover_failed"); err != nil {
		t.Fatal(err)
	}
	if it := items()["A/Both"]; it.Applied != ItemApplied || it.Detail != "cover_failed" || it.Holds != nil {
		t.Errorf("applied = %q %q %v, want what the apply did alone", it.Applied, it.Detail, it.Holds)
	}
}

// TestClearCommunityMatches: clearing one library removes its community overrides
// and covers and rebuilds its books (the scanned values back, an admin's own edit,
// upload and an enrichment kept), drops its match runs and its books'
// items in a run over every library (and such a run it leaves with none,
// server#214), counts only indexed books (a leftover at a gone path is no book),
// and leaves the other library alone; clearing every library then takes the rest.
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
	setEnrichment(t, c, one.ID, "A/Enriched", "B000000003", "")
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
	// A run over every library that only reached this library's books.
	emptiedRun, _ := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, Total: 1})
	if err := c.RecordMatchItem(ctx, emptiedRun, &MatchRunItem{LibraryID: one.ID, Path: "A/Uploaded", Outcome: OutcomeNone}); err != nil {
		t.Fatal(err)
	}

	got, err := c.ClearCommunityMatches(ctx, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ClearedMatches{Books: 2, Covers: 1, Runs: 2}); *got != want {
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
		t.Errorf("enriched asin = %q, want the enrichment's kept", b.ASIN)
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
	if _, err := c.GetMatchRun(ctx, emptiedRun); !errors.Is(err, ErrNotFound) {
		t.Errorf("the run over every library left empty = %v, want removed", err)
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

// TestMatchedBooks: a refresh run's books are those with an ASIN or ISBN (from an
// enrichment or an override alike), in the library asked for or in every one; a
// book with neither is left out.
func TestMatchedBooks(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	one, _ := c.CreateLibrary(ctx, Library{Name: "One", Root: "/tmp/one"})
	two, _ := c.CreateLibrary(ctx, Library{Name: "Two", Root: "/tmp/two"})
	for _, ref := range []Ref{{one.ID, "A/Enriched"}, {one.ID, "A/ISBN"}, {one.ID, "A/Plain"}, {two.ID, "B/Other"}} {
		if _, err := c.UpsertBook(ctx, scannedBook(ref.LibraryID, ref.Path)); err != nil {
			t.Fatal(err)
		}
	}
	setEnrichment(t, c, one.ID, "A/Enriched", "B000000001", "")
	if err := c.EditBook(ctx, one.ID, "A/ISBN", BookEdit{Set: map[string]string{FieldISBN: "9780000000002"}}); err != nil {
		t.Fatal(err)
	}
	setEnrichment(t, c, two.ID, "B/Other", "B000000003", "")
	paths := func(libraryID int64) []string {
		t.Helper()
		books, err := c.MatchedBooks(ctx, libraryID)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(books))
		for i, b := range books {
			out[i] = b.RelPath
		}
		return out
	}
	if got, want := strings.Join(paths(one.ID), ","), "A/Enriched,A/ISBN"; got != want {
		t.Errorf("library one = %s, want %s", got, want)
	}
	if got, want := strings.Join(paths(0), ","), "A/Enriched,A/ISBN,B/Other"; got != want {
		t.Errorf("every library = %s, want %s", got, want)
	}
}

// TestCommunityRevertKeepsAnEdit: a community edit's revert (a match clearing a
// series it wrote) removes only a community value, never an admin's own edit
// made since; an admin's revert removes either.
func TestCommunityRevertKeepsAnEdit(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "One", Root: "/tmp/one"})
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "A/Book")); err != nil {
		t.Fatal(err)
	}
	edit := func(source string, set map[string]string, revert ...string) {
		t.Helper()
		if err := c.EditBook(ctx, lib.ID, "A/Book", BookEdit{Source: source, Set: set, Revert: revert}); err != nil {
			t.Fatal(err)
		}
	}
	series := func() string {
		t.Helper()
		b, err := c.GetBookByPath(ctx, lib.ID, "A/Book")
		if err != nil {
			t.Fatal(err)
		}
		return b.Series
	}
	edit(SourceEdited, map[string]string{FieldSeries: "Mine"})
	edit(SourceCommunity, nil, FieldSeries)
	if got := series(); got != "Mine" {
		t.Fatalf("after a community revert = %q, want the admin's edit kept", got)
	}
	edit(SourceCommunity, map[string]string{FieldSeries: "Theirs"})
	edit(SourceCommunity, nil, FieldSeries)
	if got := series(); got == "Theirs" {
		t.Fatalf("after a community revert = %q, want the community value gone", got)
	}
	edit(SourceEdited, map[string]string{FieldSeries: "Mine"})
	edit(SourceEdited, nil, FieldSeries)
	if got := series(); got == "Mine" {
		t.Fatalf("after an admin revert = %q, want the edit gone", got)
	}
}

// Security: EditedValue answers only an admin's own edit (never a tag's, the
// path's, an ABS file's or the community's value, nor a field with none) that
// says the same as the value asked about, and returns the book's own spelling
// of it: a series position or an other series list compared as read, anything
// else exactly.
func TestMatchStateEditedValue(t *testing.T) {
	t.Parallel()
	more := EncodeMoreSeries([]SeriesRef{{Name: "Cosmere", Position: 2}})
	st := &MatchState{Fields: map[string]FieldValue{
		FieldTitle:       {Value: "Mistborn", Source: SourceEdited},
		FieldAuthor:      {Value: "Brandon Sanderson", Source: SourceCommunity},
		FieldNarrator:    {Value: "Michael Kramer", Source: SourceTag},
		FieldSeries:      {Value: "Mistborn", Source: SourcePath},
		FieldPublished:   {Value: "2006", Source: SourceABS},
		FieldSeriesIndex: {Value: "1", Source: SourceEdited},
		FieldMoreSeries:  {Value: more, Source: SourceEdited},
	}}
	for _, c := range []struct {
		field, value, want string
		ok                 bool
	}{
		{FieldTitle, "Mistborn", "Mistborn", true},
		{FieldTitle, "mistborn", "", false},
		{FieldSeriesIndex, "1.0", "1", true},
		{FieldSeriesIndex, "2", "", false},
		{FieldMoreSeries, `[{"name":"Cosmere","position":2.0}]`, more, true},
		{FieldMoreSeries, `[{"name":"Cosmere","position":3}]`, "", false},
		{FieldAuthor, "Brandon Sanderson", "", false},
		{FieldNarrator, "Michael Kramer", "", false},
		{FieldSeries, "Mistborn", "", false},
		{FieldPublished, "2006", "", false},
		{FieldISBN, "", "", false},
	} {
		if got, ok := st.EditedValue(c.field, c.value); got != c.want || ok != c.ok {
			t.Errorf("EditedValue(%s, %q) = %q, %t; want %q, %t", c.field, c.value, got, ok, c.want, c.ok)
		}
	}
}

// TestLanguageCounts: a library's books counted by the language each says (its own,
// and its tags'), every book of the library asked for, matched or not, none of
// another; and a match run's books carry their genres.
func TestLanguageCounts(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	one, _ := c.CreateLibrary(ctx, Library{Name: "One", Root: "/tmp/one"})
	two, _ := c.CreateLibrary(ctx, Library{Name: "Two", Root: "/tmp/two"})
	for _, b := range []*Book{
		{LibraryID: one.ID, RelPath: "A/1", TagLanguage: "German", Genres: []string{"Radio Drama"}},
		{LibraryID: one.ID, RelPath: "A/2", TagLanguage: "German", ASIN: "B000000001"},
		{LibraryID: one.ID, RelPath: "A/3"},
		{LibraryID: two.ID, RelPath: "B/1", TagLanguage: "English"},
	} {
		b.Title, b.IsFolder = b.RelPath, true
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET language = 'fr' WHERE rel_path = 'A/3'`); err != nil {
		t.Fatal(err)
	}
	got, err := c.LanguageCounts(ctx, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b LanguageCount) int {
		return strings.Compare(a.Language+a.TagLanguage, b.Language+b.TagLanguage)
	})
	if want := []LanguageCount{{"", "German", 2}, {"fr", "", 1}}; !slices.Equal(got, want) {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
	books, err := c.UnmatchedBooks(ctx, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	genres := map[string][]string{}
	for _, b := range books {
		genres[b.RelPath] = b.Genres
	}
	if !slices.Equal(genres["A/1"], []string{"Radio Drama"}) || len(genres["A/3"]) != 0 {
		t.Errorf("genres = %q, want the book's", genres)
	}
}

// An unlink sets due again (an empty resolved_at) the upcoming job's rows for every
// series the matched book is in, its main one and the ones its list names, in
// its own library only; a series no matched book is in, or whose matched book's
// match named its identifiers' work, keeps its resolution.
func TestUnlinkSetsMatchedSeriesDue(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Shelf", Root: "/tmp/s"})
	lib2, _ := c.CreateLibrary(ctx, Library{Name: "Other", Root: "/tmp/o"})
	for _, b := range []*Book{
		{LibraryID: lib.ID, RelPath: "gg", Title: "Guards! Guards!", Series: "Discworld", SeriesIndex: 8},
		{LibraryID: lib.ID, RelPath: "dune", Title: "Dune", Series: "Dune", SeriesIndex: 1},
		{LibraryID: lib2.ID, RelPath: "gg", Title: "Guards! Guards!", Series: "Discworld", SeriesIndex: 8},
	} {
		b.Format = "m4b"
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EditBook(ctx, lib.ID, "gg", BookEdit{Set: map[string]string{FieldMoreSeries: `[{"name":"City Watch","position":1}]`}}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, "gg", BookEdit{Match: &MatchAnchor{WorkID: "w"}, Source: SourceCommunity}); err != nil {
		t.Fatal(err)
	}
	// Dune's match names the work its identifiers resolved to: it stood in
	// for nothing, so its going leaves Dune's resolution standing.
	if err := c.EditBook(ctx, lib.ID, "dune", BookEdit{Match: &MatchAnchor{WorkID: "d"}, Source: SourceCommunity}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveUpcomingOwned(ctx, []UpcomingOwned{{Ref: Ref{LibraryID: lib.ID, Path: "dune"}, IDKey: "a:B0DUNE", WorkID: "d", CheckedAt: "now"}}, nil); err != nil {
		t.Fatal(err)
	}
	const at = "2026-11-01T00:00:00Z"
	for _, s := range []struct {
		lib  int64
		name string
	}{{lib.ID, "Discworld"}, {lib.ID, "City Watch"}, {lib.ID, "Dune"}, {lib2.ID, "Discworld"}} {
		if err := c.SetUpcomingSeries(ctx, s.lib, seriesKey(s.name), s.name, 1, []string{"s"}, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"gg", "dune"} {
		if err := c.EditBook(ctx, lib.ID, p, BookEdit{Match: &MatchAnchor{}, Source: SourceCommunity}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := c.UpcomingSeriesAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[fmt.Sprintf("%d %s", r.LibraryID, r.Name)] = r.ResolvedAt
	}
	want := map[string]string{
		fmt.Sprintf("%d Discworld", lib.ID): "", fmt.Sprintf("%d City Watch", lib.ID): "",
		fmt.Sprintf("%d Dune", lib.ID): at, fmt.Sprintf("%d Discworld", lib2.ID): at,
	}
	if !maps.Equal(got, want) {
		t.Fatalf("resolved_at = %v, want %v", got, want)
	}
}

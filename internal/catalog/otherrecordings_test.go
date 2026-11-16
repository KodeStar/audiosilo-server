package catalog

import (
	"context"
	"reflect"
	"testing"
)

// recordingsFixture is two books libraries and a podcasts one holding
// recordings of the work "w-one" known in each way a book can be: by a recorded
// match (book_matches), by the work its ASIN resolved to (upcoming_owned), and
// both.
type recordingsFixture struct {
	c           *Catalog
	ctx         context.Context
	main, other *Library
	pods        *Library
}

func newRecordingsFixture(t *testing.T) *recordingsFixture {
	t.Helper()
	c, ctx := newTestCatalog(t)
	f := &recordingsFixture{c: c, ctx: ctx}
	var err error
	if f.main, err = c.CreateLibrary(ctx, Library{Name: "Main", Root: "/tmp/main"}); err != nil {
		t.Fatal(err)
	}
	if f.other, err = c.CreateLibrary(ctx, Library{Name: "Other", Root: "/tmp/other"}); err != nil {
		t.Fatal(err)
	}
	if f.pods, err = c.CreateLibrary(ctx, Library{Name: "Pods", Root: "/tmp/pods", Kind: KindPodcasts}); err != nil {
		t.Fatal(err)
	}
	for _, b := range []Book{
		// The book a page is for: matched in the console.
		{LibraryID: f.main.ID, RelPath: "Bray/One", Title: "One", Narrator: "R. C. Bray", ASIN: "B0BRAY"},
		// Another narrator, known by its ASIN's work.
		{LibraryID: f.main.ID, RelPath: "Kemper/One", Title: "One", Narrator: "Kemper", ASIN: "B0KEMPER"},
		// Another recording in the second library, matched in the console.
		{LibraryID: f.other.ID, RelPath: "Abridged/One", Title: "One (Abridged)", Narrator: "Abe"},
		// The same recording again (same ASIN as the page's book): a duplicate.
		{LibraryID: f.other.ID, RelPath: "Copy/One", Title: "One", Narrator: "R. C. Bray", ASIN: "B0BRAY"},
		// Its ASIN resolved to w-one, but the console matched it to w-two.
		{LibraryID: f.main.ID, RelPath: "Rematched/Two", Title: "Two", Narrator: "Kemper", ASIN: "B0WRONG"},
		// Private: outside a member's grant.
		{LibraryID: f.main.ID, RelPath: "Private/One", Title: "One", Narrator: "Zed", ASIN: "B0ZED"},
		// Unrelated.
		{LibraryID: f.main.ID, RelPath: "Else/Three", Title: "Three", ASIN: "B0THREE"},
		// A podcast episode that somehow carries the work: never a recording.
		{LibraryID: f.pods.ID, RelPath: "Show/Episode", Title: "Episode", Narrator: "Host"},
	} {
		b.AddedAt = "2024-01-01T00:00:00Z"
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []struct {
		lib  int64
		path string
		work string
	}{{f.main.ID, "Bray/One", "w-one"}, {f.other.ID, "Abridged/One", "w-one"}, {f.main.ID, "Rematched/Two", "w-two"},
		{f.pods.ID, "Show/Episode", "w-one"}} {
		if _, err := c.db.ExecContext(ctx, `INSERT INTO book_matches(library_id, path, work_id, how, set_at)
			VALUES(?, ?, ?, 'accepted', 'now')`, m.lib, m.path, m.work); err != nil {
			t.Fatal(err)
		}
	}
	owned := []UpcomingOwned{
		{Ref: Ref{LibraryID: f.main.ID, Path: "Bray/One"}, IDKey: "a:B0BRAY", WorkID: "w-one"},
		{Ref: Ref{LibraryID: f.main.ID, Path: "Kemper/One"}, IDKey: "a:B0KEMPER", WorkID: "w-one"},
		{Ref: Ref{LibraryID: f.other.ID, Path: "Copy/One"}, IDKey: "a:B0BRAY", WorkID: "w-one"},
		{Ref: Ref{LibraryID: f.main.ID, Path: "Rematched/Two"}, IDKey: "a:B0WRONG", WorkID: "w-one"},
		{Ref: Ref{LibraryID: f.main.ID, Path: "Private/One"}, IDKey: "a:B0ZED", WorkID: "w-one"},
		{Ref: Ref{LibraryID: f.main.ID, Path: "Else/Three"}, IDKey: "a:B0THREE", WorkID: ""}, // no community match
	}
	for i := range owned {
		owned[i].CheckedAt = "now"
	}
	if err := c.SaveUpcomingOwned(ctx, owned, nil); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *recordingsFixture) all() []Scope {
	return []Scope{{LibraryID: f.main.ID, AllowAll: true}, {LibraryID: f.other.ID, AllowAll: true}, {LibraryID: f.pods.ID, AllowAll: true}}
}

// TestBookWorkID: every read of a book carries its work key, a recorded match
// first, else the work its identifiers resolved to, else none.
func TestBookWorkID(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	want := map[string]string{
		"Bray/One": "w-one", "Kemper/One": "w-one", "Rematched/Two": "w-two", "Private/One": "w-one", "Else/Three": "",
	}
	page, err := f.c.ListBooks(f.ctx, ListOptions{LibraryID: f.main.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, b := range page.Books {
		got[b.RelPath] = b.WorkID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("list work ids = %v, want %v", got, want)
	}
	// The single-book read, an aliased read (the recent shelf) and a read off a
	// derived table (a series' next book) carry it too.
	b, err := f.c.GetBookByPath(f.ctx, f.other.ID, "Abridged/One")
	if err != nil || b.WorkID != "w-one" {
		t.Fatalf("GetBookByPath = %+v, %v", b, err)
	}
	recent, err := f.c.RecentBooks(f.ctx, f.all(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range recent {
		if b.RelPath == "Rematched/Two" && b.WorkID != "w-two" {
			t.Fatalf("recent Rematched/Two work = %q", b.WorkID)
		}
	}
	if _, err := f.c.db.ExecContext(f.ctx, `UPDATE books SET series = 'S', series_key = ?,
		series_index = CASE rel_path WHEN 'Bray/One' THEN 1 ELSE 2 END
		WHERE library_id = ? AND rel_path IN ('Bray/One', 'Kemper/One')`, seriesKey("S"), f.main.ID); err != nil {
		t.Fatal(err)
	}
	first, err := f.c.GetBookByPath(f.ctx, f.main.ID, "Bray/One")
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := f.c.NextInSeries(f.ctx, f.main.ID, first, Scope{LibraryID: f.main.ID, AllowAll: true})
	if err != nil || next == nil || next.RelPath != "Kemper/One" || next.WorkID != "w-one" {
		t.Fatalf("NextInSeries = %+v, %v", next, err)
	}
}

// TestOtherRecordings is the allowed+denied pair: an unrestricted caller gets
// every other recording of the work across books libraries (its own library
// first, then title, then narrator), never the book itself, a copy sharing its
// ASIN, a book whose recorded match names another work, or a podcast; a member
// whose grant misses one recording never gets it.
func TestOtherRecordings(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	me := Ref{LibraryID: f.main.ID, Path: "Bray/One"}

	got, err := f.c.OtherRecordings(f.ctx, f.all(), me)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Kemper/One", "Private/One", "Abridged/One"}; !reflect.DeepEqual(relPaths(got), want) {
		t.Fatalf("allowed = %q, want %q", relPaths(got), want)
	}
	if got[2].LibraryID != f.other.ID || got[2].WorkID != "w-one" || got[0].Narrator != "Kemper" || got[0].Files != nil {
		t.Fatalf("books = %+v", got)
	}

	// Denied: granted only Kemper/ and Bray/ in the main library, the private
	// recording and the other library's never appear.
	member := []Scope{{LibraryID: f.main.ID, Paths: []string{"Bray", "Kemper"}}}
	got, err = f.c.OtherRecordings(f.ctx, member, me)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Kemper/One"}; !reflect.DeepEqual(relPaths(got), want) {
		t.Fatalf("denied = %q, want %q", relPaths(got), want)
	}

	// Seen from the second library: its own library first.
	got, err = f.c.OtherRecordings(f.ctx, f.all(), Ref{LibraryID: f.other.ID, Path: "Abridged/One"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Copy/One", "Kemper/One", "Bray/One", "Private/One"}; !reflect.DeepEqual(relPaths(got), want) {
		t.Fatalf("from the other library = %q, want %q", relPaths(got), want)
	}

	// None: no work, an unknown book, no scopes, or a book alone with its work.
	for _, tc := range []struct {
		scopes []Scope
		ref    Ref
	}{
		{f.all(), Ref{LibraryID: f.main.ID, Path: "Else/Three"}},
		{f.all(), Ref{LibraryID: f.main.ID, Path: "Missing"}},
		{nil, me},
		{f.all(), Ref{LibraryID: f.main.ID, Path: "Rematched/Two"}},
	} {
		if got, err := f.c.OtherRecordings(f.ctx, tc.scopes, tc.ref); err != nil || len(got) != 0 {
			t.Fatalf("OtherRecordings(%+v) = %q, %v", tc.ref, relPaths(got), err)
		}
	}
}

// TestInProgress: only a book with a position and not finished is in progress,
// and only the user's own.
func TestInProgress(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	uid := seedUser(t, f.c, f.ctx)
	save := func(path string, pos float64, finished bool) {
		t.Helper()
		if _, err := f.c.SaveProgress(f.ctx, uid, Progress{Ref: Ref{LibraryID: f.main.ID, Path: path}, Position: pos,
			Duration: 100, Finished: finished, UpdatedAt: "2024-01-01T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	save("Kemper/One", 10, false)
	save("Private/One", 100, true)
	save("Else/Three", 0, false)
	refs := []Ref{{LibraryID: f.main.ID, Path: "Kemper/One"}, {LibraryID: f.main.ID, Path: "Private/One"},
		{LibraryID: f.main.ID, Path: "Else/Three"}, {LibraryID: f.main.ID, Path: "Kemper/One"}, {LibraryID: f.other.ID, Path: "Kemper/One"}}
	got, err := f.c.InProgress(f.ctx, uid, refs)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[Ref]bool{{LibraryID: f.main.ID, Path: "Kemper/One"}: true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("InProgress = %v, want %v", got, want)
	}
	if got, err := f.c.InProgress(f.ctx, uid+1, refs); err != nil || len(got) != 0 {
		t.Fatalf("another user's = %v, %v", got, err)
	}
}

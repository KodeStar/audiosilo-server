package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/meta"
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
		// The same recording again (the page's book's ASIN, tagged in lower
		// case): a duplicate.
		{LibraryID: f.other.ID, RelPath: "Copy/One", Title: "One", Narrator: "R. C. Bray", ASIN: "b0bray"},
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

// book reads the fixture's book at path in lib.
func (f *recordingsFixture) book(t *testing.T, lib int64, path string) *Book {
	t.Helper()
	b, err := f.c.GetBookByPath(f.ctx, lib, path)
	if err != nil {
		t.Fatal(err)
	}
	return b
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

// TestBookWorkIDFollowsIdentifiers: the work the upcoming job resolved from a
// book's identifiers is its work only while they are still the book's (the
// row's id_key): a retag, or another book at the path, holds none until the job
// resolves it again, and neither does an OtherRecordings candidate; a tag only
// spelled differently (case, spaces) keeps it, and a recorded match stands
// whatever the identifiers.
func TestBookWorkIDFollowsIdentifiers(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	retag := func(lib int64, path, asin string) {
		t.Helper()
		b := f.book(t, lib, path)
		b.ASIN = asin
		if _, err := f.c.UpsertBook(f.ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	work := func(lib int64, path string) string {
		t.Helper()
		return f.book(t, lib, path).WorkID
	}
	retag(f.main.ID, "Kemper/One", " b0kemper ")
	if got := work(f.main.ID, "Kemper/One"); got != "w-one" {
		t.Fatalf("respelled tag: work %q, want w-one", got)
	}
	retag(f.main.ID, "Kemper/One", "B0SOMEOTHER")
	retag(f.main.ID, "Bray/One", "B0SOMEOTHER")
	if got := work(f.main.ID, "Kemper/One"); got != "" {
		t.Fatalf("retagged: work %q, want none until resolved again", got)
	}
	if got := work(f.main.ID, "Bray/One"); got != "w-one" {
		t.Fatalf("retagged with a recorded match: work %q, want w-one", got)
	}
	got, err := f.c.OtherRecordings(f.ctx, f.all(), f.book(t, f.main.ID, "Bray/One"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Private/One", "Copy/One", "Abridged/One"}; !reflect.DeepEqual(relPaths(got), want) {
		t.Fatalf("other recordings after the retag = %q, want %q", relPaths(got), want)
	}
}

// TestIDKeyExpr: the SQL key a book's identifiers are matched to a stored
// resolution by is meta.IDKey's, the key the upcoming job stores it under.
func TestIDKeyExpr(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	for _, tc := range []struct{ asin, isbn string }{
		{"B00FLIJJSY", ""}, {" b00flijjsy\t", "9780553418026"}, {"", "978-0-553-41802-6"},
		{"  ", " 978 0553 41802x\n"}, {"", "0-553-41802-X"}, {" ", " - "},
		// White space beyond ASCII, which strings.TrimSpace trims too.
		{"B00FLIJJSY ", ""}, {"\u0085b00flijjsy　", ""}, {" ", " 978-0553418026 "},
	} {
		var key sql.NullString
		if err := c.db.QueryRowContext(ctx, `SELECT `+idKeyExpr("b")+` FROM (SELECT ? AS asin, ? AS isbn) b`,
			tc.asin, tc.isbn).Scan(&key); err != nil {
			t.Fatal(err)
		}
		// No identifiers: NULL, which equals no stored key, for meta.IDKey's "".
		if got, want := key.String, meta.IDKey(tc.asin, tc.isbn); got != want {
			t.Errorf("idKeyExpr(%q, %q) = %q, want %q", tc.asin, tc.isbn, got, want)
		}
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
	me := f.book(t, f.main.ID, "Bray/One")

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
	got, err = f.c.OtherRecordings(f.ctx, f.all(), f.book(t, f.other.ID, "Abridged/One"))
	if err != nil {
		t.Fatal(err)
	}
	// Its own library first: Copy/One, then the rest, but Bray/One, Copy/One's
	// ASIN again.
	if want := []string{"Copy/One", "Kemper/One", "Private/One"}; !reflect.DeepEqual(relPaths(got), want) {
		t.Fatalf("from the other library = %q, want %q", relPaths(got), want)
	}

	// None: no work, no scopes, or a book alone with its work.
	for _, tc := range []struct {
		scopes []Scope
		book   *Book
	}{
		{f.all(), f.book(t, f.main.ID, "Else/Three")},
		{nil, me},
		{f.all(), f.book(t, f.main.ID, "Rematched/Two")},
	} {
		if got, err := f.c.OtherRecordings(f.ctx, tc.scopes, tc.book); err != nil || len(got) != 0 {
			t.Fatalf("OtherRecordings(%s) = %q, %v", tc.book.RelPath, relPaths(got), err)
		}
	}
}

// TestOtherRecordingsOneEach (server#241): each other recording is listed
// once. Of copies of one (a shared ASIN, else ISBN, normalized) the first in order
// is kept, the book's own library first, and copies never crowd a recording out
// of the limit. The pair: a caller who can open only a copy in another library
// gets that copy, and one who can open only the original never gets the copy.
func TestOtherRecordingsOneEach(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	var owned []UpcomingOwned
	add := func(lib int64, path, asin string) {
		t.Helper()
		b := Book{LibraryID: lib, RelPath: path, Title: "One", Narrator: "Kemper", ASIN: asin, AddedAt: "2024-01-01T00:00:00Z"}
		if _, err := f.c.UpsertBook(f.ctx, &b); err != nil {
			t.Fatal(err)
		}
		owned = append(owned, UpcomingOwned{Ref: Ref{LibraryID: lib, Path: path}, IDKey: meta.IDKey(asin, ""),
			WorkID: "w-one", CheckedAt: "now"})
	}
	// Kemper/One again in the second library, and more copies of it in the
	// first than the limit holds.
	add(f.other.ID, "Kemper/One", " b0kemper ")
	for i := range MaxOtherRecordings + 5 {
		add(f.main.ID, fmt.Sprintf("Kemper/One copy %02d", i), "B0KEMPER")
	}
	if err := f.c.SaveUpcomingOwned(f.ctx, owned, nil); err != nil {
		t.Fatal(err)
	}
	me := f.book(t, f.main.ID, "Bray/One")
	got, err := f.c.OtherRecordings(f.ctx, f.all(), me)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Kemper/One", "Private/One", "Abridged/One"}; !reflect.DeepEqual(relPaths(got), want) {
		t.Fatalf("allowed = %q, want %q", relPaths(got), want)
	}
	if got[0].LibraryID != f.main.ID {
		t.Fatalf("kept %+v, want the book's own library's copy", got[0])
	}

	// Allowed: only the second library's copy is open to this member, so it is
	// the recording listed.
	member := []Scope{{LibraryID: f.main.ID, Paths: []string{"Bray"}}, {LibraryID: f.other.ID, Paths: []string{"Kemper"}}}
	if got, err = f.c.OtherRecordings(f.ctx, member, me); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].LibraryID != f.other.ID || got[0].RelPath != "Kemper/One" {
		t.Fatalf("copy only = %+v", got)
	}
	// Denied: a copy outside the grant never stands in for the one inside it.
	// Asked from the second library, its own copy comes first in order, so
	// dropping copies before the scope filter would leave this member nothing.
	member = []Scope{{LibraryID: f.main.ID, Paths: []string{"Kemper"}}, {LibraryID: f.other.ID, Paths: []string{"Abridged"}}}
	if got, err = f.c.OtherRecordings(f.ctx, member, f.book(t, f.other.ID, "Abridged/One")); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].LibraryID != f.main.ID || got[0].RelPath != "Kemper/One" {
		t.Fatalf("original only = %+v", got)
	}
}

// TestOtherRecordingsSharedISBN (server#241): two recordings with their own
// ASINs are both listed though they share an ISBN (the print edition's, as a
// match often fills it in), and a book with only that ISBN is a copy.
func TestOtherRecordingsSharedISBN(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	const isbn = "9780000000002"
	for path, spelled := range map[string]string{"Kemper/One": isbn, "Private/One": "978-0-00-000000-2"} {
		b := f.book(t, f.main.ID, path)
		b.ISBN = spelled
		if _, err := f.c.UpsertBook(f.ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// Spelled unlike either recording's, so only the normalized ISBN makes it a copy.
	printed := Book{LibraryID: f.other.ID, RelPath: "Print/One", Title: "One", Narrator: "Kemper", ISBN: "978 0000000002",
		AddedAt: "2024-01-01T00:00:00Z"}
	if _, err := f.c.UpsertBook(f.ctx, &printed); err != nil {
		t.Fatal(err)
	}
	if err := f.c.SaveUpcomingOwned(f.ctx, []UpcomingOwned{{Ref: Ref{LibraryID: f.other.ID, Path: "Print/One"},
		IDKey: meta.IDKey("", printed.ISBN), WorkID: "w-one", CheckedAt: "now"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.book(t, f.other.ID, "Print/One").WorkID; got != "w-one" {
		t.Fatalf("Print/One work = %q, want w-one", got)
	}
	got, err := f.c.OtherRecordings(f.ctx, f.all(), f.book(t, f.main.ID, "Bray/One"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Kemper/One", "Private/One", "Abridged/One"}; !reflect.DeepEqual(relPaths(got), want) {
		t.Fatalf("other recordings = %q, want %q", relPaths(got), want)
	}
}

// TestOtherRecordingsPlan: a work's recordings are found by the work id
// indexes on both sides whatever the caller reaches, one library (whose
// library_id SQLite pushes down into the candidate reads) or several, never by
// a read of every recorded match or resolved book of a library, and each book
// by its key.
func TestOtherRecordingsPlan(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	me, err := f.c.GetBookByPath(f.ctx, f.main.ID, "Bray/One")
	if err != nil {
		t.Fatal(err)
	}
	for name, scopes := range map[string][]Scope{
		"one library":          {{LibraryID: f.main.ID, AllowAll: true}},
		"one library, a share": {{LibraryID: f.main.ID, Paths: []string{"Bray", "Kemper"}}},
		"every library":        f.all(),
	} {
		q, args := otherRecordingsQuery(scopes, me)
		joined := strings.Join(queryPlan(t, f.c, q, args), "\n")
		for _, want := range []string{
			"SEARCH book_matches USING INDEX idx_book_matches_work (work_id=?)",
			"SEARCH upcoming_owned USING COVERING INDEX idx_upcoming_owned_work (work_id=?",
			"SEARCH b USING INDEX sqlite_autoindex_books_1 (library_id=? AND rel_path=?)",
			// Each candidate's own work key (workIDExpr, its id_key check too) by
			// the book's key on both sides.
			"SEARCH wm USING INDEX sqlite_autoindex_book_matches_1 (library_id=? AND path=?)",
			"SEARCH wo USING PRIMARY KEY (library_id=? AND path=?)",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: plan does not use %s:\n%s", name, want, joined)
			}
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

// TestUpcomingOwnedWorksIDKey (server#412): a book holds the work the upcoming
// job resolved only while it still carries the identifiers that work was
// resolved by (id_key, compared normalized), as workIDExpr reads it.
func TestUpcomingOwnedWorksIDKey(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for path, asin := range map[string]string{"Retagged/One": "B0NEW", "Kept/Two": " b0two "} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: path, Title: path, ASIN: asin, AddedAt: "2020-01-01"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SaveUpcomingOwned(ctx, []UpcomingOwned{
		{Ref: Ref{LibraryID: lib.ID, Path: "Retagged/One"}, IDKey: "a:B0OLD", WorkID: "w-old", CheckedAt: "now"},
		{Ref: Ref{LibraryID: lib.ID, Path: "Kept/Two"}, IDKey: "a:B0TWO", WorkID: "w-two", CheckedAt: "now"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"w-two": true}
	for name, scopes := range map[string][]Scope{"all": nil, "scoped": {{LibraryID: lib.ID, AllowAll: true}}} {
		got, err := c.UpcomingOwnedWorks(ctx, scopes, scopes == nil, []string{"w-old", "w-two"})
		if err != nil || !maps.Equal(got, want) {
			t.Fatalf("%s: owned = %v, %v, want %v (a retagged book holds no stale work)", name, got, err, want)
		}
	}
}

// TestUpcomingOwnedWorksMatch (server#375): a book is the work its work key
// names, so one matched in the console with no ASIN or ISBN holds its match's
// work, and one whose match names another work than its identifiers resolved
// to holds the match's alone. The pair: a caller whose grant misses the
// matched book holds none of it.
func TestUpcomingOwnedWorksMatch(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	// Abridged/One (other library) has no identifier and a recorded match to
	// w-one; Rematched/Two's ASIN resolved to w-one but its match names w-two.
	ids := []string{"w-one", "w-two"}
	got, err := f.c.UpcomingOwnedWorks(f.ctx, nil, true, ids)
	if err != nil || !maps.Equal(got, map[string]bool{"w-one": true, "w-two": true}) {
		t.Fatalf("all: owned = %v, %v", got, err)
	}
	// Allowed: the other library alone holds w-one through the match-only book.
	other := []Scope{{LibraryID: f.other.ID, Paths: []string{"Abridged"}}}
	if got, err = f.c.UpcomingOwnedWorks(f.ctx, other, false, ids); err != nil || !maps.Equal(got, map[string]bool{"w-one": true}) {
		t.Fatalf("match-only book: owned = %v, %v, want w-one", got, err)
	}
	// Rematched/Two alone holds its match's work, not its ASIN's.
	rematched := []Scope{{LibraryID: f.main.ID, Paths: []string{"Rematched"}}}
	if got, err = f.c.UpcomingOwnedWorks(f.ctx, rematched, false, ids); err != nil || !maps.Equal(got, map[string]bool{"w-two": true}) {
		t.Fatalf("rematched book: owned = %v, %v, want w-two", got, err)
	}
	// Denied: a grant missing the match-only book holds nothing of it.
	if got, err = f.c.UpcomingOwnedWorks(f.ctx, []Scope{{LibraryID: f.other.ID, Paths: []string{"Copy"}}}, false, []string{"w-two"}); err != nil || len(got) != 0 {
		t.Fatalf("outside the grant: owned = %v, %v", got, err)
	}
}

// TestOtherRecordingsSkipsOwnCopies (server#413): the book's own copies that
// teach the copy rule nothing are left out by the query itself, so hundreds of
// them are never read; a copy carrying another ISBN is still read, and an
// ISBN-only book sharing that ISBN stays a copy, as match.Recordings has it.
func TestOtherRecordingsSkipsOwnCopies(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	var owned []UpcomingOwned
	add := func(path, asin, isbn string) {
		t.Helper()
		b := Book{LibraryID: f.other.ID, RelPath: path, Title: "One", Narrator: "R. C. Bray", ASIN: asin, ISBN: isbn,
			AddedAt: "2024-01-01T00:00:00Z"}
		if _, err := f.c.UpsertBook(f.ctx, &b); err != nil {
			t.Fatal(err)
		}
		owned = append(owned, UpcomingOwned{Ref: Ref{LibraryID: f.other.ID, Path: path}, IDKey: meta.IDKey(asin, isbn),
			WorkID: "w-one", CheckedAt: "now"})
	}
	// Bray/One is B0BRAY with no ISBN: Copy/One (b0bray) is its copy already.
	add("A copy with an ISBN/One", " B0BRAY", "978-0-00-000009-9") // its ISBN marks the next one
	add("Z print/One", "", "9780000000099")                        // a copy through that ISBN
	if err := f.c.SaveUpcomingOwned(f.ctx, owned, nil); err != nil {
		t.Fatal(err)
	}
	me := f.book(t, f.main.ID, "Bray/One")
	q, args := otherRecordingsQuery(f.all(), me)
	rows, err := queryRows(f.ctx, f.c.db, scanBookRow, q, args...)
	if err != nil {
		t.Fatal(err)
	}
	read := relPaths(rows)
	if slices.Contains(read, "Copy/One") || !slices.Contains(read, "A copy with an ISBN/One") {
		t.Fatalf("query read %q: want no Copy/One, the copy with an ISBN kept", read)
	}
	got, err := f.c.OtherRecordings(f.ctx, f.all(), me)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Kemper/One", "Private/One", "Abridged/One"}; !reflect.DeepEqual(relPaths(got), want) {
		t.Fatalf("other recordings = %q, want %q", relPaths(got), want)
	}
}

// TestWorkBooks (server#412): every book of the works asked whatever its
// series, by its work key (a recorded match naming another work is not one),
// books libraries only, in library order then path; the pair: a member's grant
// keeps the rest out.
func TestWorkBooks(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	got, err := f.c.WorkBooks(f.ctx, f.all(), []string{"w-one"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Bray/One", "Kemper/One", "Private/One", "Abridged/One", "Copy/One"}; !reflect.DeepEqual(relPaths(got), want) {
		t.Fatalf("allowed = %q, want %q", relPaths(got), want)
	}
	member := []Scope{{LibraryID: f.main.ID, Paths: []string{"Kemper", "Rematched"}}}
	if got, err = f.c.WorkBooks(f.ctx, member, []string{"w-one", "w-two"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Kemper/One", "Rematched/Two"}; !reflect.DeepEqual(relPaths(got), want) {
		t.Fatalf("denied = %q, want %q", relPaths(got), want)
	}
	if got, err = f.c.WorkBooks(f.ctx, nil, []string{"w-one"}); err != nil || len(got) != 0 {
		t.Fatalf("no scopes = %q, %v", relPaths(got), err)
	}
}

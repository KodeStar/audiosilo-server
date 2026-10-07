package catalog

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// annotationFixture is the user-state fixture with two indexed books.
type annotationFixture struct {
	*userStateFixture
	all []Scope // the whole library
}

const (
	annBookA = "A/Book"  // indexed
	annBookB = "B/Book"  // indexed
	annGhost = "C/Ghost" // not indexed
)

func newAnnotationFixture(t *testing.T) *annotationFixture {
	t.Helper()
	f := &annotationFixture{userStateFixture: newUserStateFixture(t, time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))}
	f.all = []Scope{{LibraryID: f.lib, AllowAll: true}}
	for _, p := range []string{annBookA, annBookB} {
		if _, err := f.c.UpsertBook(t.Context(), &Book{LibraryID: f.lib, RelPath: p, IsFolder: true, Title: p}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *annotationFixture) bookmark(t *testing.T, user int64, p string, pos float64, note, label string) *Bookmark {
	t.Helper()
	b, err := f.c.AddBookmark(t.Context(), user, Bookmark{Ref: f.ref(p), Position: pos, Note: note, Label: label})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *annotationFixture) note(t *testing.T, user int64, p string, pos float64, body string) *Note {
	t.Helper()
	n, err := f.c.AddNote(t.Context(), user, Note{Ref: f.ref(p), Position: pos, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A label is "" or a lowercase machine key; a bookmark note is at most
// MaxBookmarkNote characters (runes, not bytes), a note body MaxNoteBody.
func TestCheckBookmarkAndNoteBounds(t *testing.T) {
	for _, l := range []string{"", "quote", "fell_asleep", "a", "q2", "a" + strings.Repeat("b", 31)} {
		if err := checkBookmark(nil, nil, &l); err != nil {
			t.Errorf("label %q refused: %v", l, err)
		}
	}
	for _, l := range []string{"Quote", "1quote", "_quote", "fell-asleep", " quote", "quote ", "quoté",
		"a" + strings.Repeat("b", 32), "fell asleep"} {
		if err := checkBookmark(nil, nil, &l); !errors.Is(err, ErrInvalidLabel) {
			t.Errorf("label %q = %v, want ErrInvalidLabel", l, err)
		}
	}
	if err := checkBookmark(nil, new(strings.Repeat("é", MaxBookmarkNote)), nil); err != nil {
		t.Errorf("a note of %d two-byte characters refused: %v", MaxBookmarkNote, err)
	}
	if err := checkBookmark(nil, new(strings.Repeat("a", MaxBookmarkNote+1)), nil); !errors.Is(err, ErrBookmarkNoteTooLong) {
		t.Errorf("an over-long note = %v, want ErrBookmarkNoteTooLong", err)
	}
	if err := checkNote(new(strings.Repeat("é", MaxNoteBody)), new(0.0)); err != nil {
		t.Errorf("a body of %d characters refused: %v", MaxNoteBody, err)
	}
	if err := checkNote(new(strings.Repeat("a", MaxNoteBody+1)), nil); !errors.Is(err, ErrNoteBodyTooLong) {
		t.Errorf("an over-long body = %v, want ErrNoteBodyTooLong", err)
	}

	// The adds check too, and store nothing they refuse.
	f := newAnnotationFixture(t)
	ctx := t.Context()
	if _, err := f.c.AddBookmark(ctx, f.ann, Bookmark{Ref: f.ref(annBookA), Label: "Nope"}); !errors.Is(err, ErrInvalidLabel) {
		t.Fatalf("AddBookmark with a bad label = %v", err)
	}
	if _, err := f.c.AddNote(ctx, f.ann, Note{Ref: f.ref(annBookA), Body: strings.Repeat("a", MaxNoteBody+1)}); !errors.Is(err, ErrNoteBodyTooLong) {
		t.Fatalf("AddNote with an over-long body = %v", err)
	}
	for _, p := range []float64{-1, math.NaN(), math.Inf(1)} {
		if err := checkNote(nil, &p); !errors.Is(err, ErrInvalidPosition) {
			t.Errorf("position %v = %v, want ErrInvalidPosition", p, err)
		}
		if _, err := f.c.AddNote(ctx, f.ann, Note{Ref: f.ref(annBookA), Position: p}); !errors.Is(err, ErrInvalidPosition) {
			t.Errorf("AddNote at %v = %v, want ErrInvalidPosition", p, err)
		}
	}
	if ns, _ := f.c.ListNotes(ctx, f.ann, f.ref(annBookA)); len(ns) != 0 {
		t.Fatalf("a refused note was stored: %+v", ns)
	}
	if bms, _ := f.c.ListBookmarks(ctx, f.ann, f.ref(annBookA)); len(bms) != 0 {
		t.Fatalf("a refused bookmark was stored: %+v", bms)
	}
	b := f.bookmark(t, f.ann, annBookA, 5, "n", "quote")
	if bms, _ := f.c.ListBookmarks(ctx, f.ann, f.ref(annBookA)); len(bms) != 1 || bms[0] != *b || b.Label != "quote" {
		t.Fatalf("bookmarks = %+v, want the added %+v", bms, b)
	}
}

// The owner's edits: partial, checked, the whole row back; another user's id,
// an unknown one and one outside the caller's current access are ErrNotFound and
// change nothing. The catalog is file-backed, so a write sent through the
// read-only reader pool would fail here.
func TestEditBookmark(t *testing.T) {
	f := newAnnotationFixture(t)
	ctx := t.Context()
	b := f.bookmark(t, f.ann, annBookA, 12, "first", "quote")

	label := "relisten"
	got, err := f.c.EditBookmark(ctx, f.ann, b.ID, BookmarkEdit{Label: &label}, f.all)
	if err != nil {
		t.Fatal(err)
	}
	want := *b
	want.Label = "relisten"
	if *got != want {
		t.Fatalf("label edit = %+v, want %+v", got, want)
	}
	note, none := "second", ""
	if got, err = f.c.EditBookmark(ctx, f.ann, b.ID, BookmarkEdit{Note: &note}, f.all); err != nil || got.Note != "second" || got.Label != "relisten" {
		t.Fatalf("note edit = %+v %v", got, err)
	}
	if got, err = f.c.EditBookmark(ctx, f.ann, b.ID, BookmarkEdit{Label: &none}, f.all); err != nil || got.Label != "" || got.Note != "second" {
		t.Fatalf("clearing the label = %+v %v", got, err)
	}
	stored := func() Bookmark {
		t.Helper()
		bms, err := f.c.ListBookmarks(ctx, f.ann, f.ref(annBookA))
		if err != nil || len(bms) != 1 {
			t.Fatalf("ann's bookmarks = %+v %v", bms, err)
		}
		return bms[0]
	}
	if s := stored(); s != *got {
		t.Fatalf("stored %+v, answered %+v", s, got)
	}

	bad, long := "Bad", strings.Repeat("a", MaxBookmarkNote+1)
	other := []Scope{{LibraryID: f.lib, Paths: []string{"B"}}}
	for _, c := range []struct {
		name   string
		user   int64
		id     int64
		edit   BookmarkEdit
		scopes []Scope
		want   error
	}{
		{"another user's id", f.bob, b.ID, BookmarkEdit{Note: &note}, f.all, ErrNotFound},
		{"an unknown id", f.ann, b.ID + 100, BookmarkEdit{Note: &note}, f.all, ErrNotFound},
		{"outside current access", f.ann, b.ID, BookmarkEdit{Note: &note}, other, ErrNotFound},
		{"no access at all", f.ann, b.ID, BookmarkEdit{Note: &note}, nil, ErrNotFound},
		{"an unknown id before an empty edit", f.ann, b.ID + 100, BookmarkEdit{}, f.all, ErrNotFound},
		{"nothing to change", f.ann, b.ID, BookmarkEdit{}, f.all, ErrNothingToChange},
		{"a bad label", f.ann, b.ID, BookmarkEdit{Label: &bad}, f.all, ErrInvalidLabel},
		{"a long note", f.ann, b.ID, BookmarkEdit{Note: &long, Label: &label}, f.all, ErrBookmarkNoteTooLong},
	} {
		before := stored()
		if _, err := f.c.EditBookmark(ctx, c.user, c.id, c.edit, c.scopes); !errors.Is(err, c.want) {
			t.Errorf("%s = %v, want %v", c.name, err, c.want)
		}
		if after := stored(); after != before {
			t.Errorf("%s changed the bookmark: %+v -> %+v", c.name, before, after)
		}
	}
}

func TestEditNote(t *testing.T) {
	f := newAnnotationFixture(t)
	ctx := t.Context()
	n := f.note(t, f.ann, annBookA, 0, "a thought")

	f.clock = f.clock.Add(time.Minute)
	pos := 93.5
	got, err := f.c.EditNote(ctx, f.ann, n.ID, NoteEdit{Position: &pos}, f.all)
	if err != nil {
		t.Fatal(err)
	}
	if got.Position != 93.5 || got.Body != "a thought" || got.CreatedAt != n.CreatedAt || got.UpdatedAt == n.UpdatedAt {
		t.Fatalf("position edit = %+v (added %+v)", got, n)
	}
	body := "a better thought"
	if got, err = f.c.EditNote(ctx, f.ann, n.ID, NoteEdit{Body: &body}, f.all); err != nil || got.Body != body || got.Position != 93.5 {
		t.Fatalf("body edit = %+v %v", got, err)
	}
	stored := func() Note {
		t.Helper()
		ns, err := f.c.ListNotes(ctx, f.ann, f.ref(annBookA))
		if err != nil || len(ns) != 1 {
			t.Fatalf("ann's notes = %+v %v", ns, err)
		}
		return ns[0]
	}
	if s := stored(); s != *got {
		t.Fatalf("stored %+v, answered %+v", s, got)
	}

	neg, nan, inf, long := -1.0, math.NaN(), math.Inf(1), strings.Repeat("a", MaxNoteBody+1)
	other := []Scope{{LibraryID: f.lib, Paths: []string{"B"}}}
	for _, c := range []struct {
		name   string
		user   int64
		id     int64
		edit   NoteEdit
		scopes []Scope
		want   error
	}{
		{"another user's id", f.bob, n.ID, NoteEdit{Body: &body}, f.all, ErrNotFound},
		{"an unknown id", f.ann, n.ID + 100, NoteEdit{Body: &body}, f.all, ErrNotFound},
		{"outside current access", f.ann, n.ID, NoteEdit{Body: &body}, other, ErrNotFound},
		{"nothing to change", f.ann, n.ID, NoteEdit{}, f.all, ErrNothingToChange},
		{"a negative position", f.ann, n.ID, NoteEdit{Position: &neg}, f.all, ErrInvalidPosition},
		{"a NaN position", f.ann, n.ID, NoteEdit{Position: &nan}, f.all, ErrInvalidPosition},
		{"an infinite position", f.ann, n.ID, NoteEdit{Position: &inf}, f.all, ErrInvalidPosition},
		{"a long body", f.ann, n.ID, NoteEdit{Body: &long}, f.all, ErrNoteBodyTooLong},
	} {
		before := stored()
		if _, err := f.c.EditNote(ctx, c.user, c.id, c.edit, c.scopes); !errors.Is(err, c.want) {
			t.Errorf("%s = %v, want %v", c.name, err, c.want)
		}
		if after := stored(); after != before {
			t.Errorf("%s changed the note: %+v -> %+v", c.name, before, after)
		}
	}
}

// annRow is a row of one of the all-books lists, as the tests compare them.
type annRow struct {
	id      int64
	path    string
	hasBook bool // its book is attached, and is the one at its path
}

// rowsOf turns a page's items into annRows.
func rowsOf[T any](items []T, row func(*T) (int64, Ref, *Book)) []annRow {
	out := make([]annRow, len(items))
	for i := range items {
		id, ref, b := row(&items[i])
		out[i] = annRow{id: id, path: ref.Path, hasBook: b != nil && b.RelPath == ref.Path}
	}
	return out
}

// annPager reads a page of one list for a user within scopes.
type annPager func(f *annotationFixture, user int64, scopes []Scope, o PageOptions) ([]annRow, string, error)

// annLists are the three all-books lists, each read through one adapter.
var annLists = []struct {
	name string
	page annPager
}{
	{"bookmarks", func(f *annotationFixture, user int64, scopes []Scope, o PageOptions) ([]annRow, string, error) {
		p, err := f.c.ListMyBookmarks(context.Background(), user, scopes, o)
		if err != nil {
			return nil, "", err
		}
		return rowsOf(p.Bookmarks, func(b *MyBookmark) (int64, Ref, *Book) { return b.ID, b.Ref, b.Book }), p.NextCursor, nil
	}},
	{"notes", func(f *annotationFixture, user int64, scopes []Scope, o PageOptions) ([]annRow, string, error) {
		p, err := f.c.ListMyNotes(context.Background(), user, scopes, o)
		if err != nil {
			return nil, "", err
		}
		return rowsOf(p.Notes, func(n *MyNote) (int64, Ref, *Book) { return n.ID, n.Ref, n.Book }), p.NextCursor, nil
	}},
	{"history", func(f *annotationFixture, user int64, scopes []Scope, o PageOptions) ([]annRow, string, error) {
		p, err := f.c.ListAllHistory(context.Background(), user, scopes, o)
		if err != nil {
			return nil, "", err
		}
		return rowsOf(p.History, func(h *HistoryEntry) (int64, Ref, *Book) { return h.ID, h.Ref, h.Book }), p.NextCursor, nil
	}},
}

// pageAll walks a list a page at a time (limit rows each), returning every row
// in order and the number of pages.
func pageAll(t *testing.T, limit int, page func(PageOptions) ([]annRow, string, error)) ([]annRow, int) {
	t.Helper()
	var all []annRow
	cursor, pages := "", 0
	for {
		got, next, err := page(PageOptions{Limit: limit, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(got) > limit || (next != "" && len(got) != limit) {
			t.Fatalf("page %d: %d rows for limit %d (next %q)", pages, len(got), limit, next)
		}
		all = append(all, got...)
		if next == "" {
			return all, pages
		}
		if pages > 100 {
			t.Fatal("paging does not end")
		}
		cursor = next
	}
}

func annIDs(rows []annRow) []int64 {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.id
	}
	return ids
}

// The all-books lists, newest first, a page at a time: ties in the timestamp are
// broken by id so no row is lost or repeated across pages; rows outside the
// caller's current access are left out in the query (pages stay full) but kept;
// another user's rows never appear; each row carries its book when one is indexed.
func TestAnnotationListsPage(t *testing.T) {
	f := newAnnotationFixture(t)
	ctx := t.Context()
	paths := []string{annBookA, annBookB, annGhost}
	// 11 rows of each list over 4 timestamps (ties), on 3 paths; bob's rows
	// interleaved. Time and id both rise row by row, so newest first is the ids
	// falling, ties included.
	for i := range 11 {
		f.clock = time.Date(2026, 10, 7, 9, i/3, 0, 0, time.UTC)
		p := paths[i%3]
		ended := f.clock.Add(time.Hour).Format(time.RFC3339)
		for _, user := range []int64{f.ann, f.bob} {
			f.bookmark(t, user, p, float64(i), "", "")
			f.note(t, user, p, float64(i), fmt.Sprint(i))
			if err := f.c.AddHistory(ctx, user, f.ref(p), 0, 1, ended, ended); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, l := range annLists {
		t.Run(l.name, func(t *testing.T) {
			pager := func(user int64, scopes []Scope) func(PageOptions) ([]annRow, string, error) {
				return func(o PageOptions) ([]annRow, string, error) { return l.page(f, user, scopes, o) }
			}
			full, _ := pageAll(t, 500, pager(f.ann, f.all))
			ids := annIDs(full)
			if len(ids) != 11 || !slices.IsSortedFunc(ids, func(a, b int64) int { return int(b - a) }) {
				t.Fatalf("ann's rows = %v, want 11 newest (highest id) first", ids)
			}
			for _, limit := range []int{1, 2, 3, 4, 10, 11, 12} {
				got, pages := pageAll(t, limit, pager(f.ann, f.all))
				if !slices.Equal(annIDs(got), ids) {
					t.Fatalf("limit %d: %v, want %v", limit, annIDs(got), ids)
				}
				if want := (len(ids) + limit - 1) / limit; pages != want {
					t.Fatalf("limit %d: %d pages, want %d", limit, pages, want)
				}
			}
			for _, r := range full {
				if r.hasBook != (r.path != annGhost) {
					t.Fatalf("row on %s: has a book %v", r.path, r.hasBook)
				}
			}

			// Access "A" only: the rows on B and C are left out, before paging.
			var onA []int64
			for _, r := range full {
				if r.path == annBookA {
					onA = append(onA, r.id)
				}
			}
			scoped, _ := pageAll(t, 2, pager(f.ann, []Scope{{LibraryID: f.lib, Paths: []string{"A"}}}))
			if len(onA) != 4 || !slices.Equal(annIDs(scoped), onA) {
				t.Fatalf("scoped to A: %v, want %v", annIDs(scoped), onA)
			}
			if got, _ := pageAll(t, 5, pager(f.ann, nil)); len(got) != 0 {
				t.Fatalf("no access: %v", got)
			}
			// Kept: the whole library's access shows them again.
			if got, _ := pageAll(t, 4, pager(f.ann, f.all)); !slices.Equal(annIDs(got), ids) {
				t.Fatalf("after re-granting: %v", annIDs(got))
			}

			// Bob's own rows, never ann's.
			bobRows, _ := pageAll(t, 3, pager(f.bob, f.all))
			bobs := annIDs(bobRows)
			if len(bobs) != 11 || slices.ContainsFunc(bobs, func(id int64) bool { return slices.Contains(ids, id) }) {
				t.Fatalf("bob's rows = %v, ann's %v", bobs, ids)
			}
		})
	}
}

// A cursor that doesn't decode is ErrInvalidCursor on every list; a sort value
// holding a NUL still round-trips (decodeCursor is encodeCursor's exact
// inverse); an empty list is [], never null.
func TestAnnotationListsCursor(t *testing.T) {
	f := newAnnotationFixture(t)
	for _, l := range annLists {
		for _, c := range []string{"!!!", base64.RawURLEncoding.EncodeToString([]byte("no separator")),
			base64.RawURLEncoding.EncodeToString([]byte("2026\x00twelve")), "a b"} {
			if _, _, err := l.page(f, f.ann, f.all, PageOptions{Cursor: c}); !errors.Is(err, ErrInvalidCursor) {
				t.Errorf("%s cursor %q = %v, want ErrInvalidCursor", l.name, c, err)
			}
		}
		rows, next, err := l.page(f, f.bob, f.all, PageOptions{})
		if err != nil || rows == nil || len(rows) != 0 || next != "" {
			t.Errorf("bob's empty %s = %v %q %v", l.name, rows, next, err)
		}
	}
	if v, id, err := decodeCursor(encodeCursor("odd\x00value", 42)); err != nil || v != "odd\x00value" || id != 42 {
		t.Fatalf("a NUL in the value: %q %d %v", v, id, err)
	}
}

// A label rides along when its book moves (the row moves, carryListeningState),
// and a user's bookmarks and notes go with them (FK cascade).
func TestAnnotationsMoveAndPurge(t *testing.T) {
	f := newAnnotationFixture(t)
	ctx := t.Context()
	b := f.bookmark(t, f.ann, "old/Book", 7, "kept", "fell_asleep")
	f.note(t, f.ann, "old/Book", 7, "kept too")
	f.bookmark(t, f.bob, "old/Book", 1, "", "quote")
	if err := f.c.MoveDurableState(ctx, f.lib, "old/Book", "new/Book"); err != nil {
		t.Fatal(err)
	}
	bms, err := f.c.ListBookmarks(ctx, f.ann, f.ref("new/Book"))
	want := *b
	want.Path = "new/Book"
	if err != nil || len(bms) != 1 || bms[0] != want {
		t.Fatalf("moved bookmarks = %+v %v, want %+v", bms, err, want)
	}

	if _, err := f.c.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, f.ann); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.c.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM bookmarks WHERE user_id = ?1) + (SELECT COUNT(*) FROM notes WHERE user_id = ?1)`,
		f.ann).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after deleting ann: %d of her rows, %v", n, err)
	}
	if bms, _ := f.c.ListBookmarks(ctx, f.bob, f.ref("new/Book")); len(bms) != 1 || bms[0].Label != "quote" {
		t.Fatalf("deleting ann touched bob's bookmark: %+v", bms)
	}
}

// The lists order by their timestamp as text, so it is stored fixed width (UTC
// milliseconds): a whole second sorts before its own fractions, which
// RFC3339Nano's trimmed form ("...:00Z" after "...:00.5Z") would get wrong. Here
// the ids run against time, so only the timestamp can order the rows right.
func TestAnnotationTimestampsSortAsText(t *testing.T) {
	f := newAnnotationFixture(t)
	base := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	want := map[string][]int64{}
	for _, frac := range []time.Duration{500 * time.Millisecond, 250 * time.Millisecond, 0} {
		f.clock = base.Add(frac)
		want["bookmarks"] = append(want["bookmarks"], f.bookmark(t, f.ann, annBookA, 0, "", "").ID)
		want["notes"] = append(want["notes"], f.note(t, f.ann, annBookA, 0, "").ID)
	}
	for _, l := range annLists[:2] { // history: TestAddHistoryNormalisesTimes
		rows, _, err := l.page(f, f.ann, f.all, PageOptions{})
		if err != nil || !slices.Equal(annIDs(rows), want[l.name]) {
			t.Fatalf("%s = %v %v, want %v", l.name, annIDs(rows), err, want[l.name])
		}
	}
	bms, err := f.c.ListBookmarks(t.Context(), f.ann, f.ref(annBookA))
	if err != nil || !slices.ContainsFunc(bms, func(b Bookmark) bool { return b.CreatedAt == "2026-10-07T09:00:00.000Z" }) {
		t.Fatalf("bookmarks = %+v %v, want created_at in the fixed-width form", bms, err)
	}
	f.clock = base.Add(time.Minute)
	if n, err := f.c.EditNote(t.Context(), f.ann, want["notes"][0], NoteEdit{Position: new(1.0)}, f.all); err != nil ||
		n.UpdatedAt != "2026-10-07T09:01:00.000Z" {
		t.Fatalf("edited note = %+v %v, want updated_at in the fixed-width form", n, err)
	}
}

// A listening span's client times are stored fixed width in UTC (any fraction,
// any offset), so the history orders by them as text; an empty or unparsable one
// is the server's time, never a refusal.
func TestAddHistoryNormalisesTimes(t *testing.T) {
	f := newAnnotationFixture(t)
	ctx := t.Context()
	f.clock = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct{ in, want string }{
		{"2026-10-07T10:00:00.5Z", "2026-10-07T10:00:00.500Z"},
		{"2026-10-07T10:00:00Z", "2026-10-07T10:00:00.000Z"},
		{"2026-10-07T12:00:00.25+02:00", "2026-10-07T10:00:00.250Z"},
		{"yesterday-ish", "2026-10-07T12:00:00.000Z"},
		{"", "2026-10-07T12:00:00.000Z"},
	} {
		if err := f.c.AddHistory(ctx, f.bob, f.ref(annBookA), 0, 1, c.in, c.in); err != nil {
			t.Fatalf("AddHistory(%q) = %v", c.in, err)
		}
		got, err := f.c.ListHistory(ctx, f.bob, f.ref(annBookA), 500)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, h := range got {
			found = found || (h.StartedAt == c.want && h.EndedAt == c.want)
		}
		if !found {
			t.Fatalf("AddHistory(%q): history %+v, want a span at %q", c.in, got, c.want)
		}
	}
	p, err := f.c.ListAllHistory(ctx, f.bob, f.all, PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ended []string
	for _, h := range p.History {
		ended = append(ended, h.EndedAt)
	}
	want := []string{"2026-10-07T12:00:00.000Z", "2026-10-07T12:00:00.000Z", "2026-10-07T10:00:00.500Z",
		"2026-10-07T10:00:00.250Z", "2026-10-07T10:00:00.000Z"}
	if !slices.Equal(ended, want) {
		t.Fatalf("history newest first = %v, want %v", ended, want)
	}
}

// An edit checks only the fields it sets: a bookmark stored before the note bound
// (a legacy note over MaxBookmarkNote) can still take a label, and a note's body
// edit doesn't re-check its stored position.
func TestEditChecksOnlyTheFieldsItSets(t *testing.T) {
	f := newAnnotationFixture(t)
	ctx := t.Context()
	long := strings.Repeat("a", MaxBookmarkNote+1)
	res, err := f.c.db.ExecContext(ctx,
		`INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, created_at) VALUES(?,?,?,?,?,?)`,
		f.ann, f.lib, annBookA, 5, long, "2026-01-01T00:00:00.000Z")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	label := "quote"
	got, err := f.c.EditBookmark(ctx, f.ann, id, BookmarkEdit{Label: &label}, f.all)
	if err != nil || got.Label != "quote" || got.Note != long {
		t.Fatalf("labelling a legacy long-note bookmark = %+v %v", got, err)
	}
	if _, err := f.c.EditBookmark(ctx, f.ann, id, BookmarkEdit{Note: &long}, f.all); !errors.Is(err, ErrBookmarkNoteTooLong) {
		t.Fatalf("setting a long note = %v, want ErrBookmarkNoteTooLong", err)
	}

	res, err = f.c.db.ExecContext(ctx,
		`INSERT INTO notes(user_id, library_id, rel_path, position, body, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
		f.ann, f.lib, annBookA, -3, strings.Repeat("b", MaxNoteBody+1), "2026-01-01T00:00:00.000Z", "2026-01-01T00:00:00.000Z")
	if err != nil {
		t.Fatal(err)
	}
	nid, _ := res.LastInsertId()
	body := "short now"
	if n, err := f.c.EditNote(ctx, f.ann, nid, NoteEdit{Body: &body}, f.all); err != nil || n.Body != body || n.Position != -3 {
		t.Fatalf("a body edit on a legacy note = %+v %v", n, err)
	}
}

// A bookmark's position follows a note's rule: finite and >= 0.
func TestBookmarkPositionRule(t *testing.T) {
	f := newAnnotationFixture(t)
	for _, p := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := f.c.AddBookmark(t.Context(), f.ann, Bookmark{Ref: f.ref(annBookA), Position: p}); !errors.Is(err, ErrInvalidPosition) {
			t.Errorf("AddBookmark at %v = %v, want ErrInvalidPosition", p, err)
		}
	}
	if bms, _ := f.c.ListBookmarks(t.Context(), f.ann, f.ref(annBookA)); len(bms) != 0 {
		t.Fatalf("a refused bookmark was stored: %+v", bms)
	}
	f.bookmark(t, f.ann, annBookA, 0, "", "")
}

// A span with one unparsable time takes the other for both (a zero-length span
// there), so it never starts after it ends; with neither parsable both are now.
func TestAddHistoryOneSideUnparsable(t *testing.T) {
	f := newAnnotationFixture(t)
	ctx := t.Context()
	f.clock = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct{ start, end, want string }{
		{"garbage", "2026-10-07T10:00:00Z", "2026-10-07T10:00:00.000Z"},
		{"2026-10-07T09:00:00Z", "", "2026-10-07T09:00:00.000Z"},
		{"", "nope", "2026-10-07T12:00:00.000Z"},
	} {
		if err := f.c.AddHistory(ctx, f.bob, f.ref(annGhost), 0, 1, c.start, c.end); err != nil {
			t.Fatal(err)
		}
		got, err := f.c.ListHistory(ctx, f.bob, f.ref(annGhost), 1)
		if err != nil || len(got) != 1 {
			t.Fatalf("history = %+v %v", got, err)
		}
		if h := got[0]; h.StartedAt != c.want || h.EndedAt != c.want {
			t.Errorf("AddHistory(%q, %q) stored %q..%q, want both %q", c.start, c.end, h.StartedAt, h.EndedAt, c.want)
		}
		if _, err := f.c.db.ExecContext(ctx, `DELETE FROM listening_history`); err != nil {
			t.Fatal(err)
		}
	}
}

// A page holds at most 500 rows; absent or non-positive is the default 100.
func TestClampPageLimit(t *testing.T) {
	for in, want := range map[int]int{0: 100, -5: 100, 1: 1, 100: 100, 500: 500, 501: 500, 1 << 20: 500} {
		if got := clampPageLimit(in); got != want {
			t.Errorf("clampPageLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

package catalog

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// annotationFixture is a catalog with a controllable clock, one library with two
// indexed books, and two users.
type annotationFixture struct {
	c        *Catalog
	clock    time.Time
	lib      int64
	ann, bob int64
	all      []Scope // the whole library
}

const (
	annBookA = "A/Book"  // indexed
	annBookB = "B/Book"  // indexed
	annGhost = "C/Ghost" // not indexed
)

func newAnnotationFixture(t *testing.T) *annotationFixture {
	t.Helper()
	c, ctx := newTestCatalog(t)
	f := &annotationFixture{c: c, clock: time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)}
	c.now = func() time.Time { return f.clock }
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	f.lib = lib.ID
	f.all = []Scope{{LibraryID: lib.ID, AllowAll: true}}
	for _, p := range []string{annBookA, annBookB} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: p}); err != nil {
			t.Fatal(err)
		}
	}
	f.ann, f.bob = seedNamedUser(t, c, "ann"), seedNamedUser(t, c, "bob")
	return f
}

func (f *annotationFixture) ref(p string) Ref { return Ref{LibraryID: f.lib, Path: p} }

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
		if err := CheckBookmark("", l); err != nil {
			t.Errorf("label %q refused: %v", l, err)
		}
	}
	for _, l := range []string{"Quote", "1quote", "_quote", "fell-asleep", " quote", "quote ", "quoté",
		"a" + strings.Repeat("b", 32), "fell asleep"} {
		if err := CheckBookmark("", l); !errors.Is(err, ErrInvalidLabel) {
			t.Errorf("label %q = %v, want ErrInvalidLabel", l, err)
		}
	}
	if err := CheckBookmark(strings.Repeat("é", MaxBookmarkNote), ""); err != nil {
		t.Errorf("a note of %d two-byte characters refused: %v", MaxBookmarkNote, err)
	}
	if err := CheckBookmark(strings.Repeat("a", MaxBookmarkNote+1), ""); !errors.Is(err, ErrBookmarkNoteTooLong) {
		t.Errorf("an over-long note = %v, want ErrBookmarkNoteTooLong", err)
	}
	if err := checkNoteBody(strings.Repeat("é", MaxNoteBody)); err != nil {
		t.Errorf("a body of %d characters refused: %v", MaxNoteBody, err)
	}
	if err := checkNoteBody(strings.Repeat("a", MaxNoteBody+1)); !errors.Is(err, ErrNoteBodyTooLong) {
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

// pageAll walks a list a page at a time (limit rows each), returning every row's
// id in order and the number of pages.
func pageAll(t *testing.T, limit int, page func(PageOptions) ([]int64, string, error)) ([]int64, int) {
	t.Helper()
	var ids []int64
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
		ids = append(ids, got...)
		if next == "" {
			return ids, pages
		}
		if pages > 100 {
			t.Fatal("paging does not end")
		}
		cursor = next
	}
}

// The all-books lists, newest first, a page at a time: ties in the timestamp are
// broken by id so no row is lost or repeated across pages; rows outside the
// caller's current access are left out in the query (pages stay full) but kept;
// another user's rows never appear; each row carries its book when one is indexed.
func TestAnnotationListsPage(t *testing.T) {
	f := newAnnotationFixture(t)
	ctx := t.Context()
	wantBM, wantNote, wantHist := []int64{}, []int64{}, []int64{}
	paths := []string{annBookA, annBookB, annGhost}
	// 11 rows each over 4 timestamps (ties), on 3 paths; bob's rows interleaved.
	for i := range 11 {
		f.clock = time.Date(2026, 10, 7, 9, i/3, 0, 0, time.UTC)
		p := paths[i%3]
		wantBM = append(wantBM, f.bookmark(t, f.ann, p, float64(i), "", "").ID)
		wantNote = append(wantNote, f.note(t, f.ann, p, float64(i), fmt.Sprint(i)).ID)
		f.bookmark(t, f.bob, p, 1, "", "")
		f.note(t, f.bob, p, 1, "bob")
		ended := fmt.Sprintf("2026-10-07T10:0%d:00Z", i/3)
		if err := f.c.AddHistory(ctx, f.ann, f.ref(p), 0, 1, ended, ended); err != nil {
			t.Fatal(err)
		}
		if err := f.c.AddHistory(ctx, f.bob, f.ref(p), 0, 1, ended, ended); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := f.c.ListAllHistory(ctx, f.ann, f.all, PageOptions{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hist.History {
		wantHist = append(wantHist, h.ID)
	}
	if len(wantHist) != 11 {
		t.Fatalf("ann's history = %d rows, want 11", len(wantHist))
	}
	newestFirst := func(ids []int64) []int64 { // timestamps rise with the id here
		out := make([]int64, len(ids))
		for i, id := range ids {
			out[len(ids)-1-i] = id
		}
		return out
	}

	lists := []struct {
		name string
		want []int64
		page func(user int64, scopes []Scope) func(PageOptions) ([]int64, string, error)
		book func(user int64, scopes []Scope) map[string]bool // path -> has a book
	}{
		{"bookmarks", newestFirst(wantBM), func(user int64, scopes []Scope) func(PageOptions) ([]int64, string, error) {
			return func(o PageOptions) ([]int64, string, error) {
				p, err := f.c.ListMyBookmarks(ctx, user, scopes, o)
				if err != nil {
					return nil, "", err
				}
				ids := []int64{}
				for _, b := range p.Bookmarks {
					ids = append(ids, b.ID)
				}
				return ids, p.NextCursor, nil
			}
		}, func(user int64, scopes []Scope) map[string]bool {
			p, err := f.c.ListMyBookmarks(ctx, user, scopes, PageOptions{Limit: 500})
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]bool{}
			for _, b := range p.Bookmarks {
				out[b.Path] = b.Book != nil && b.Book.RelPath == b.Path
			}
			return out
		}},
		{"notes", newestFirst(wantNote), func(user int64, scopes []Scope) func(PageOptions) ([]int64, string, error) {
			return func(o PageOptions) ([]int64, string, error) {
				p, err := f.c.ListMyNotes(ctx, user, scopes, o)
				if err != nil {
					return nil, "", err
				}
				ids := []int64{}
				for _, n := range p.Notes {
					ids = append(ids, n.ID)
				}
				return ids, p.NextCursor, nil
			}
		}, func(user int64, scopes []Scope) map[string]bool {
			p, err := f.c.ListMyNotes(ctx, user, scopes, PageOptions{Limit: 500})
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]bool{}
			for _, n := range p.Notes {
				out[n.Path] = n.Book != nil && n.Book.RelPath == n.Path
			}
			return out
		}},
		{"history", wantHist, func(user int64, scopes []Scope) func(PageOptions) ([]int64, string, error) {
			return func(o PageOptions) ([]int64, string, error) {
				p, err := f.c.ListAllHistory(ctx, user, scopes, o)
				if err != nil {
					return nil, "", err
				}
				ids := []int64{}
				for _, h := range p.History {
					ids = append(ids, h.ID)
				}
				return ids, p.NextCursor, nil
			}
		}, func(user int64, scopes []Scope) map[string]bool {
			p, err := f.c.ListAllHistory(ctx, user, scopes, PageOptions{Limit: 500})
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]bool{}
			for _, h := range p.History {
				out[h.Path] = h.Book != nil && h.Book.RelPath == h.Path
			}
			return out
		}},
	}
	join := func(ids []int64) string { return fmt.Sprint(ids) }
	for _, l := range lists {
		t.Run(l.name, func(t *testing.T) {
			if l.name == "history" {
				// Its want came from the list itself: check that order is newest first.
				for i := 1; i < len(hist.History); i++ {
					a, b := hist.History[i-1], hist.History[i]
					if a.EndedAt < b.EndedAt || (a.EndedAt == b.EndedAt && a.ID < b.ID) {
						t.Fatalf("history out of order at %d: %+v then %+v", i, a, b)
					}
				}
			}
			for _, limit := range []int{1, 2, 3, 4, 10, 11, 12} {
				got, pages := pageAll(t, limit, l.page(f.ann, f.all))
				if join(got) != join(l.want) {
					t.Fatalf("limit %d: %v, want %v", limit, got, l.want)
				}
				if want := (len(l.want) + limit - 1) / limit; pages != want {
					t.Fatalf("limit %d: %d pages, want %d", limit, pages, want)
				}
			}

			// Access "A" only: the rows on B and C are left out, before paging.
			scoped := []Scope{{LibraryID: f.lib, Paths: []string{"A"}}}
			all := l.book(f.ann, f.all)
			if len(all) != 3 || !all[annBookA] || !all[annBookB] || all[annGhost] {
				t.Fatalf("books by path = %v, want A and B indexed, C not", all)
			}
			got, _ := pageAll(t, 2, l.page(f.ann, scoped))
			full, _ := pageAll(t, 500, l.page(f.ann, f.all))
			var wantScoped []int64 // got's rows in the full list's order
			for _, id := range full {
				if slices.Contains(got, id) {
					wantScoped = append(wantScoped, id)
				}
			}
			if len(got) != 4 || join(got) != join(wantScoped) {
				t.Fatalf("scoped to A: %v (from %v), want the 4 rows on A in order", got, full)
			}
			if p := l.book(f.ann, scoped); len(p) != 1 || !p[annBookA] {
				t.Fatalf("scoped to A: paths %v", p)
			}
			if got, _ := pageAll(t, 5, l.page(f.ann, nil)); len(got) != 0 {
				t.Fatalf("no access: %v", got)
			}
			// Kept: widening the access again shows them.
			if got, _ := pageAll(t, 500, l.page(f.ann, f.all)); len(got) != 11 {
				t.Fatalf("after re-granting: %d rows", len(got))
			}

			// Bob's own rows, never ann's.
			bobs, _ := pageAll(t, 3, l.page(f.bob, f.all))
			for _, id := range bobs {
				if slices.Contains(l.want, id) {
					t.Fatalf("bob's list holds ann's row %d", id)
				}
			}
			if len(bobs) != 11 {
				t.Fatalf("bob's list = %d rows, want 11", len(bobs))
			}
		})
	}
}

// A cursor that doesn't decode is ErrInvalidCursor; a sort value holding a NUL
// (a client's ended_at) still round-trips.
func TestAnnotationListsCursor(t *testing.T) {
	f := newAnnotationFixture(t)
	ctx := t.Context()
	for _, c := range []string{"!!!", base64.RawURLEncoding.EncodeToString([]byte("no separator")),
		base64.RawURLEncoding.EncodeToString([]byte("2026\x00twelve")), "a b"} {
		if _, err := f.c.ListMyBookmarks(ctx, f.ann, f.all, PageOptions{Cursor: c}); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("bookmarks cursor %q = %v, want ErrInvalidCursor", c, err)
		}
		if _, err := f.c.ListMyNotes(ctx, f.ann, f.all, PageOptions{Cursor: c}); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("notes cursor %q = %v, want ErrInvalidCursor", c, err)
		}
		if _, err := f.c.ListAllHistory(ctx, f.ann, f.all, PageOptions{Cursor: c}); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("history cursor %q = %v, want ErrInvalidCursor", c, err)
		}
	}
	if v, id, err := decodeCursor(encodeCursor("odd\x00value", 42)); err != nil || v != "odd\x00value" || id != 42 {
		t.Fatalf("a NUL in the value: %q %d %v", v, id, err)
	}
	for _, ended := range []string{"x\x00a", "x\x00b"} {
		if err := f.c.AddHistory(ctx, f.ann, f.ref(annBookA), 0, 1, ended, ended); err != nil {
			t.Fatal(err)
		}
	}
	p, err := f.c.ListAllHistory(ctx, f.ann, f.all, PageOptions{Limit: 1})
	if err != nil || len(p.History) != 1 || p.NextCursor == "" {
		t.Fatalf("first page = %+v %v", p, err)
	}
	p2, err := f.c.ListAllHistory(ctx, f.ann, f.all, PageOptions{Limit: 1, Cursor: p.NextCursor})
	if err != nil || len(p2.History) != 1 || p2.History[0].ID == p.History[0].ID || p2.NextCursor != "" {
		t.Fatalf("second page = %+v %v", p2, err)
	}
	// Empty lists are [], never null.
	empty, err := f.c.ListMyBookmarks(ctx, f.bob, f.all, PageOptions{})
	if err != nil || empty.Bookmarks == nil || len(empty.Bookmarks) != 0 || empty.NextCursor != "" {
		t.Fatalf("bob's empty bookmarks = %+v %v", empty, err)
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

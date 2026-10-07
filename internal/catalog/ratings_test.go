package catalog

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// ratingFixture is the user-state fixture with the rating helpers.
type ratingFixture struct{ *userStateFixture }

func newRatingFixture(t *testing.T) *ratingFixture {
	t.Helper()
	return &ratingFixture{newUserStateFixture(t, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))}
}

// rate stores a rating a minute after the last one, so each is newer.
func (f *ratingFixture) rate(t *testing.T, user int64, p string, stars int, note string) *Rating {
	t.Helper()
	f.clock = f.clock.Add(time.Minute)
	r, err := f.c.SetRating(t.Context(), user, f.ref(p), stars, note)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *ratingFixture) get(t *testing.T, user int64, p string) *Rating {
	t.Helper()
	r, err := f.c.GetRating(t.Context(), user, f.ref(p))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRatingsSetGetDelete(t *testing.T) {
	f := newRatingFixture(t)
	ctx := t.Context()
	if r := f.get(t, f.ann, "A/Book"); r != nil {
		t.Fatalf("no rating yet = %+v, want nil", r)
	}
	first := f.rate(t, f.ann, "A/Book", 4, "  loved the ending \n")
	if first.Stars != 4 || first.Note != "loved the ending" || first.Path != "A/Book" ||
		first.CreatedAt != "2026-10-01T09:01:00.000Z" || first.UpdatedAt != first.CreatedAt {
		t.Fatalf("first rating = %+v", first)
	}
	// Re-rating replaces it and keeps when it was first rated.
	again := f.rate(t, f.ann, "A/Book", 2, "")
	if again.Stars != 2 || again.Note != "" || again.CreatedAt != first.CreatedAt || again.UpdatedAt != "2026-10-01T09:02:00.000Z" {
		t.Fatalf("re-rating = %+v", again)
	}
	if got := f.get(t, f.ann, "A/Book"); got == nil || *got != *again {
		t.Fatalf("get = %+v, want %+v", got, again)
	}
	// Another listener's rating is theirs alone.
	if r := f.get(t, f.bob, "A/Book"); r != nil {
		t.Fatalf("bob sees ann's rating: %+v", r)
	}

	// Refused: stars outside 1-5, a note over 500 characters (runes, after trimming).
	for _, stars := range []int{0, 6, -1} {
		if _, err := f.c.SetRating(ctx, f.ann, f.ref("A/Book"), stars, ""); !errors.Is(err, ErrInvalidRating) {
			t.Errorf("stars %d: err = %v, want ErrInvalidRating", stars, err)
		}
	}
	if _, err := f.c.SetRating(ctx, f.ann, f.ref("A/Book"), 3, strings.Repeat("é", 501)); !errors.Is(err, ErrRatingNoteTooLong) {
		t.Errorf("501-rune note: err = %v, want ErrRatingNoteTooLong", err)
	}
	if r, err := f.c.SetRating(ctx, f.ann, f.ref("A/Book"), 3, " "+strings.Repeat("é", 500)+" "); err != nil || len([]rune(r.Note)) != 500 {
		t.Errorf("500-rune note: %v", err)
	}
	if got := f.get(t, f.ann, "A/Book"); got.Stars != 3 {
		t.Fatalf("a refused rating changed the stored one: %+v", got)
	}

	// Delete is idempotent and leaves others' ratings alone.
	f.rate(t, f.bob, "A/Book", 5, "")
	for range 2 {
		if err := f.c.DeleteRating(ctx, f.ann, f.ref("A/Book")); err != nil {
			t.Fatal(err)
		}
	}
	if f.get(t, f.ann, "A/Book") != nil || f.get(t, f.bob, "A/Book") == nil {
		t.Fatal("delete removed the wrong rating")
	}
}

func TestListRatingsScopedNewestFirstWithBooks(t *testing.T) {
	f := newRatingFixture(t)
	ctx := t.Context()
	if _, err := f.c.UpsertBook(ctx, &Book{LibraryID: f.lib, RelPath: "A/Indexed", IsFolder: true, Title: "Indexed",
		Author: "Ann Author"}); err != nil {
		t.Fatal(err)
	}
	other, err := f.c.CreateLibrary(ctx, Library{Name: "M", Root: "/tmp/m"})
	if err != nil {
		t.Fatal(err)
	}
	f.rate(t, f.ann, "A/Indexed", 5, "")
	f.rate(t, f.ann, "B/Unindexed", 3, "")
	f.clock = f.clock.Add(time.Minute)
	if _, err := f.c.SetRating(ctx, f.ann, Ref{LibraryID: other.ID, Path: "C/Elsewhere"}, 1, ""); err != nil {
		t.Fatal(err)
	}
	f.rate(t, f.bob, "A/Indexed", 2, "")

	all := []Scope{{LibraryID: f.lib, AllowAll: true}, {LibraryID: other.ID, AllowAll: true}}
	list, err := f.c.ListRatings(ctx, f.ann, all)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Path != "C/Elsewhere" || list[1].Path != "B/Unindexed" || list[2].Path != "A/Indexed" {
		t.Fatalf("ann's ratings (newest first, never bob's) = %+v", list)
	}
	if list[1].Book != nil || list[2].Book == nil || list[2].Book.Title != "Indexed" {
		t.Fatalf("books: unindexed %+v, indexed %+v", list[1].Book, list[2].Book)
	}

	// Denied: a path outside the caller's current access is kept but not returned.
	narrowed := []Scope{{LibraryID: f.lib, Paths: []string{"A"}}}
	list, err = f.c.ListRatings(ctx, f.ann, narrowed)
	if err != nil || len(list) != 1 || list[0].Path != "A/Indexed" {
		t.Fatalf("narrowed list = %+v %v", list, err)
	}
	if list, err := f.c.ListRatings(ctx, f.ann, nil); err != nil || len(list) != 0 {
		t.Fatalf("no access = %+v %v, want none", list, err)
	}
	if f.get(t, f.ann, "B/Unindexed") == nil {
		t.Fatal("a hidden rating was deleted")
	}
}

// A rating follows its book's move; where the listener already rated the new path,
// the newer rating wins whole.
func TestRatingsMoveWithTheBook(t *testing.T) {
	f := newRatingFixture(t)
	ctx := t.Context()
	f.rate(t, f.ann, "old/Book", 4, "moved")
	if err := f.c.MoveDurableState(ctx, f.lib, "old/Book", "new/Book"); err != nil {
		t.Fatal(err)
	}
	if f.get(t, f.ann, "old/Book") != nil {
		t.Fatal("the rating stayed at the old path")
	}
	if r := f.get(t, f.ann, "new/Book"); r == nil || r.Stars != 4 || r.Note != "moved" {
		t.Fatalf("moved rating = %+v", r)
	}

	// Collision, the moved rating newer: it wins, created_at the earlier one.
	f.rate(t, f.ann, "dest/Book", 1, "stale")
	newer := f.rate(t, f.ann, "src/Book", 5, "newer")
	// Collision, the destination newer: bob keeps his.
	f.rate(t, f.bob, "src/Book", 2, "bob old")
	bobDest := f.rate(t, f.bob, "dest/Book", 3, "bob new")
	if err := f.c.MoveDurableState(ctx, f.lib, "src/Book", "dest/Book"); err != nil {
		t.Fatal(err)
	}
	got := f.get(t, f.ann, "dest/Book")
	if got == nil || *got != (Rating{Ref: f.ref("dest/Book"), Stars: 5, Note: "newer", CreatedAt: newer.CreatedAt, UpdatedAt: newer.UpdatedAt}) {
		t.Fatalf("ann after the collision = %+v, want the newer %+v", got, newer)
	}
	if got := f.get(t, f.bob, "dest/Book"); got == nil || *got != *bobDest {
		t.Fatalf("bob after the collision = %+v, want his newer %+v", got, bobDest)
	}
	if f.get(t, f.ann, "src/Book") != nil || f.get(t, f.bob, "src/Book") != nil {
		t.Fatal("ratings stayed at the moved-from path")
	}
}

// Joining disc books carries their ratings onto the joined book, the newest
// winning per listener; an unplaced part keeps its own.
func TestRatingsJoin(t *testing.T) {
	f := newRatingFixture(t)
	const into = "Author/Book"
	f.rate(t, f.ann, into+"/CD1", 2, "disc one")
	f.rate(t, f.ann, into+"/CD2", 4, "disc two")
	f.rate(t, f.bob, into+"/CD3", 5, "unplaced")
	parts := []JoinPart{{Path: into + "/CD1"}, {Path: into + "/CD2", Offset: 100}, {Path: into + "/CD3", Unplaced: true, Last: true}}
	if err := f.c.JoinDurableState(t.Context(), f.lib, into, parts, 0); err != nil {
		t.Fatal(err)
	}
	if r := f.get(t, f.ann, into); r == nil || r.Stars != 4 || r.Note != "disc two" {
		t.Fatalf("ann's joined rating = %+v, want the newest part's", r)
	}
	if f.get(t, f.ann, into+"/CD1") != nil || f.get(t, f.ann, into+"/CD2") != nil {
		t.Fatal("part ratings stayed behind")
	}
	if f.get(t, f.bob, into) != nil || f.get(t, f.bob, into+"/CD3") == nil {
		t.Fatal("an unplaced part's rating should stay on its own path")
	}
}

// Ratings are purged with their user and with their library (FK cascade).
func TestRatingsPurgedWithUserAndLibrary(t *testing.T) {
	f := newRatingFixture(t)
	ctx := t.Context()
	f.rate(t, f.ann, "A/Book", 4, "")
	f.rate(t, f.bob, "A/Book", 3, "")
	count := func() (n int) {
		t.Helper()
		if err := f.c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ratings`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := f.c.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, f.ann); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 || f.get(t, f.bob, "A/Book") == nil {
		t.Fatalf("after deleting ann: %d ratings, want bob's only", n)
	}
	if err := f.c.DeleteLibrary(ctx, f.lib); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("after deleting the library: %d ratings, want 0", n)
	}
}

package catalog

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// queuePaths reads a user's queue as paths, in order.
func queuePaths(t *testing.T, c *Catalog, user int64, scopes []Scope) []string {
	t.Helper()
	items, err := c.Queue(t.Context(), user, scopes)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, it := range items {
		out = append(out, it.Path)
	}
	return out
}

func wantPaths(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func intp(n int) *int { return &n }

// Adds land at the end or at a position (past the end = the end); an already
// queued book stays put unless a position is given, then moves; a remove is
// idempotent and the order holds after it.
func TestQueueAddMoveRemove(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	user := seedUser(t, c, ctx)
	all := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	ref := func(p string) Ref { return Ref{LibraryID: lib.ID, Path: p} }
	add := func(p string, pos *int) {
		t.Helper()
		if err := c.AddToQueue(ctx, user, ref(p), pos, all); err != nil {
			t.Fatal(err)
		}
	}

	add("a", nil)
	add("b", nil)
	add("c", nil)
	add("d", intp(1))
	wantPaths(t, "after insert at 1", queuePaths(t, c, user, all), "a", "d", "b", "c")
	add("b", nil) // idempotent: stays where it is
	wantPaths(t, "after re-add", queuePaths(t, c, user, all), "a", "d", "b", "c")
	add("c", intp(0))
	wantPaths(t, "after move to 0", queuePaths(t, c, user, all), "c", "a", "d", "b")
	add("a", intp(99))
	wantPaths(t, "after move past the end", queuePaths(t, c, user, all), "c", "d", "b", "a")
	add("e", intp(99))
	wantPaths(t, "after add past the end", queuePaths(t, c, user, all), "c", "d", "b", "a", "e")

	for range 2 { // idempotent
		if err := c.RemoveFromQueue(ctx, user, ref("d")); err != nil {
			t.Fatal(err)
		}
	}
	wantPaths(t, "after remove", queuePaths(t, c, user, all), "c", "b", "a", "e")
	add("f", intp(1))
	wantPaths(t, "after insert past a removed gap", queuePaths(t, c, user, all), "c", "f", "b", "a", "e")

	// Another user's queue is their own.
	other := seedNamedUser(t, c, "other")
	wantPaths(t, "other user's queue", queuePaths(t, c, other, all))
	if err := c.RemoveFromQueue(ctx, other, ref("c")); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "after another user's remove", queuePaths(t, c, user, all), "c", "f", "b", "a", "e")
}

// A full queue refuses a new book (ErrListFull) but still moves a queued one.
func TestQueueFull(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	user := seedUser(t, c, ctx)
	for i := range MaxQueue {
		if err := c.AddToQueue(ctx, user, Ref{LibraryID: lib.ID, Path: fmt.Sprintf("b%03d", i)}, nil, []Scope{{LibraryID: lib.ID, AllowAll: true}}); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if err := c.AddToQueue(ctx, user, Ref{LibraryID: lib.ID, Path: "one-more"}, nil, []Scope{{LibraryID: lib.ID, AllowAll: true}}); !errors.Is(err, ErrListFull) {
		t.Fatalf("add to a full queue = %v, want ErrListFull", err)
	}
	if err := c.AddToQueue(ctx, user, Ref{LibraryID: lib.ID, Path: "b499"}, intp(0), []Scope{{LibraryID: lib.ID, AllowAll: true}}); err != nil {
		t.Fatalf("move within a full queue: %v", err)
	}
	got := queuePaths(t, c, user, []Scope{{LibraryID: lib.ID, AllowAll: true}})
	if len(got) != MaxQueue || got[0] != "b499" {
		t.Fatalf("full queue = %d entries, first %q", len(got), got[0])
	}
}

// A replace keeps only exactly indexed books in scope, first duplicate wins,
// deletes everything else stored (hidden rows included), keeps a kept entry's
// added_at, and refuses more than MaxQueue refs.
func TestSetQueueSkipRule(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	other, _ := c.CreateLibrary(ctx, Library{Name: "O", Root: "/tmp/o"})
	user := seedUser(t, c, ctx)
	for _, p := range []string{"Kid/A", "Kid/B", "Adult/X"} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: p}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: other.ID, RelPath: "Z", Title: "Z"}); err != nil {
		t.Fatal(err)
	}
	kid := []Scope{{LibraryID: lib.ID, Paths: []string{"Kid"}}}
	ref := func(p string) Ref { return Ref{LibraryID: lib.ID, Path: p} }

	// A row stored while the user could reach it, now hidden.
	if err := c.AddToQueue(ctx, user, ref("Adult/X"), nil, []Scope{{LibraryID: lib.ID, AllowAll: true}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddToQueue(ctx, user, ref("Kid/A"), nil, []Scope{{LibraryID: lib.ID, AllowAll: true}}); err != nil {
		t.Fatal(err)
	}
	before, _ := c.Queue(ctx, user, kid)
	refs := []Ref{
		ref("Kid/B"),
		ref("Kid/../Adult/X"),            // cleans to an out-of-scope path
		ref("Kid/A"),                     // indexed, in scope
		ref("/Kid/B/"),                   // a duplicate once cleaned
		ref("Kid/Nope"),                  // not indexed
		ref("Kid"),                       // a folder, not a book
		{LibraryID: other.ID, Path: "Z"}, // a library the user has no scope in
		ref(""),
	}
	if err := c.SetQueue(ctx, user, refs, kid); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "replaced queue", queuePaths(t, c, user, kid), "Kid/B", "Kid/A")
	// The hidden row went too: the whole library shows only what was kept.
	wantPaths(t, "replaced queue, unscoped", queuePaths(t, c, user, []Scope{{LibraryID: lib.ID, AllowAll: true}}),
		"Kid/B", "Kid/A")
	after, _ := c.Queue(ctx, user, kid)
	if after[1].AddedAt != before[0].AddedAt {
		t.Fatalf("kept entry's added_at moved: %q -> %q", before[0].AddedAt, after[1].AddedAt)
	}
	if after[0].Book == nil || after[0].Book.Title != "Kid/B" {
		t.Fatalf("book not attached: %+v", after[0])
	}

	if err := c.SetQueue(ctx, user, make([]Ref, MaxQueue+1), kid); !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("replace with %d refs = %v, want ErrTooManyItems", MaxQueue+1, err)
	}
	if err := c.SetQueue(ctx, user, make([]Ref, MaxQueue), kid); err != nil {
		t.Fatalf("replace with %d refs: %v", MaxQueue, err)
	}
	wantPaths(t, "replaced with nothing listable", queuePaths(t, c, user, kid))
}

// A queued book outside the reader's current access is kept but not returned,
// and comes back with access; no scopes at all shows nothing.
func TestQueueVisibility(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	user := seedUser(t, c, ctx)
	for _, p := range []string{"Kid/A", "Adult/X"} {
		if err := c.AddToQueue(ctx, user, Ref{LibraryID: lib.ID, Path: p}, nil, []Scope{{LibraryID: lib.ID, AllowAll: true}}); err != nil {
			t.Fatal(err)
		}
	}
	wantPaths(t, "kid scope", queuePaths(t, c, user, []Scope{{LibraryID: lib.ID, Paths: []string{"Kid"}}}), "Kid/A")
	wantPaths(t, "no scope", queuePaths(t, c, user, nil))
	wantPaths(t, "whole library", queuePaths(t, c, user, []Scope{{LibraryID: lib.ID, AllowAll: true}}), "Kid/A", "Adult/X")
	// Unindexed: no book attached.
	items, _ := c.Queue(ctx, user, []Scope{{LibraryID: lib.ID, AllowAll: true}})
	if items[0].Book != nil {
		t.Fatalf("unindexed entry has a book: %+v", items[0])
	}
}

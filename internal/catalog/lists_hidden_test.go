package catalog

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// hiddenList is the up-next queue or a collection, written and read by its owner
// with given scopes, for the hidden-row tests (rows outside those scopes).
type hiddenList struct {
	name  string
	l     orderedList
	owner int64 // the list's owner key: the user, or the collection
	add   func(ref Ref, pos *int, scopes []Scope) error
	paths func(scopes []Scope) []string
}

func hiddenLists(t *testing.T, e *collectionsEnv) []hiddenList {
	t.Helper()
	ctx := t.Context()
	col, err := e.c.CreateCollection(ctx, e.owner, "Mine", "")
	if err != nil {
		t.Fatal(err)
	}
	return []hiddenList{
		{name: "queue", l: upNext, owner: e.owner,
			add: func(ref Ref, pos *int, scopes []Scope) error {
				return e.c.AddToQueue(ctx, e.owner, ref, pos, scopes)
			},
			paths: func(scopes []Scope) []string { return queuePaths(t, e.c, e.owner, scopes) }},
		{name: "collection", l: collectionItems, owner: col.ID,
			add: func(ref Ref, pos *int, scopes []Scope) error {
				return e.c.AddCollectionItem(ctx, col.ID, e.owner, ref, pos, scopes)
			},
			paths: func(scopes []Scope) []string { return itemPaths(t, e.c, col.ID, e.owner, scopes) }},
	}
}

// An add's position is an index among the rows the caller sees: the book lands
// before the visible row at that index (a move below the visible row there with
// a hidden row in front, an insert at a visible index), or just after the last
// visible row when the position is absent or past the visible end; hidden rows
// stay where they are.
func TestListAddAtVisibleIndex(t *testing.T) {
	e := newCollectionsEnv(t)
	for _, list := range hiddenLists(t, e) {
		t.Run(list.name, func(t *testing.T) {
			add := func(p string, pos *int, scopes []Scope) {
				t.Helper()
				if err := list.add(e.ref(p), pos, scopes); err != nil {
					t.Fatal(err)
				}
			}
			check := func(what string, kid []string, all ...string) {
				t.Helper()
				wantPaths(t, what+" (kid)", list.paths(e.kid), kid...)
				wantPaths(t, what+" (all)", list.paths(e.all), all...)
			}
			for _, p := range []string{"Adult/H", "Kid/A", "Kid/B"} {
				add(p, nil, e.all)
			}
			check("seeded", []string{"Kid/A", "Kid/B"}, "Adult/H", "Kid/A", "Kid/B")

			add("Kid/A", intp(1), e.kid)
			check("A moved to 1", []string{"Kid/B", "Kid/A"}, "Adult/H", "Kid/B", "Kid/A")
			add("Kid/C", intp(1), e.kid)
			check("C inserted at 1", []string{"Kid/B", "Kid/C", "Kid/A"}, "Adult/H", "Kid/B", "Kid/C", "Kid/A")
			add("Kid/D", intp(0), e.kid)
			check("D inserted at 0", []string{"Kid/D", "Kid/B", "Kid/C", "Kid/A"}, "Adult/H", "Kid/D", "Kid/B", "Kid/C", "Kid/A")

			add("Adult/T", nil, e.all) // a hidden row at the end
			add("Kid/E", nil, e.kid)
			check("E added", []string{"Kid/D", "Kid/B", "Kid/C", "Kid/A", "Kid/E"},
				"Adult/H", "Kid/D", "Kid/B", "Kid/C", "Kid/A", "Kid/E", "Adult/T")
			add("Kid/D", intp(99), e.kid)
			check("D moved past the end", []string{"Kid/B", "Kid/C", "Kid/A", "Kid/E", "Kid/D"},
				"Adult/H", "Kid/B", "Kid/C", "Kid/A", "Kid/E", "Kid/D", "Adult/T")
			add("Kid/B", intp(4), e.kid) // the last index: four other rows visible
			check("B moved to the last index", []string{"Kid/C", "Kid/A", "Kid/E", "Kid/D", "Kid/B"},
				"Adult/H", "Kid/C", "Kid/A", "Kid/E", "Kid/D", "Kid/B", "Adult/T")

			// Nothing visible: the stored end.
			add("Kid/F", nil, []Scope{{LibraryID: e.lib.ID, Paths: []string{"Kid/F"}}})
			check("F added seeing nothing else", []string{"Kid/C", "Kid/A", "Kid/E", "Kid/D", "Kid/B", "Kid/F"},
				"Adult/H", "Kid/C", "Kid/A", "Kid/E", "Kid/D", "Kid/B", "Adult/T", "Kid/F")
		})
	}
}

// The cap counts the rows the caller sees: a list full only because of hidden
// rows takes a new book by deleting the oldest hidden row (added_at, ties in
// stored order), one per add, gone for good once access returns; a list whose
// visible rows alone are at the cap is the full error, and a move within it
// still works.
func TestListCapEvictsOldestHidden(t *testing.T) {
	e := newCollectionsEnv(t)
	ctx := t.Context()
	for _, list := range hiddenLists(t, e) {
		t.Run(list.name, func(t *testing.T) {
			// Stored full: three hidden rows among visible ones. h2 and h3 are the
			// oldest (a tie, h2 first in order), then h1, which is first in order.
			hidden := map[int]struct{ path, added string }{
				0:              {"Adult/h1", "2026-01-03T00:00:00.000Z"},
				10:             {"Adult/h2", "2026-01-01T00:00:00.000Z"},
				list.l.max - 1: {"Adult/h3", "2026-01-01T00:00:00.000Z"},
			}
			for i := range list.l.max {
				p, added := fmt.Sprintf("Kid/v%04d", i), "2026-01-02T00:00:00.000Z"
				if h, ok := hidden[i]; ok {
					p, added = h.path, h.added
				}
				if _, err := e.c.db.ExecContext(ctx, list.l.insert, list.owner, e.lib.ID, p, 2*i, added); err != nil {
					t.Fatal(err)
				}
			}
			stored := func() int {
				t.Helper()
				return len(list.paths(e.all))
			}
			hiddenLeft := func(want ...string) {
				t.Helper()
				var got []string
				for _, p := range list.paths(e.all) {
					if strings.HasPrefix(p, "Adult/") {
						got = append(got, p)
					}
				}
				wantPaths(t, "hidden rows left", got, want...)
			}

			for i, left := range [][]string{{"Adult/h1", "Adult/h3"}, {"Adult/h1"}, nil} {
				if err := list.add(e.ref(fmt.Sprintf("Kid/new%d", i)), nil, e.kid); err != nil {
					t.Fatalf("add %d to a list full of hidden rows: %v", i, err)
				}
				hiddenLeft(left...)
				if n := stored(); n != list.l.max {
					t.Fatalf("after add %d: %d rows stored, want %d", i, n, list.l.max)
				}
			}
			if err := list.add(e.ref("Kid/one-more"), nil, e.kid); !errors.Is(err, ErrListFull) {
				t.Fatalf("add to a visibly full list = %v, want ErrListFull", err)
			}
			if err := list.add(e.ref("Kid/new2"), intp(0), e.kid); err != nil {
				t.Fatalf("move within a visibly full list: %v", err)
			}
			if got := list.paths(e.kid); len(got) != list.l.max || got[0] != "Kid/new2" {
				t.Fatalf("visibly full list = %d rows, first %q", len(got), got[0])
			}
		})
	}
}

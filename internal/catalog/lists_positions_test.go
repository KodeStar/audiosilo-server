package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

// countingTx counts the statements an orderedList issues.
type countingTx struct {
	tx           *sql.Tx
	reads, execs int
}

func (c *countingTx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	c.reads++
	return c.tx.QueryContext(ctx, q, args...)
}

func (c *countingTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	c.execs++
	return c.tx.ExecContext(ctx, q, args...)
}

// inListTx runs fn on the up-next list in one transaction and reports the
// statements it read and wrote with.
func inListTx(t *testing.T, c *Catalog, fn func(tx listTx) error) (reads, execs int) {
	t.Helper()
	ctr := &countingTx{}
	if err := c.db.WithTx(t.Context(), "test", func(tx *sql.Tx) error {
		ctr.tx = tx
		return fn(ctr)
	}); err != nil {
		t.Fatal(err)
	}
	return ctr.reads, ctr.execs
}

// A one-book change to a long list with gaps in its positions is one read and at
// most two writes (a range shift and the row), whatever the list's length, and
// the order holds; a replace that only drops rows writes only the deletes.
func TestListWritesStayLocal(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	user := seedUser(t, c, ctx)
	all := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	ref := func(p string) Ref { return Ref{LibraryID: lib.ID, Path: p} }
	var want []string
	for i := range MaxQueue - 2 { // stored with gaps, as removes leave them; two adds fill it
		p := fmt.Sprintf("b%03d", i)
		if _, err := c.db.ExecContext(ctx, `INSERT INTO up_next(user_id, library_id, rel_path, position, added_at)
		  VALUES(?,?,?,?,'t')`, user, lib.ID, p, 3*i); err != nil {
			t.Fatal(err)
		}
		want = append(want, p)
	}
	step := func(what string, maxExecs int, fn func(tx listTx) (bool, error)) {
		t.Helper()
		reads, execs := inListTx(t, c, func(tx listTx) error { _, err := fn(tx); return err })
		if what == "remove" {
			reads++ // a remove reads nothing
		}
		if reads != 1 || execs > maxExecs {
			t.Fatalf("%s: %d reads, %d writes; want 1 read, at most %d writes", what, reads, execs, maxExecs)
		}
		wantPaths(t, what, queuePaths(t, c, user, all), want...)
	}

	want = slices.Insert(want, 0, "new")
	step("insert at the front", 2, func(tx listTx) (bool, error) {
		return upNext.add(ctx, tx, user, ref("new"), intp(0), "now")
	})
	want = slices.Insert(want, 250, "mid")
	step("insert in the middle", 2, func(tx listTx) (bool, error) {
		return upNext.add(ctx, tx, user, ref("mid"), intp(250), "now")
	})
	want = slices.Insert(want[:len(want)-1], 0, "b497")
	step("move the last to the front", 2, func(tx listTx) (bool, error) {
		return upNext.add(ctx, tx, user, ref("b497"), intp(0), "now")
	})
	want = append(slices.Delete(want, 1, 2), "new")
	step("move to the end", 1, func(tx listTx) (bool, error) {
		return upNext.add(ctx, tx, user, ref("new"), intp(MaxQueue), "now")
	})
	step("move to where it is", 0, func(tx listTx) (bool, error) {
		return upNext.add(ctx, tx, user, ref("mid"), intp(slices.Index(want, "mid")), "now")
	})
	want = slices.DeleteFunc(want, func(p string) bool { return p == "b100" })
	step("remove", 1, func(tx listTx) (bool, error) {
		return upNext.drop(ctx, tx, user, ref("b100"))
	})

	// A replace keeping the order and dropping three writes only the deletes.
	kept := slices.DeleteFunc(slices.Clone(want), func(p string) bool { return p == "b007" || p == "b200" || p == "b300" })
	refs := make([]Ref, len(kept))
	for i, p := range kept {
		refs[i] = ref(p)
	}
	want = kept
	step("replace dropping three", 3, func(tx listTx) (bool, error) {
		return upNext.replace(ctx, tx, user, refs, "now")
	})
}

// Random adds, moves, removes and replaces keep the order a plain slice model
// says, gaps and all.
func TestListOrderMatchesModel(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	user := seedUser(t, c, ctx)
	all := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	ref := func(p string) Ref { return Ref{LibraryID: lib.ID, Path: p} }
	rng := rand.New(rand.NewPCG(1, 2))
	model := []string{}
	for op := range 400 {
		p := fmt.Sprintf("b%02d", rng.IntN(40))
		at := slices.Index(model, p)
		switch k := rng.IntN(10); {
		case k < 5: // add or move, maybe at a position
			var pos *int
			if rng.IntN(3) > 0 {
				pos = intp(rng.IntN(len(model) + 3))
			}
			if err := c.db.WithTx(ctx, "add", func(tx *sql.Tx) error {
				_, err := upNext.add(ctx, tx, user, ref(p), pos, "now")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if at >= 0 && pos == nil {
				break
			}
			if at >= 0 {
				model = slices.Delete(model, at, at+1)
			}
			i := len(model)
			if pos != nil {
				i = min(*pos, len(model))
			}
			model = slices.Insert(model, i, p)
		case k < 8:
			if err := c.RemoveFromQueue(ctx, user, ref(p)); err != nil {
				t.Fatal(err)
			}
			if at >= 0 {
				model = slices.Delete(model, at, at+1)
			}
		default: // a replace: the model shuffled, some dropped, some added
			next := slices.Clone(model)
			rng.Shuffle(len(next), func(i, j int) { next[i], next[j] = next[j], next[i] })
			if len(next) > 2 && rng.IntN(2) == 0 {
				next = next[:len(next)-2]
			}
			for range rng.IntN(3) {
				if q := fmt.Sprintf("b%02d", rng.IntN(40)); !slices.Contains(next, q) {
					next = slices.Insert(next, rng.IntN(len(next)+1), q)
				}
			}
			refs := make([]Ref, len(next))
			for i, q := range next {
				refs[i] = ref(q)
			}
			if err := c.db.WithTx(ctx, "replace", func(tx *sql.Tx) error {
				_, err := upNext.replace(ctx, tx, user, refs, "now")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			model = next
		}
		wantPaths(t, fmt.Sprintf("after op %d", op), queuePaths(t, c, user, all), model...)
	}
}

// Equal positions (only a hand-written database has them) order by path, and an
// add between two of them still lands at its index.
func TestListAddBetweenEqualPositions(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	user := seedUser(t, c, ctx)
	for _, p := range []string{"a", "b", "c"} {
		if _, err := c.db.ExecContext(ctx, `INSERT INTO up_next(user_id, library_id, rel_path, position, added_at)
		  VALUES(?,?,?,5,'t')`, user, lib.ID, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.AddToQueue(ctx, user, Ref{LibraryID: lib.ID, Path: "x"}, intp(1)); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "queue", queuePaths(t, c, user, []Scope{{LibraryID: lib.ID, AllowAll: true}}), "a", "x", "b", "c")
}

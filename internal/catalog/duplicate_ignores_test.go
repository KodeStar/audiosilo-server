package catalog

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestDuplicateComparisonIgnores(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/l"})
	other, _ := c.CreateLibrary(ctx, Library{Name: "M", Root: "/m"})
	uid := seedUser(t, c, ctx)
	a := Ref{lib.ID, "Best"}
	b := Ref{lib.ID, "Second"}
	x := Ref{other.ID, "Second"}
	put := func(r Ref, asin string) {
		t.Helper()
		f := "mp3"
		if r == a {
			f = "m4b"
		}
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: r.LibraryID, RelPath: r.Path, Title: r.Path, ASIN: asin, Format: f}); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []Ref{a, b, x} {
		put(r, "same")
	}
	key := func(refs ...Ref) string { return comparisonKey(refs) }
	check := func(ignored, across bool, want ...string) {
		t.Helper()
		// All three comparisons may share the first copy across page boundaries.
		q := DuplicateQuery{Ignored: ignored, Across: across, Limit: 1}
		var keys []string
		for {
			page, err := c.DuplicatePage(ctx, q)
			if err != nil || page.Total != len(want) || len(keys) > len(want) {
				t.Fatalf("page %+v, want %v: %v", page, want, err)
			}
			for _, g := range page.Groups {
				keys = append(keys, g.Key)
			}
			if page.NextCursor == "" {
				break
			}
			q.Cursor = page.NextCursor
		}
		slices.Sort(keys)
		slices.Sort(want)
		if !slices.Equal(keys, want) {
			t.Fatalf("comparison keys = %q, want %q", keys, want)
		}
	}
	check(false, true, key(a, b), key(a, x), key(b, x))
	if err := c.IgnoreIssue(ctx, IssueDuplicate, []Ref{x, a}, uid); err != nil {
		t.Fatal(err)
	}
	check(true, true, key(a, x))
	check(false, true, key(a, b), key(b, x))
	if err := c.IgnoreIssue(ctx, IssueDuplicate, []Ref{b, a}, uid); err != nil {
		t.Fatal(err)
	}
	check(true, false, key(a, b))
	check(false, true, key(b, x))
	if err := c.UnignoreIssue(ctx, IssueDuplicate, []Ref{a, x}); err != nil {
		t.Fatal(err)
	}
	check(false, true, key(a, x), key(b, x))

	d, e := Ref{lib.ID, "D"}, Ref{lib.ID, "E"}
	put(d, "another")
	put(e, "another")
	if err := c.IgnoreIssue(ctx, IssueDuplicate, []Ref{d, e}, uid); err != nil {
		t.Fatal(err)
	}
	put(a, "regroup")
	put(d, "regroup")
	check(false, false, key(a, d))
	check(true, false)
	put(a, "same")
	put(d, "another")
	check(true, false, key(a, b), key(d, e))
	put(d, "same")
	check(false, false, key(a, b, d))
	// The exact decision survives an index rebuild, a moved path and a retry.
	if _, err := c.db.ExecContext(ctx, `DELETE FROM books WHERE library_id=? AND rel_path=?`, lib.ID, b.Path); err != nil {
		t.Fatal(err)
	}
	put(d, "another")
	moved := Ref{b.LibraryID, "A & B/\"Moved\""}
	if err := c.MoveDurableState(ctx, b.LibraryID, b.Path, moved.Path); err != nil {
		t.Fatal(err)
	}
	put(moved, "same")
	check(true, false, key(a, moved), key(d, e))
	if err := c.UnignoreIssue(ctx, IssueDuplicate, []Ref{moved, a}); err != nil {
		t.Fatal(err)
	}
	check(false, false, key(a, moved))
	for _, bad := range [][]Ref{{a}, {a, a}, {a, b, x}} {
		if err := c.IgnoreIssue(ctx, IssueDuplicate, bad, uid); !errors.Is(err, ErrDuplicateSelection) {
			t.Fatalf("invalid selection %v: %v", bad, err)
		}
	}
}

func TestLegacyDuplicateMappingAndUndo(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	l, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/l"})
	m, _ := c.CreateLibrary(ctx, Library{Name: "M", Root: "/m"})
	write := func(q string, args ...any) {
		t.Helper()
		if _, err := c.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	put := func(r Ref, asin string) {
		t.Helper()
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: r.LibraryID, RelPath: r.Path, Title: r.Path, ASIN: asin}); err != nil {
			t.Fatal(err)
		}
	}
	a, b, x := Ref{l.ID, "A"}, Ref{l.ID, "B"}, Ref{m.ID, "X"}
	for _, r := range []Ref{a, b, x} {
		put(r, "one")
		write(`INSERT INTO issue_ignores(library_id,path,kind,created_at) VALUES(?,?,'duplicate',?)`, r.LibraryID, r.Path, r.Path)
	}
	y := Ref{m.ID, "Missing"}
	write(`INSERT INTO issue_ignores(library_id,path,kind,created_at) VALUES(?,?,'duplicate','other')`, m.ID, y.Path)
	write(`INSERT INTO server_state VALUES('map_duplicate_ignores_0092','true','t')`)
	for range 2 {
		if err := c.MapLegacyDuplicateIgnores(ctx); err != nil {
			t.Fatal(err)
		}
	}
	check := func(ignored bool, expected int) {
		t.Helper()
		page, err := c.DuplicatePage(ctx, DuplicateQuery{Ignored: ignored, Across: true})
		if err != nil || page.Total != expected {
			t.Fatalf("ignored %v: %+v (%v), want %d", ignored, page, err, expected)
		}
	}
	check(true, 3)
	put(y, "one") // unmapped at upgrade: retains its old coverage
	check(true, 6)
	if err := c.UnignoreIssue(ctx, IssueDuplicate, []Ref{y, a}); err != nil {
		t.Fatal(err)
	}
	check(true, 5)
	check(false, 1)
}

// Suggest again on a mapped group stands when a copy with an unmapped flag
// joins it later: the old Suggest again cleared both copies' flags, so the
// all-members fallback must not hide the larger group. Comparisons that don't
// hold the restored one keep the fallback.
func TestSuggestAgainOutlastsALaterLegacyCopy(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	l, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/l"})
	m, _ := c.CreateLibrary(ctx, Library{Name: "M", Root: "/m"})
	write := func(q string, args ...any) {
		t.Helper()
		if _, err := c.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	put := func(r Ref) {
		t.Helper()
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: r.LibraryID, RelPath: r.Path, Title: r.Path, ASIN: "one"}); err != nil {
			t.Fatal(err)
		}
	}
	a, b, late, y := Ref{l.ID, "A"}, Ref{l.ID, "B"}, Ref{l.ID, "Late"}, Ref{m.ID, "Y"}
	put(a)
	put(b)
	for _, r := range []Ref{a, b, late, y} {
		write(`INSERT INTO issue_ignores(library_id,path,kind,created_at) VALUES(?,?,'duplicate',?)`, r.LibraryID, r.Path, r.Path)
	}
	write(`INSERT INTO server_state VALUES('map_duplicate_ignores_0092','true','t')`)
	if err := c.MapLegacyDuplicateIgnores(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.UnignoreIssue(ctx, IssueDuplicate, []Ref{b, a}); err != nil {
		t.Fatal(err)
	}
	put(late)
	put(y)
	if page, err := c.DuplicatePage(ctx, DuplicateQuery{Across: true}); err != nil || page.Total != 1 || page.Groups[0].Key != comparisonKey([]Ref{a, b, late}) {
		t.Fatalf("open comparisons %+v (%v), want A/B/Late", page, err)
	}
	if page, err := c.DuplicatePage(ctx, DuplicateQuery{Across: true, Ignored: true}); err != nil || page.Total != 3 {
		t.Fatalf("ignored comparisons %+v (%v), want the three pairs with Y", page, err)
	}
}

// The old across view showed a component whole while any copy in it had no
// flag. Its flagged pair stays on show after the upgrade, over the decision
// 0092 recovered from the request's stamp, until an admin decides it.
func TestLegacyMixedComponentPairStaysShown(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	l, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/l"})
	m, _ := c.CreateLibrary(ctx, Library{Name: "M", Root: "/m"})
	n, _ := c.CreateLibrary(ctx, Library{Name: "N", Root: "/n"})
	a, x, z := Ref{l.ID, "A"}, Ref{m.ID, "X"}, Ref{n.ID, "Z"}
	for _, r := range []Ref{a, x, z} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: r.LibraryID, RelPath: r.Path, Title: r.Path, ASIN: "one"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO issue_ignores(library_id,path,kind,created_at) VALUES(?1,'A','duplicate','t'),(?2,'X','duplicate','t')`,
		`INSERT INTO duplicate_decisions(library_id,other_library_id,paths,created_at) VALUES(?1,?2,'["A","X"]','t')`,
		`INSERT INTO server_state VALUES('map_duplicate_ignores_0092','true','t')`,
	} {
		if _, err := c.db.ExecContext(ctx, q, l.ID, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.MapLegacyDuplicateIgnores(ctx); err != nil {
		t.Fatal(err)
	}
	if page, err := c.DuplicatePage(ctx, DuplicateQuery{Across: true}); err != nil || page.Total != 3 {
		t.Fatalf("open comparisons %+v (%v), want all three", page, err)
	}
	if err := c.IgnoreIssue(ctx, IssueDuplicate, []Ref{x, a}, 0); err != nil {
		t.Fatal(err)
	}
	if page, err := c.DuplicatePage(ctx, DuplicateQuery{Across: true, Ignored: true}); err != nil || page.Total != 1 || page.Groups[0].Key != comparisonKey([]Ref{a, x}) {
		t.Fatalf("ignored comparisons %+v (%v), want A/X", page, err)
	}
}

func TestCrossLibraryPairsUseDirectEvidence(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	l, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/l"})
	m, _ := c.CreateLibrary(ctx, Library{Name: "M", Root: "/m"})
	for _, b := range []Book{
		{LibraryID: l.ID, RelPath: "A", Title: "A", ContentHash: "audio", Size: 100},
		{LibraryID: l.ID, RelPath: "B", Title: "B", ContentHash: "audio", Size: 100, ISBN: "123", Duration: 1000},
		{LibraryID: m.ID, RelPath: "C", Title: "C", ISBN: "123", Duration: 9000},
	} {
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	page, err := c.DuplicatePage(ctx, DuplicateQuery{Across: true})
	if err != nil || page.Total != 2 {
		t.Fatalf("all comparisons %+v, %v", page, err)
	}
	for _, g := range page.Groups {
		refs := make([]Ref, 0, len(g.Books))
		for _, b := range g.Books {
			refs = append(refs, Ref{b.LibraryID, b.Path})
		}
		switch comparisonKey(refs) {
		case comparisonKey([]Ref{{l.ID, "A"}, {l.ID, "B"}}):
			if g.Reason != dupSameFiles {
				t.Fatalf("A/B reason %s", g.Reason)
			}
		case comparisonKey([]Ref{{l.ID, "B"}, {m.ID, "C"}}):
			if g.Reason != dupIDLengthDiffers {
				t.Fatalf("B/C reason %s", g.Reason)
			}
		default:
			t.Fatalf("indirect comparison %+v", refs)
		}
	}
	page, err = c.DuplicatePage(ctx, DuplicateQuery{LibraryID: m.ID, Across: true})
	if err != nil || page.Total != 1 || page.Groups[0].Reason != dupIDLengthDiffers {
		t.Fatalf("library M comparisons %+v, %v", page, err)
	}
}

// A shared identifier makes a cross pair per two copies, so one more copies in a
// library share than maxCrossCopies is a placeholder: its copies group in their
// library and make no cross pairs. Below the bound every pair is offered.
func TestCrossLibraryPairsSkipAPlaceholderIdentifier(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	l, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/l"})
	m, _ := c.CreateLibrary(ctx, Library{Name: "M", Root: "/m"})
	put := func(lib int64, path, isbn string) {
		t.Helper()
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib, RelPath: path, Title: path, ISBN: isbn}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range maxCrossCopies + 1 {
		put(l.ID, fmt.Sprintf("Dummy %02d", i), "0000000000")
	}
	put(m.ID, "Dummy", "0000000000")
	put(l.ID, "Real 1", "123")
	put(l.ID, "Real 2", "123")
	put(m.ID, "Real 3", "123")
	put(m.ID, "Real 4", "123")
	page, err := c.DuplicatePage(ctx, DuplicateQuery{Across: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var placeholder, inLibrary, cross int
	for _, g := range page.Groups {
		switch {
		case g.Books[0].LibraryID != g.Books[len(g.Books)-1].LibraryID:
			cross++
			if strings.HasPrefix(g.Books[0].Path, "Dummy") {
				t.Fatalf("placeholder cross pair %s / %s", g.Books[0].Path, g.Books[1].Path)
			}
		case len(g.Books) == maxCrossCopies+1:
			placeholder++
		default:
			inLibrary++
		}
	}
	if placeholder != 1 || inLibrary != 2 || cross != 4 || page.Total != 7 {
		t.Fatalf("placeholder %d, in-library %d, cross %d, total %d; want 1, 2, 4, 7", placeholder, inLibrary, cross, page.Total)
	}
}

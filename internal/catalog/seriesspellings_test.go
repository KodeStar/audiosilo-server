package catalog

import (
	"reflect"
	"testing"
)

// TestSeriesSpellingGroups: one series spelled three ways is one Health issue in
// its library, counted per book by the predicate the list shares, offering the
// Series page's name; series alike only give or take an article are not one.
// Ignoring every book moves the group to the ignored view, and setting one
// spelling on every book settles it, keeping each book's position.
func TestSeriesSpellingGroups(t *testing.T) {
	t.Parallel()
	c, ctx, lib := seedMonsters(t)
	want := []SeriesSpellingGroup{{LibraryID: lib, Suggested: "He Who Fights with Monsters", Books: 13,
		Spellings: []SeriesSpelling{
			{Name: "He Who Fights With Monsters", Books: 6},
			{Name: "He Who Fights with Monsters", Books: 5},
			{Name: "He Who Fights WIth Monsters", Books: 2},
		}}}
	groups := func(ignored bool) []SeriesSpellingGroup {
		t.Helper()
		g, err := c.SeriesSpellingGroups(ctx, 0, ignored)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	counts := func() IssueCount {
		t.Helper()
		ic, err := c.IssueCounts(ctx, []string{IssueSeriesSpellings})
		if err != nil {
			t.Fatal(err)
		}
		return ic[0]
	}
	listed := func() []string {
		t.Helper()
		page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{Issue: IssueSeriesSpellings}, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range page.Books {
			out = append(out, b.Path)
		}
		return out
	}
	if got := groups(false); !reflect.DeepEqual(got, want) {
		t.Fatalf("groups =\n%+v\nwant\n%+v", got, want)
	}
	if got := groups(true); len(got) != 0 {
		t.Fatalf("ignored groups = %+v, want none", got)
	}
	if ic := counts(); ic.Count != 13 || ic.Ignored != 0 || len(ic.Samples) == 0 {
		t.Fatalf("count = %+v, want 13 books", ic)
	}
	if got := listed(); len(got) != 13 {
		t.Fatalf("listed %d books, want 13: %q", len(got), got)
	}
	if other, err := c.SeriesSpellingGroups(ctx, lib+1, false); err != nil || len(other) != 0 {
		t.Fatalf("another library's groups = %+v, %v", other, err)
	}

	var refs []Ref
	for _, n := range allMonsters {
		refs = append(refs, Ref{LibraryID: lib, Path: monsterPath(int(n))})
	}
	if err := c.IgnoreIssue(ctx, IssueSeriesSpellings, refs, seedUser(t, c, ctx)); err != nil {
		t.Fatal(err)
	}
	if got := groups(false); len(got) != 0 {
		t.Fatalf("open groups after ignoring = %+v", got)
	}
	if got := groups(true); !reflect.DeepEqual(got, want) {
		t.Fatalf("ignored groups = %+v", got)
	}
	if ic := counts(); ic.Count != 0 || ic.Ignored != 13 {
		t.Fatalf("count after ignoring = %+v", ic)
	}
	if err := c.UnignoreIssue(ctx, IssueSeriesSpellings, refs); err != nil {
		t.Fatal(err)
	}

	// A book listing this series only as a secondary membership keeps its main series.
	if err := c.EditBook(ctx, lib, "Disc/8", BookEdit{Set: map[string]string{FieldMoreSeries: `[{"name":"He Who Fights With Monsters","position":14}]`}}); err != nil {
		t.Fatal(err)
	}
	merged, err := c.MergeSpellings(ctx, lib, FieldSeries, []string{"He Who Fights WIth Monsters", "He Who Fights With Monsters"}, want[0].Suggested, 0)
	if err != nil || len(merged) != 8 {
		t.Fatalf("merged %d books, %v", len(merged), err)
	}
	if b := mustBook(t, c, ctx, lib, "Disc/8"); b.Series != "Discworld" {
		t.Fatalf("secondary membership changed the main series to %q", b.Series)
	}
	if got := groups(false); len(got) != 0 {
		t.Fatalf("groups after the rename = %+v", got)
	}
	if ic := counts(); ic.Count != 0 {
		t.Fatalf("count after the rename = %+v", ic)
	}
	for _, n := range allMonsters {
		if b, _ := c.GetBookByPath(ctx, lib, monsterPath(int(n))); b.SeriesIndex != n {
			t.Errorf("book %v at %v after the rename", n, b.SeriesIndex)
		}
	}
}

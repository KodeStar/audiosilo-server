package upcoming

import (
	"context"
	"fmt"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// CoverFor hands out exactly the cover List does for each work, whatever the
// scopes: the first library's when two series carry the work, none for a work
// with a copy in scope, one without a cover, or one outside the caller's
// series.
func TestCoverForMatchesList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.expanse("2011", "2026-11-12", "2026-12-01", "2027-01-05")
	for i := range f.md.series["expanse"].Members {
		if i != 2 { // e3 has no cover
			f.md.series["expanse"].Members[i].CoverURL = fmt.Sprintf("https://covers.example/e%d.jpg", i+1)
		}
	}
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	// Another library holds a series of its own carrying e2 under another
	// cover, and a copy of e4 filed under no series.
	other := f.library(t, "Other")
	f.md.series["shorts"] = &meta.SeriesRecord{ID: "shorts", Name: "Expanse Shorts", Members: []meta.SeriesMember{
		{WorkID: "s1", Position: "1", Title: "Short 1", ReleaseDate: "2012"},
		{WorkID: "e2", Position: "2", Title: "Expanse 2", ReleaseDate: "2026-11-12", CoverURL: "https://covers.example/shorts-e2.jpg"},
	}}
	f.md.workRefs["s1"] = []meta.SeriesRef{{ID: "shorts", Name: "Expanse Shorts"}}
	f.book(t, other, "Expanse Shorts", 1, "S1", "s1")
	f.loose(t, other, "loose/four", "A4", "e4")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}

	inMain := []catalog.Scope{{LibraryID: f.lib.ID, AllowAll: true}}
	inOther := []catalog.Scope{{LibraryID: other.ID, AllowAll: true}}
	both := []catalog.Scope{inMain[0], inOther[0]}
	for _, c := range []struct {
		name   string
		scopes []catalog.Scope
		want   map[string]string // the covers List hands out; every other work has none
	}{
		{"both libraries", both, map[string]string{"e2": "https://covers.example/e2.jpg"}},
		{"main only", inMain, map[string]string{"e2": "https://covers.example/e2.jpg", "e4": "https://covers.example/e4.jpg"}},
		{"other only", inOther, map[string]string{"e2": "https://covers.example/shorts-e2.jpg"}},
		{"no scopes", nil, map[string]string{}},
	} {
		ans, err := List(ctx, f.cat, c.scopes, func(string) string { return "" }, 90, f.clock)
		if err != nil {
			t.Fatal(err)
		}
		listed := map[string]string{}
		for _, it := range ans.Items {
			if it.Work.CoverURL != "" {
				listed[it.Work.ID] = it.Work.CoverURL
			}
		}
		if fmt.Sprint(listed) != fmt.Sprint(c.want) {
			t.Fatalf("%s: List's covers = %v, want %v", c.name, listed, c.want)
		}
		for _, work := range []string{"e1", "e2", "e3", "e4", "s1", "unknown"} {
			got, err := CoverFor(ctx, f.cat, c.scopes, work)
			if err != nil {
				t.Fatal(err)
			}
			if got != listed[work] {
				t.Errorf("%s: CoverFor(%s) = %q, List hands out %q", c.name, work, got, listed[work])
			}
		}
	}
}

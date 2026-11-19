package upcoming

import (
	"context"
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// matchOnly indexes a book at path with no ASIN or ISBN, in series at pos
// ("" for none), and records its console match to work.
func (f *fixture) matchOnly(t *testing.T, lib *catalog.Library, path, series string, pos float64, work string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: path, Title: path, Author: "A",
		Series: series, SeriesIndex: pos, AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.EditBook(ctx, lib.ID, path, catalog.BookEdit{
		Match: &catalog.MatchAnchor{WorkID: work}, Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
}

// A book matched in the console with no ASIN or ISBN is the work it was
// matched to (server#375): in its series it holds that work's slot, and filed
// anywhere it is a copy, so /me/upcoming never lists the work as coming.
func TestRunnerMatchOnlyBookIsOwned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.expanse("2011", "2012", "2013", "2014")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	// e2 in the series with no position: only its work can hold slot 2.
	f.matchOnly(t, f.lib, "The Expanse/Calibans War", "The Expanse", 0, "e2")
	f.book(t, f.lib, "The Expanse", 4, "A4", "e4")
	// e3 matched in the console, filed under no series in another library.
	other := f.library(t, "Other")
	f.matchOnly(t, other, "loose/three", "", 0, "e3")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	// e3 is a gap in the main library's series; e2 is held there.
	if got, want := f.items(t), []string{"e3=missing"}; !slices.Equal(got, want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	inMain := []catalog.Scope{{LibraryID: f.lib.ID, AllowAll: true}}
	both := append(slices.Clone(inMain), catalog.Scope{LibraryID: other.ID, AllowAll: true})
	listed := func(scopes []catalog.Scope) []string {
		t.Helper()
		ans, err := List(ctx, f.cat, scopes, func(id string) string { return id }, 90, f.clock)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range ans.Items {
			out = append(out, it.Work.ID)
		}
		return out
	}
	// Allowed: the caller who can open the matched copy has e3, so nothing is
	// coming; denied: one who can't still sees the gap.
	if got := listed(both); len(got) != 0 {
		t.Fatalf("with the match-only copy in scope: listed %v, want none", got)
	}
	if got := listed(inMain); !slices.Equal(got, []string{"e3"}) {
		t.Fatalf("without it: listed %v, want e3", got)
	}
	if !f.holds(t, "e2") || !f.holds(t, "e3") {
		t.Fatal("the server holds both match-only books' works")
	}
}

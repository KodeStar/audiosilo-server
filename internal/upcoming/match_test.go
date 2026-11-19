package upcoming

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
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

// A match recorded on a book already indexed (server#375) is a changed work
// like an ASIN edit: its series, resolved to nothing by the book's wrong ASIN,
// is resolved again by the match at the next run, not a month on. The match's
// own time says so, not a memory of the last run: a server started since (a
// new Runner) finds it too, and a series resolved by a match is not read again
// for it.
func TestRunnerSeriesResolvedAgainOnAMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.expanse("2011", "2012")
	f.md.workRefs["x1"] = []meta.SeriesRef{{ID: "other", Name: "Space Operas"}}
	f.book(t, f.lib, "The Expanse", 1, "A1", "x1") // a wrong ASIN
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if rows, _ := f.cat.UpcomingSeriesAll(ctx); len(rows) != 1 || rows[0].SeriesID != "" {
		t.Fatalf("series = %+v", rows)
	}
	f.clock = f.clock.Add(time.Hour)
	if err := f.cat.EditBook(ctx, f.lib.ID, "The Expanse/1", catalog.BookEdit{
		Match: &catalog.MatchAnchor{WorkID: "e1"}, Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	// The server restarted before the kicked run: nothing is remembered.
	f.r = New(f.cat, f.md, f.r.opt)
	f.clock = f.clock.Add(time.Hour)
	if err := f.r.RunOnce(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=missing"}) {
		t.Fatalf("items after the match = %v, want e2=missing", got)
	}
	resolved := func() string {
		t.Helper()
		rows, err := f.cat.UpcomingSeriesAll(ctx)
		if err != nil || len(rows) != 1 || rows[0].SeriesID != "expanse" {
			t.Fatalf("series = %+v, %v", rows, err)
		}
		return rows[0].ResolvedAt
	}
	was := resolved()
	f.clock = f.clock.Add(24 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := resolved(); got != was {
		t.Fatalf("the series was resolved again at %s (first %s) with no match since", got, was)
	}
}

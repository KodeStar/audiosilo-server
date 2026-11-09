package matchrun

import (
	"context"
	"fmt"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// TestPlanRefreshLeavesTheOfferedIdentifier (server#330 item 1): a refresh of a
// book with neither identifier never fills the ASIN or ISBN its recorded match
// offered (the admin unticked it, or reverted it since), under either scope; one
// the recording gained since (another value) still fills, and the match then
// records it as offered; a match recorded before 0070 (offered none) fills as
// before; a heal keeps the offered identifiers, or takes the proposal's.
func TestPlanRefreshLeavesTheOfferedIdentifier(t *testing.T) {
	t.Parallel()
	c := cand("the-martian", 100, "")
	p := Propose(&c, &c.Recordings[0])
	p.FoundBy = workFoundBy("the-martian", "bray")
	asin, isbn := p.Values[catalog.FieldASIN], p.Values[catalog.FieldISBN]
	if asin == "" || isbn == "" {
		t.Fatalf("proposal = %+v, want an ASIN and an ISBN", p.Values)
	}
	// Every other field the proposal offers, so only the identifiers can change.
	full := map[string]string{}
	for f, v := range p.Values {
		if f != catalog.FieldASIN && f != catalog.FieldISBN {
			full[f] = v
		}
	}
	anchored := func(offeredASIN, offeredISBN string) *catalog.MatchState {
		st := state(full, nil, false)
		st.Anchor = catalog.MatchAnchor{WorkID: "the-martian", RecordingID: "bray", How: catalog.MatchHowAccepted,
			OfferedASIN: offeredASIN, OfferedISBN: offeredISBN}
		return st
	}
	for _, scope := range ScopesFor(catalog.MatchModeRefresh) {
		// Both offered and neither on the book: nothing to write, so a refresh
		// records no item (refreshBook asks planRefresh).
		if w := planRefresh(scope, anchored(asin, isbn), p); !w.Empty() {
			t.Errorf("%s: offered identifiers reverted = %+v, want nothing", scope, w)
		}
		// Only the ASIN offered (the ISBN the recording gained since): the ISBN
		// fills, the ASIN doesn't, and the match records both as offered now.
		w := planRefresh(scope, anchored(asin, ""), p)
		if _, ok := w.Set[catalog.FieldASIN]; ok || w.Set[catalog.FieldISBN] != isbn {
			t.Errorf("%s: set = %v, want the ISBN alone", scope, w.Set)
		}
		if w.Anchor == nil || w.Anchor.WorkID != "the-martian" || w.Anchor.RecordingID != "bray" ||
			w.Anchor.OfferedASIN != asin || w.Anchor.OfferedISBN != isbn {
			t.Errorf("%s: anchor = %+v, want the match recording both as offered", scope, w.Anchor)
		}
		// Another ASIN offered than the recording has now: the new one fills.
		w = planRefresh(scope, anchored("B0OLD00001", isbn), p)
		if w.Set[catalog.FieldASIN] != asin {
			t.Errorf("%s: a gained ASIN = %v, want it filled", scope, w.Set)
		}
		if _, ok := w.Set[catalog.FieldISBN]; ok {
			t.Errorf("%s: set = %v, want the offered ISBN left out", scope, w.Set)
		}
		// A match recorded before 0070 offered none: both fill, as before.
		w = planRefresh(scope, anchored("", ""), p)
		if w.Set[catalog.FieldASIN] != asin || w.Set[catalog.FieldISBN] != isbn {
			t.Errorf("%s: pre-0070 match = %v, want both filled", scope, w.Set)
		}
		if w.Anchor == nil || w.Anchor.OfferedASIN != asin || w.Anchor.OfferedISBN != isbn {
			t.Errorf("%s: pre-0070 anchor = %+v, want what it filled recorded as offered", scope, w.Anchor)
		}
		// A book with an identifier of its own: never one written, offered or not.
		own := anchored("", "")
		own.Fields[catalog.FieldASIN] = catalog.FieldValue{Value: "B0MINE0001", Source: catalog.SourceTag}
		if w := planRefresh(scope, own, p); len(w.Set) != 0 || w.Anchor != nil {
			t.Errorf("%s: own ASIN = %+v, want nothing", scope, w)
		}
	}

	// A heal (the work merged since) keeps the offered identifiers when the
	// proposal names none (an identifier lookup's), and takes the proposal's
	// when it names any (a lookup by the work).
	st := anchored("B0OFFERED1", "")
	st.Anchor.WorkID = "old-slug"
	st.Fields[catalog.FieldASIN] = catalog.FieldValue{Value: "B0OFFERED1", Source: catalog.SourceCommunity}
	byID := p
	byID.Values = map[string]string{catalog.FieldTitle: "The Martian"}
	if w := planRefresh(ScopeFill, st, byID); w.Anchor == nil || w.Anchor.WorkID != "the-martian" ||
		w.Anchor.OfferedASIN != "B0OFFERED1" || w.Anchor.OfferedISBN != "" {
		t.Errorf("heal by identifier: anchor = %+v, want the offered ASIN kept", w.Anchor)
	}
	st.Fields[catalog.FieldASIN] = catalog.FieldValue{}
	if w := planRefresh(ScopeFill, st, p); w.Anchor == nil || w.Anchor.OfferedASIN != asin || w.Anchor.OfferedISBN != isbn {
		t.Errorf("heal by work: anchor = %+v, want the proposal's identifiers", w.Anchor)
	}

	// A match run records what its proposal offers, written or not.
	if w := Plan(ScopeFill, state(nil, nil, false), p); w.Anchor == nil ||
		w.Anchor.OfferedASIN != asin || w.Anchor.OfferedISBN != isbn {
		t.Errorf("match anchor = %+v, want the proposal's identifiers offered", w.Anchor)
	}
}

// TestRefreshLeavesARevertedIdentifier (server#330 item 1), end to end: a match
// run applies a store listing with its ASIN; the admin reverts the ASIN; a
// refresh offers nothing for the book (no item), and an apply writes none. A
// match accepted with the ASIN unticked likewise.
func TestRefreshLeavesARevertedIdentifier(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Reverted", "The Martian", "", nil)
	e.book(t, "A/Unticked", "The Martian", "", nil)
	hit := cand("the-martian", 100, "")
	hit.RecordingID = "bray"
	hit.Recordings[0].ISBNs = nil
	e.matcher.byTitle["The Martian"] = []meta.MatchCandidate{hit}
	run := e.start(t, catalog.MatchModeMatch, "uk")
	if run.Counts.Auto != 2 {
		t.Fatalf("match run = %+v, want both confident", run)
	}
	// Unticked: matched by hand from the review with the ASIN offered, not taken.
	if err := e.cat.EditBook(ctx, e.lib, "A/Unticked", catalog.BookEdit{
		Set:    map[string]string{catalog.FieldNarrator: "R. C. Bray"},
		Match:  &catalog.MatchAnchor{WorkID: "the-martian", RecordingID: "bray", OfferedASIN: "B0UK000001"},
		Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill, UserID: e.admin}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	if a := e.field(t, "A/Reverted", catalog.FieldASIN); a.Value != "B0UK000001" {
		t.Fatalf("applied asin = %+v", a)
	}
	if a := e.anchorOf(t, "A/Reverted"); a.WorkID != "the-martian" || a.OfferedASIN != "B0UK000001" {
		t.Fatalf("applied anchor = %+v, want the ASIN recorded as offered", a)
	}
	if a := e.field(t, "A/Unticked", catalog.FieldASIN); a.Value != "" {
		t.Fatalf("unticked asin = %+v, want none", a)
	}
	if err := e.cat.EditBook(ctx, e.lib, "A/Reverted", catalog.BookEdit{Revert: []string{catalog.FieldASIN}}); err != nil {
		t.Fatal(err)
	}
	if a := e.field(t, "A/Reverted", catalog.FieldASIN); a.Value != "" {
		t.Fatalf("reverted asin = %+v", a)
	}

	// Every detail the work offers is on the books now (fill wrote them, or
	// the hand match below adds them), so only the ASIN could change.
	if err := e.cat.EditBook(ctx, e.lib, "A/Unticked", catalog.BookEdit{
		Set: map[string]string{catalog.FieldTitle: "The Martian", catalog.FieldDescription: "Stranded.",
			catalog.FieldPublished: "2011", catalog.FieldSeries: "Mars", catalog.FieldSeriesIndex: "1"},
		Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	e.matcher.byWork["the-martian"] = []meta.MatchCandidate{func() meta.MatchCandidate {
		c := workAnswer("the-martian", "bray")
		c.Recordings[0].ISBNs = nil
		return c
	}()}
	refresh := e.start(t, catalog.MatchModeRefresh, "uk")
	if refresh.Total != 2 || refresh.Counts.Auto+refresh.Counts.Review != 0 {
		t.Fatalf("refresh = %+v, want both looked up and nothing to offer", refresh)
	}
	for _, path := range []string{"A/Reverted", "A/Unticked"} {
		if a := e.field(t, path, catalog.FieldASIN); a.Value != "" {
			t.Errorf("%s asin = %+v, want it left out", path, a)
		}
	}
}

// TestRefreshApplyLeavesOutNoOps (server#332): a refresh apply with no
// exclusions over items with nothing to change under the scope writes only the
// ones with changes; the others are left out as an exclusion leaves them (still
// pending, not skipped), and apply_total ends at the books changed.
func TestRefreshApplyLeavesOutNoOps(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	answer := func(description string) []meta.MatchCandidate {
		return []meta.MatchCandidate{{WorkID: "w", Title: "Another Title", Score: 100,
			Authors: []meta.MetaPersonRef{{Name: "Andy Weir"}}, Description: description}}
	}
	for i := range 5 {
		asin := fmt.Sprintf("B0REFRESH%d", i)
		e.book(t, fmt.Sprintf("A/Book%d", i), "Book", asin, nil)
		// Three offer only another title: overwrite would change the book,
		// fill nothing. Two offer a description fill takes.
		e.matcher.byASIN[asin] = answer("")
		if i >= 3 {
			e.matcher.byASIN[asin] = answer("A description.")
		}
	}
	run := e.start(t, catalog.MatchModeRefresh, "")
	if run.Counts.Auto != 5 || run.Counts.Pending != 5 {
		t.Fatalf("refresh = %+v, want five confident items", run)
	}
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill, UserID: e.admin}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	got, err := e.cat.GetMatchRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != catalog.MatchApplied || got.ApplyTotal != 2 || got.ApplyDone != 2 ||
		got.Counts.Applied != 2 || got.Counts.Skipped != 0 || got.Counts.Pending != 3 {
		t.Fatalf("run after apply = %+v, want 2 of 2 applied, 3 left pending, none skipped", got)
	}
	for _, it := range e.items(t, got, "") {
		want := ""
		if it.Path >= "A/Book3" {
			want = catalog.ItemApplied
		}
		if it.Applied != want || it.Detail != "" {
			t.Errorf("%s = %q %q, want %q", it.Path, it.Applied, it.Detail, want)
		}
		if d := e.field(t, it.Path, catalog.FieldDescription).Value; (want != "") != (d != "") {
			t.Errorf("%s description = %q", it.Path, d)
		}
	}
}

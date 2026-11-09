package matchrun

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// anotherTitle is a refresh's answer offering another title (only overwrite
// takes it) and, when not empty, a description (fill takes it too).
func anotherTitle(description string) []meta.MatchCandidate {
	return []meta.MatchCandidate{{WorkID: "w", Title: "Another Title", Score: 100,
		Authors: []meta.MetaPersonRef{{Name: "Andy Weir"}}, Description: description}}
}

// applyAndWait applies run under scope and waits for it, returning the run as
// the apply started and as it ended.
func (e *env) applyAndWait(t *testing.T, run *catalog.MatchRun, scope string) (started, ended *catalog.MatchRun) {
	t.Helper()
	ctx := context.Background()
	started, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: scope, UserID: e.admin})
	if err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	if ended, err = e.cat.GetMatchRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	return started, ended
}

// TestRefreshApplyLeavesOutUnchangedUnread (server#332): a refresh's Done
// under a scope its items had nothing to change under (an empty refresh: every
// item only overwrite would change) takes none of them: the apply starts at a
// total of 0, never reading a book or writing a row per item, so the run is
// applying (Unlink refused) for no time at all, whatever the library's size.
func TestRefreshApplyLeavesOutUnchangedUnread(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const n = 3000
	for i := range n {
		asin := fmt.Sprintf("B0N%07d", i)
		e.book(t, fmt.Sprintf("A/Book%04d", i), "Book", asin, nil)
		e.matcher.byASIN[asin] = anotherTitle("")
	}
	run := e.start(t, catalog.MatchModeRefresh, "")
	if run.Counts.Pending != n {
		t.Fatalf("refresh = %+v, want %d pending", run, n)
	}
	begun := time.Now()
	started, ended := e.applyAndWait(t, run, ScopeFill)
	t.Logf("an empty refresh's Done over %d items took %v", n, time.Since(begun))
	if started.ApplyTotal != 0 {
		t.Fatalf("apply started at a total of %d, want 0 (no item has a change under fill)", started.ApplyTotal)
	}
	if ended.Status != catalog.MatchApplied || ended.ApplyTotal != 0 || ended.ApplyDone != 0 ||
		ended.Counts.Pending != n || ended.Counts.Skipped != 0 {
		t.Fatalf("run after apply = %+v, want applied, every item still pending", ended)
	}
}

// TestRefreshApplyWritesOnlyWhatTheReviewShowed (server#332): a refresh item
// the review showed no change for under the scope is left out, even when the
// book changed since so that planning it now would write something the admin
// never saw. One it showed a change for under the scope applies beside it.
func TestRefreshApplyWritesOnlyWhatTheReviewShowed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Unchanged", "Book", "B0REVIEW01", nil)
	e.book(t, "A/Changed", "Book", "B0REVIEW02", nil)
	e.matcher.byASIN["B0REVIEW01"] = anotherTitle("A description.")
	e.matcher.byASIN["B0REVIEW02"] = anotherTitle("A description.")
	described := func(edit catalog.BookEdit) {
		t.Helper()
		if err := e.cat.EditBook(ctx, e.lib, "A/Unchanged", edit); err != nil {
			t.Fatal(err)
		}
	}
	// Fill has nothing to take; overwrite would change the title and description.
	described(catalog.BookEdit{Set: map[string]string{catalog.FieldDescription: "Old."}, Source: catalog.SourceCommunity})
	run := e.start(t, catalog.MatchModeRefresh, "")
	for _, it := range e.items(t, run, "") {
		if fill := len(it.Changes[ScopeFill].Fields) > 0; fill != (it.Path == "A/Changed") || len(it.Changes[ScopeOverwrite].Fields) == 0 {
			t.Fatalf("%s changes = %+v", it.Path, it.Changes)
		}
	}
	described(catalog.BookEdit{Revert: []string{catalog.FieldDescription}}) // since the review: fill would write it now
	started, ended := e.applyAndWait(t, run, ScopeFill)
	if d := e.field(t, "A/Unchanged", catalog.FieldDescription); d.Value != "" {
		t.Fatalf("description = %+v, want nothing the review didn't show written", d)
	}
	if d := e.field(t, "A/Changed", catalog.FieldDescription); d.Value != "A description." {
		t.Fatalf("changed description = %+v, want it filled", d)
	}
	if started.ApplyTotal != 1 || ended.Status != catalog.MatchApplied || ended.ApplyDone != 1 ||
		ended.Counts.Applied != 1 || ended.Counts.Pending != 1 {
		t.Fatalf("run = %+v, want one applied, the other left out (still pending)", ended)
	}
}

// TestRefreshApplyDropsItemsUnchangedSince (server#332): items that had a
// change under the scope when recorded but have none now (the books took it
// since) are left out as the apply passes them, dropped from its total in
// batches, and the apply still ends at its total.
func TestRefreshApplyDropsItemsUnchangedSince(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	const n, changed = 250, 5
	for i := range n + changed {
		asin := fmt.Sprintf("B0S%07d", i)
		e.book(t, fmt.Sprintf("A/Book%04d", i), "Book", asin, nil)
		e.matcher.byASIN[asin] = anotherTitle("A description.")
	}
	run := e.start(t, catalog.MatchModeRefresh, "")
	if run.Counts.Pending != n+changed {
		t.Fatalf("refresh = %+v", run)
	}
	for i := range n {
		if err := e.cat.EditBook(ctx, e.lib, fmt.Sprintf("A/Book%04d", i), catalog.BookEdit{
			Set: map[string]string{catalog.FieldDescription: "Taken since."}, Source: catalog.SourceCommunity,
		}); err != nil {
			t.Fatal(err)
		}
	}
	started, ended := e.applyAndWait(t, run, ScopeFill)
	if started.ApplyTotal != n+changed {
		t.Fatalf("started = %+v, want every item taken", started)
	}
	if ended.Status != catalog.MatchApplied || ended.ApplyTotal != changed || ended.ApplyDone != changed ||
		ended.Counts.Applied != changed || ended.Counts.Pending != n || ended.Counts.Skipped != 0 {
		t.Fatalf("run after apply = %+v, want %d of %d applied, %d left pending", ended, changed, changed, n)
	}
}

// TestRepickRecordsTheNewASINAsOffered (server#330): a repick of a book with a
// recorded match records the ASIN it writes as offered (how the book was
// matched kept), so once the admin reverts it a refresh never fills it back.
func TestRepickRecordsTheNewASINAsOffered(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	const path = "A/Community"
	e.book(t, path, "The Martian", "", nil)
	if err := e.cat.EditBook(ctx, e.lib, path, catalog.BookEdit{
		Set:    map[string]string{catalog.FieldASIN: "B0US000001"},
		Match:  &catalog.MatchAnchor{WorkID: "the-martian", RecordingID: "bray", OfferedASIN: "B0US000001"},
		Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	hit := cand("the-martian", 100, "")
	hit.RecordingID = "bray"
	e.matcher.byASIN["B0US000001"] = []meta.MatchCandidate{hit}
	run := e.start(t, catalog.MatchModeRepick, "uk")
	if run.Counts.Auto != 1 {
		t.Fatalf("repick = %+v", run)
	}
	e.applyAndWait(t, run, ScopeIDs)
	if a := e.field(t, path, catalog.FieldASIN); a.Value != "B0UK000001" {
		t.Fatalf("repicked asin = %+v", a)
	}
	if a := e.anchorOf(t, path); a.WorkID != "the-martian" || a.RecordingID != "bray" ||
		a.How != catalog.MatchHowAccepted || a.OfferedASIN != "B0UK000001" {
		t.Fatalf("anchor after repick = %+v, want the new ASIN offered, the match kept", a)
	}

	if err := e.cat.EditBook(ctx, e.lib, path, catalog.BookEdit{Revert: []string{catalog.FieldASIN}}); err != nil {
		t.Fatal(err)
	}
	e.matcher.byWork["the-martian"] = []meta.MatchCandidate{func() meta.MatchCandidate {
		c := workAnswer("the-martian", "bray")
		c.Recordings[0].ISBNs = nil
		return c
	}()}
	refresh := e.start(t, catalog.MatchModeRefresh, "uk")
	if refresh.Counts.Pending > 0 {
		e.applyAndWait(t, refresh, ScopeOverwrite)
	}
	if a := e.field(t, path, catalog.FieldASIN); a.Value != "" {
		t.Fatalf("asin after refresh = %+v, want the reverted repick left out", a)
	}
}

// TestRefreshHealLeavesAHandMatchMadeMeanwhile: a refresh's identifier fill
// records its match only while the book's match is still the one the apply
// read: one made by hand between the read and the write stays.
func TestRefreshHealLeavesAHandMatchMadeMeanwhile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	const path = "A/Listing"
	e.book(t, path, "The Martian", "", nil)
	e.anchor(t, path, "the-martian", "bray")
	e.matcher.byWork["the-martian"] = []meta.MatchCandidate{func() meta.MatchCandidate {
		c := workAnswer("the-martian", "bray")
		c.Recordings[0].ISBNs = nil
		return c
	}()}
	run := e.start(t, catalog.MatchModeRefresh, "uk")
	if run.Counts.Pending != 1 {
		t.Fatalf("refresh = %+v", run)
	}
	e.runner.beforeEdit = func(*catalog.MatchRunItem) {
		if err := e.cat.EditBook(ctx, e.lib, path, catalog.BookEdit{
			Match: &catalog.MatchAnchor{WorkID: "hand-work", RecordingID: "hand-rec"}, Source: catalog.SourceCommunity,
		}); err != nil {
			t.Error(err)
		}
	}
	e.applyAndWait(t, run, ScopeFill)
	if a := e.anchorOf(t, path); a.WorkID != "hand-work" || a.RecordingID != "hand-rec" {
		t.Fatalf("anchor = %+v, want the hand match kept", a)
	}
}

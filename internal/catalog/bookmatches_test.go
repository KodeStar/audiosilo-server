package catalog

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// anchorBook records a community match with no ASIN or ISBN (a store listing) on
// the book at path, as the match dialog's accept does.
func anchorBook(t *testing.T, c *Catalog, ctx context.Context, libID int64, path, work string) {
	t.Helper()
	if err := c.EditBook(ctx, libID, path, BookEdit{
		Set:    map[string]string{FieldTitle: "Community Title"},
		Match:  &MatchAnchor{WorkID: work, RecordingID: "rec-1"},
		Source: SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
}

func anchorOf(t *testing.T, c *Catalog, ctx context.Context, libID int64, path string) MatchAnchor {
	t.Helper()
	st, err := c.BookMatchState(ctx, libID, path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Anchor
}

// TestAnchorWrittenWithTheEdit: a community edit records the match in its own
// transaction (accepted by default, the run and admin kept), an edit with the
// anchor alone works, and an empty work id removes it. An admin's own edit can't
// record one, a bulk edit refuses one, and an id with control characters or
// over the bound is refused, each changing nothing.
func TestAnchorWrittenWithTheEdit(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	uid := seedUser(t, c, ctx)
	const p = "A/Book"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{
		Set: map[string]string{FieldTitle: "T"}, Match: &MatchAnchor{WorkID: " the-work ", RecordingID: "rec"},
		Source: SourceCommunity, UserID: uid,
	}); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a.WorkID != "the-work" || a.RecordingID != "rec" || a.How != MatchHowAccepted {
		t.Fatalf("anchor = %+v", a)
	}
	var setBy, runID any
	if err := c.db.QueryRowContext(ctx, `SELECT set_by, run_id FROM book_matches WHERE library_id = ? AND path = ?`,
		lib.ID, p).Scan(&setBy, &runID); err != nil || setBy != uid || runID != nil {
		t.Fatalf("set_by %v run_id %v %v", setBy, runID, err)
	}
	// The anchor alone: another work, made by a run.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Match: &MatchAnchor{WorkID: "other", How: MatchHowAuto, RunID: 7}, Source: SourceCommunity}); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a.WorkID != "other" || a.RecordingID != "" || a.How != MatchHowAuto {
		t.Fatalf("anchor after the second edit = %+v", a)
	}

	for name, edit := range map[string]BookEdit{
		"an admin's own edit": {Match: &MatchAnchor{WorkID: "mine"}},
		"a bad how":           {Match: &MatchAnchor{WorkID: "mine", How: "guessed"}, Source: SourceCommunity},
		"a control character": {Match: &MatchAnchor{WorkID: "mi\nne"}, Source: SourceCommunity},
		"a C1 control":        {Match: &MatchAnchor{WorkID: "mi\u0085ne"}, Source: SourceCommunity},
		"too long":            {Match: &MatchAnchor{WorkID: strings.Repeat("w", maxMatchID+1)}, Source: SourceCommunity},
		"a bad recording":     {Match: &MatchAnchor{WorkID: "mine", RecordingID: "r\x00"}, Source: SourceCommunity},
	} {
		if err := c.EditBook(ctx, lib.ID, p, edit); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("%s: err = %v, want ErrInvalidOverride", name, err)
		}
	}
	if err := c.EditBooks(ctx, []Ref{{lib.ID, p}}, BookEdit{Match: &MatchAnchor{WorkID: "bulk"}, Source: SourceCommunity}); !errors.Is(err, ErrInvalidOverride) {
		t.Errorf("bulk: err = %v, want ErrInvalidOverride", err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a.WorkID != "other" {
		t.Fatalf("a refused edit changed the anchor: %+v", a)
	}

	// An empty work id removes it, from any edit.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Match: &MatchAnchor{}}); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a != (MatchAnchor{}) {
		t.Fatalf("anchor after the clear = %+v, want none", a)
	}
}

// TestAnchorSurvivesRescanRebuildMoveAndJoin: the recorded match is path-keyed
// state: a rescan and a rebuild keep it, a move carries it (replacing a stale one
// at the destination), and a join copies the first part's onto the joined book.
func TestAnchorSurvivesRescanRebuildMoveAndJoin(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "old/Book"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	anchorBook(t, c, ctx, lib.ID, p, "the-work")
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a.WorkID != "the-work" {
		t.Fatalf("after rescan: %+v", a)
	}
	if _, err := c.DeleteBooksNotIn(ctx, lib.ID, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a.WorkID != "the-work" {
		t.Fatalf("after rebuild: %+v", a)
	}

	// A stale anchor at the destination (an earlier book there) gives way.
	if _, err := c.db.ExecContext(ctx, `INSERT INTO book_matches(library_id, path, work_id, how, set_at)
		VALUES(?, 'new/Book', 'stale', 'auto', 't')`, lib.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.MoveDurableState(ctx, lib.ID, p, "new/Book"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "new/Book")); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, "new/Book"); a.WorkID != "the-work" {
		t.Fatalf("after move: %+v", a)
	}
	var left int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM book_matches WHERE path = ?`, p).Scan(&left); err != nil || left != 0 {
		t.Fatalf("anchors left at the old path = %d %v", left, err)
	}

	// A join: the first part's anchor is the joined book's.
	for _, part := range []string{"Joined/CD1", "Joined/CD2"} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, part)); err != nil {
			t.Fatal(err)
		}
		anchorBook(t, c, ctx, lib.ID, part, "work-of-"+part[len("Joined/"):])
	}
	if err := c.JoinDurableState(ctx, lib.ID, "Joined", []JoinPart{{Path: "Joined/CD1"}, {Path: "Joined/CD2", Offset: 1800, Last: true}}, 3600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "Joined")); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, "Joined"); a.WorkID != "work-of-CD1" {
		t.Fatalf("after join: %+v, want the first part's", a)
	}
}

// TestAnchorIsMatched: a book matched with no ASIN or ISBN counts as matched
// everywhere "matched" is read (the list's flag, the facet, Health's Not matched,
// the Activity coverage, the series' naming), and a run's lists: a match run
// leaves it out, a refresh takes it with its work. A book with nothing stays
// unmatched; ClearCommunityMatches takes the anchor and the book is unmatched
// again.
func TestAnchorIsMatched(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, p := range []string{"A/Anchored", "A/Plain", "A/Plain2"} {
		b := scannedBook(lib.ID, p)
		b.Series = map[string]string{"A/Anchored": "Mars", "A/Plain": "MARS", "A/Plain2": "MARS"}[p]
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EditBook(ctx, lib.ID, "A/Anchored", BookEdit{Match: &MatchAnchor{WorkID: "the-work", RecordingID: "rec"}, Source: SourceCommunity}); err != nil {
		t.Fatal(err)
	}

	matched := func() map[string]bool {
		t.Helper()
		page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{LibraryID: lib.ID}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, b := range page.Books {
			out[b.Path] = b.Matched
		}
		return out
	}
	if got := matched(); !got["A/Anchored"] || got["A/Plain"] {
		t.Errorf("list matched = %v", got)
	}
	yes := true
	page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{LibraryID: lib.ID, Matched: &yes}, Limit: 10})
	if err != nil || len(page.Books) != 1 || page.Books[0].Path != "A/Anchored" {
		t.Errorf("matched=1 = %+v %v", page, err)
	}
	facets, err := c.BookFacets(ctx, BookFilter{LibraryID: lib.ID})
	if err != nil || facets.Matched.Yes != 1 || facets.Matched.No != 2 {
		t.Errorf("facet = %+v %v", facets.Matched, err)
	}
	unmatchedCount := func() int {
		t.Helper()
		counts, err := c.IssueCounts(ctx, []string{IssueUnmatched})
		if err != nil {
			t.Fatal(err)
		}
		return counts[0].Count
	}
	if n := unmatchedCount(); n != 2 {
		t.Errorf("Not matched = %d, want 2", n)
	}
	act, err := c.ActivityFor(ctx, "7d", time.Now().Add(-time.Hour), time.Now(), time.UTC)
	if err != nil || act.Coverage.Identified != 1 {
		t.Errorf("coverage = %+v %v", act.Coverage, err)
	}
	series, err := c.Series(ctx, lib.ID, nil, false)
	if err != nil || len(series) != 1 || series[0].Name != "Mars" {
		t.Errorf("series = %+v %v, want named by the matched book's spelling", series, err)
	}
	paths := func(books []Book, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, b := range books {
			out = append(out, b.RelPath+"="+b.MatchWorkID+"/"+b.MatchRecordingID)
		}
		return out
	}
	if got := paths(c.UnmatchedBooks(ctx, lib.ID)); !slices.Equal(got, []string{"A/Plain=/", "A/Plain2=/"}) {
		t.Errorf("UnmatchedBooks = %v", got)
	}
	if got := paths(c.MatchedBooks(ctx, lib.ID)); !slices.Equal(got, []string{"A/Anchored=the-work/rec"}) {
		t.Errorf("MatchedBooks = %v", got)
	}
	d, err := c.AdminBookDetail(ctx, lib.ID, "A/Anchored")
	if err != nil || d.CommunityMatch == nil || d.CommunityMatch.WorkID != "the-work" || d.CommunityMatch.How != MatchHowAccepted || d.CommunityMatch.MatchedAt == "" {
		t.Errorf("detail community match = %+v %v", d, err)
	}
	if d, err := c.AdminBookDetail(ctx, lib.ID, "A/Plain"); err != nil || d.CommunityMatch != nil {
		t.Errorf("unmatched detail community match = %+v %v, want none", d.CommunityMatch, err)
	}

	got, err := c.ClearCommunityMatches(ctx, lib.ID)
	if err != nil || *got != (ClearedMatches{Books: 1}) {
		t.Fatalf("clear = %+v %v, want the anchored book counted", got, err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, "A/Anchored"); a.WorkID != "" {
		t.Errorf("anchor after clear = %+v", a)
	}
	if n := unmatchedCount(); n != 3 {
		t.Errorf("Not matched after clear = %d, want 3", n)
	}
}

// TestClearRebuildsAnAnchoredBook: a book with both an anchor and community
// overrides is rebuilt by a clear (the anchor's row doesn't stand in for the
// overrides' rebuild) and counted once.
func TestClearRebuildsAnAnchoredBook(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "A/Book")); err != nil {
		t.Fatal(err)
	}
	anchorBook(t, c, ctx, lib.ID, "A/Book", "the-work")
	got, err := c.ClearCommunityMatches(ctx, lib.ID)
	if err != nil || *got != (ClearedMatches{Books: 1}) {
		t.Fatalf("clear = %+v %v", got, err)
	}
	if b := mustBook(t, c, ctx, lib.ID, "A/Book"); b.Title != "Original Name" {
		t.Fatalf("title = %q, want the scan's back", b.Title)
	}
}

// TestMatchedExprIsAnIndexSeek: the anchor's EXISTS reads book_matches by its
// primary key per book, never a walk of the table.
func TestMatchedExprIsAnIndexSeek(t *testing.T) {
	t.Parallel()
	c, _ := newTestCatalog(t)
	plan := strings.Join(queryPlan(t, c, `SELECT COUNT(*) FROM books b WHERE `+issuePredicates[IssueUnmatched], nil), "\n")
	if !strings.Contains(plan, "SEARCH m USING") || strings.Contains(plan, "SCAN m") {
		t.Errorf("book_matches is not read by its key:\n%s", plan)
	}
}

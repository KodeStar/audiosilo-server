package catalog

import (
	"context"
	"errors"
	"maps"
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
		Match:  &MatchAnchor{WorkID: work, RecordingID: "rec-1", OfferedASIN: "B0OFFERED1"},
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
// anchor alone works, and an empty work id removes it (and the book's community
// values: TestUnlinkClearsTheBooksCommunityMatch). An admin's own edit can't
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
		"whitespace only":     {Match: &MatchAnchor{WorkID: " \t\n"}},
		"unicode whitespace":  {Match: &MatchAnchor{WorkID: "\u2003"}},
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

	// An empty work id removes it, from any edit (TestUnlinkClearsTheBooksCommunityMatch).
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Match: &MatchAnchor{}}); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a != (MatchAnchor{}) {
		t.Fatalf("anchor after the clear = %+v, want none", a)
	}
}

// Unlink checks run status in its writer transaction, so a run cannot move to
// applying between that check and the clear. Only a run applying to the book's
// library (or every library) refuses it: one that only matches, one over
// another library and one that is ready, applied or interrupted don't. A refused
// mixed edit writes none of its fields; an ordinary edit is still allowed, and
// settling the runs (InterruptMatchRuns at startup) releases a refusing one.
func TestUnlinkWhileMatchRunWorks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  string
		library string // "same", "other", "all"
		refused bool
	}{
		{"matching, same library", MatchMatching, "same", false},
		{"matching, all libraries", MatchMatching, "all", false},
		{"applying, same library", MatchApplying, "same", true},
		{"applying, other library", MatchApplying, "other", false},
		{"applying, all libraries", MatchApplying, "all", true},
		{"ready", MatchReady, "same", false},
		{"applied", MatchApplied, "all", false},
		{"interrupted", MatchInterrupted, "same", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, ctx := newTestCatalog(t)
			lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			other, err := c.CreateLibrary(ctx, Library{Name: "Other", Root: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			const path = "A/Book"
			if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, path)); err != nil {
				t.Fatal(err)
			}
			anchorBook(t, c, ctx, lib.ID, path, "the-work")
			run := MatchRun{Mode: MatchModeRefresh}
			switch tc.library {
			case "same":
				run.LibraryID = &lib.ID
			case "other":
				run.LibraryID = &other.ID
			}
			id, err := c.StartMatchRun(ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			if tc.status != MatchMatching {
				if err := c.FinishMatching(ctx, id, MatchReady, ""); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.status {
			case MatchApplying, MatchApplied:
				if err := c.BeginApply(ctx, id, "fill", 0); err != nil {
					t.Fatal(err)
				}
				if tc.status == MatchApplied {
					if err := c.FinishApply(ctx, id, false); err != nil {
						t.Fatal(err)
					}
				}
			case MatchInterrupted:
				if _, err := c.db.ExecContext(ctx, `UPDATE match_runs SET status = ? WHERE id = ?`, MatchInterrupted, id); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.refused {
				if err := c.EditBook(ctx, lib.ID, path, BookEdit{Match: &MatchAnchor{}}); err != nil {
					t.Fatalf("unlink = %v, want allowed", err)
				}
				if a := anchorOf(t, c, ctx, lib.ID, path); a.WorkID != "" {
					t.Fatalf("unlink left the anchor: %+v", a)
				}
				return
			}
			edit := BookEdit{Match: &MatchAnchor{}, Set: map[string]string{FieldTitle: "Must not be written"}}
			if err := c.EditBook(ctx, lib.ID, path, edit); !errors.Is(err, ErrMatchRunBusy) {
				t.Fatalf("unlink while a run applies = %v, want ErrMatchRunBusy", err)
			}
			if a := anchorOf(t, c, ctx, lib.ID, path); a.WorkID != "the-work" {
				t.Fatalf("refused unlink changed the anchor: %+v", a)
			}
			if b := mustBook(t, c, ctx, lib.ID, path); b.Title != "Community Title" {
				t.Fatalf("refused unlink wrote a field: %q", b.Title)
			}
			if err := c.EditBook(ctx, lib.ID, path, BookEdit{Set: map[string]string{FieldNarrator: "My Reader"}}); err != nil {
				t.Fatalf("ordinary edit while a run works: %v", err)
			}
			if err := c.InterruptMatchRuns(ctx); err != nil {
				t.Fatal(err)
			}
			if err := c.EditBook(ctx, lib.ID, path, BookEdit{Match: &MatchAnchor{}}); err != nil {
				t.Fatalf("unlink after recovery: %v", err)
			}
			if a := anchorOf(t, c, ctx, lib.ID, path); a.WorkID != "" {
				t.Fatalf("unlink after recovery left the anchor: %+v", a)
			}
		})
	}
}

// SettleMatchRuns settles every run marked working but the one kept, in one
// transaction (matching -> interrupted, or ready once it recorded every book:
// server#350; applying -> ready), counting them; SettleMatchRun settles one,
// only while it is marked working.
func TestSettleMatchRuns(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	start := func(apply bool) int64 {
		id, err := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, Total: 2})
		if err != nil {
			t.Fatal(err)
		}
		if apply {
			if err := c.FinishMatching(ctx, id, MatchReady, ""); err != nil {
				t.Fatal(err)
			}
			if err := c.BeginApply(ctx, id, "fill", 0); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	status := func(id int64) string {
		r, err := c.GetMatchRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r.Status
	}
	matching, complete, applying, kept := start(false), start(false), start(true), start(true)
	// One book of two recorded; the other run recorded both (an item and a
	// book with nothing worth keeping), so only its finishing write was lost.
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	item := func() *MatchRunItem {
		return &MatchRunItem{LibraryID: lib.ID, Path: "A/Book", Outcome: OutcomeReview, Proposal: MatchProposal{WorkID: "w"}}
	}
	for _, rec := range []struct {
		run  int64
		item *MatchRunItem
	}{{matching, item()}, {complete, item()}, {complete, nil}} {
		if err := c.RecordMatchItem(ctx, rec.run, rec.item); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := c.SettleMatchRuns(ctx, kept); n != 3 || err != nil {
		t.Fatalf("settle = %d %v, want 3", n, err)
	}
	if status(matching) != MatchInterrupted || status(complete) != MatchReady || status(applying) != MatchReady ||
		status(kept) != MatchApplying {
		t.Fatalf("statuses = %s %s %s %s", status(matching), status(complete), status(applying), status(kept))
	}
	if r, _ := c.GetMatchRun(ctx, complete); r.FinishedAt == nil || r.Counts.Review != 1 {
		t.Fatalf("a complete run settled = %+v, want ready with its review and a finished_at", r)
	}
	if r, _ := c.GetMatchRun(ctx, matching); r.FinishedAt == nil {
		t.Fatal("an interrupted run has no finished_at")
	}
	if n, err := c.SettleMatchRuns(ctx, kept); n != 0 || err != nil {
		t.Fatalf("settle again = %d %v, want nothing", n, err)
	}
	if ok, err := c.SettleMatchRun(ctx, applying); ok || err != nil {
		t.Fatalf("settle a ready run = %v %v, want false", ok, err)
	}
	if ok, err := c.SettleMatchRun(ctx, kept); !ok || err != nil || status(kept) != MatchReady {
		t.Fatalf("settle one = %v %v, status %s", ok, err, status(kept))
	}
	if ok, err := c.SettleMatchRun(ctx, 999); ok || err != nil {
		t.Fatalf("settle an unknown run = %v %v", ok, err)
	}
}

// normalizeAnchor trims invisible characters from both ends of the ids: a work
// id of them alone is refused (never an Unlink), a recording id of them alone
// is none, interior ones and non-ASCII ids are kept.
func TestNormalizeAnchorInvisible(t *testing.T) {
	t.Parallel()
	for _, in := range []string{" ", "\u200b", "\ufeff", "\u2060", "\u3164", "\u115f", "\u1160", "\uffa0",
		"\u2800", "\ufe0f", " \u200b\t\u2060 ", "\u00ad"} {
		if _, err := normalizeAnchor(&MatchAnchor{WorkID: in}, SourceCommunity); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("work id %q: err = %v, want refused", in, err)
		}
	}
	for in, want := range map[string]string{
		"\u200bdune":        "dune",
		"\ufeffdune\u2060 ": "dune",
		"münchhausen-é":     "münchhausen-é",
		"du\u200bne":        "du\u200bne",
	} {
		a, err := normalizeAnchor(&MatchAnchor{WorkID: in, RecordingID: "\u200b\u3164"}, SourceCommunity)
		if err != nil || a.WorkID != want || a.RecordingID != "" {
			t.Errorf("work id %q = %+v %v, want %q and no recording", in, a, err, want)
		}
	}
	if a, err := normalizeAnchor(&MatchAnchor{WorkID: ""}, ""); err != nil || a.WorkID != "" {
		t.Errorf("explicit unlink = %+v %v", a, err)
	}
}

// TestAnchorOfferedIdentifiers (server#330): the identifiers a match offered
// are stored with it in the ASIN and ISBN fields' form, replaced by the next
// match recorded (none when it offers none), refused when the field would
// refuse them (on field "match", changing nothing), and ignored by an Unlink.
func TestAnchorOfferedIdentifiers(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Book"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{
		Match:  &MatchAnchor{WorkID: "the-work", OfferedASIN: " b0offered1 ", OfferedISBN: "978-0-553-41802-6"},
		Source: SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a.OfferedASIN != "B0OFFERED1" || a.OfferedISBN != "9780553418026" {
		t.Fatalf("anchor = %+v, want the offered identifiers in stored form", a)
	}
	if b, err := c.GetBookByPath(ctx, lib.ID, p); err != nil || b.ASIN != "" || b.ISBN != "" {
		t.Fatalf("book = %+v %v, want no identifier written: offered is not set", b, err)
	}
	for name, a := range map[string]MatchAnchor{
		"a bad ASIN": {WorkID: "other", OfferedASIN: "not-an-asin"},
		"a bad ISBN": {WorkID: "other", OfferedISBN: "12345"},
	} {
		err := c.EditBook(ctx, lib.ID, p, BookEdit{Match: &a, Source: SourceCommunity})
		var oe *OverrideError
		if !errors.As(err, &oe) || oe.Field != "match" {
			t.Errorf("%s: err = %v, want refused on match", name, err)
		}
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a.WorkID != "the-work" || a.OfferedASIN != "B0OFFERED1" {
		t.Fatalf("a refused edit changed the anchor: %+v", a)
	}
	// Another match offering none replaces them.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Match: &MatchAnchor{WorkID: "other"}, Source: SourceCommunity}); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a.WorkID != "other" || a.OfferedASIN != "" || a.OfferedISBN != "" {
		t.Fatalf("anchor = %+v, want the new match's (none offered)", a)
	}
	// An Unlink ignores them, even ones the field would refuse.
	if a, err := normalizeAnchor(&MatchAnchor{OfferedASIN: "not-an-asin"}, ""); err != nil || *a != (MatchAnchor{}) {
		t.Fatalf("unlink with offered = %+v %v", a, err)
	}
}

// SetCoverIf writes only while the book is as expected, checked in the write's
// transaction: no custom cover, the recorded match and the resolved ASIN and
// ISBN; SetCover writes regardless.
func TestSetCoverIf(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: t.TempDir()})
	const p = "A/Book"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{
		Set: map[string]string{FieldASIN: "B000000001"}, Match: &MatchAnchor{WorkID: "w", RecordingID: "r"}, Source: SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	img := pngBytes
	want := CoverExpect{WorkID: "w", RecordingID: "r", ASIN: "B000000001"}
	for name, expect := range map[string]CoverExpect{
		"another work":      {WorkID: "x", RecordingID: "r", ASIN: "B000000001"},
		"another recording": {WorkID: "w", ASIN: "B000000001"},
		"another asin":      {WorkID: "w", RecordingID: "r", ASIN: "B000000002"},
		"an isbn":           {WorkID: "w", RecordingID: "r", ASIN: "B000000001", ISBN: "9780000000002"},
		"no match":          {ASIN: "B000000001"},
	} {
		if err := c.CoverExpected(ctx, lib.ID, p, expect); !errors.Is(err, ErrCoverSuperseded) {
			t.Errorf("%s: CoverExpected = %v, want superseded", name, err)
		}
		if err := c.SetCoverIf(ctx, lib.ID, p, img, 0, SourceCommunity, &expect); !errors.Is(err, ErrCoverSuperseded) {
			t.Errorf("%s: SetCoverIf = %v, want superseded", name, err)
		}
	}
	if _, err := c.Cover(ctx, lib.ID, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a superseded write left a cover: %v", err)
	}
	if err := c.CoverExpected(ctx, lib.ID, "A/None", want); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no book = %v, want ErrNotFound", err)
	}
	if err := c.SetCoverIf(ctx, lib.ID, "A/None", img, 0, SourceCommunity, &want); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCoverIf, no book = %v, want ErrNotFound", err)
	}
	if err := c.SetCover(ctx, lib.ID, "A/None", img, 0, SourceEdited); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCover, no book = %v, want ErrNotFound", err)
	}
	// Art a scan found since the plan is a cover the community's doesn't go over.
	for _, has := range []bool{true, false} {
		if _, err := c.db.ExecContext(ctx, `UPDATE books SET has_cover = ? WHERE library_id = ? AND rel_path = ?`, has, lib.ID, p); err != nil {
			t.Fatal(err)
		}
		if err := c.CoverExpected(ctx, lib.ID, p, want); has != errors.Is(err, ErrCoverSuperseded) {
			t.Fatalf("has_cover %v: CoverExpected = %v", has, err)
		}
	}
	if err := c.SetCoverIf(ctx, lib.ID, p, img, 0, SourceCommunity, &want); err != nil {
		t.Fatalf("expected write = %v", err)
	}
	// It has a custom cover now: another conditional write is superseded, an
	// unconditional one (an upload) still goes in.
	if err := c.SetCoverIf(ctx, lib.ID, p, img, 0, SourceCommunity, &want); !errors.Is(err, ErrCoverSuperseded) {
		t.Fatalf("over a custom cover = %v, want superseded", err)
	}
	if err := c.SetCover(ctx, lib.ID, p, img, 0, SourceEdited); err != nil {
		t.Fatalf("upload = %v", err)
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
	if a := anchorOf(t, c, ctx, lib.ID, "new/Book"); a.WorkID != "the-work" || a.OfferedASIN != "B0OFFERED1" {
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
	if a := anchorOf(t, c, ctx, lib.ID, "Joined"); a.WorkID != "work-of-CD1" || a.OfferedASIN != "B0OFFERED1" {
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

// TestUnlinkClearsTheBooksCommunityMatch: an edit with an empty work id (the
// console's Unlink) takes back what the community match wrote for that one book,
// as ClearCommunityMatches does for a library: its community values and cover go
// and its effective metadata is rebuilt, so it is Not matched and the next run
// asks about it again. An admin's own edit stays, as does every other book's
// match, and the rest of the edit applies after the clear.
func TestUnlinkClearsTheBooksCommunityMatch(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, p := range []string{"A/Book", "A/Other"} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
			t.Fatal(err)
		}
		anchorBook(t, c, ctx, lib.ID, p, "the-work")
		if err := c.SetCover(ctx, lib.ID, p, pngBytes, 0, SourceCommunity); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EditBook(ctx, lib.ID, "A/Book", BookEdit{Set: map[string]string{FieldNarrator: "My Reader"}}); err != nil {
		t.Fatal(err)
	}
	unmatched := func() []string {
		t.Helper()
		books, err := c.UnmatchedBooks(ctx, lib.ID)
		if err != nil {
			t.Fatal(err)
		}
		return relPaths(books)
	}
	if got := unmatched(); len(got) != 0 {
		t.Fatalf("unmatched before = %v, want none", got)
	}

	if err := c.EditBook(ctx, lib.ID, "A/Book", BookEdit{Match: &MatchAnchor{}}); err != nil {
		t.Fatal(err)
	}
	b := mustBook(t, c, ctx, lib.ID, "A/Book")
	if b.Title != "Original Name" || b.Narrator != "My Reader" {
		t.Errorf("unlinked book = title %q narrator %q, want the scan's title and the admin's narrator", b.Title, b.Narrator)
	}
	if a := anchorOf(t, c, ctx, lib.ID, "A/Book"); a != (MatchAnchor{}) {
		t.Errorf("anchor after the unlink = %+v, want none", a)
	}
	if _, err := c.Cover(ctx, lib.ID, "A/Book"); !errors.Is(err, ErrNotFound) {
		t.Errorf("community cover after the unlink = %v, want removed", err)
	}
	if titles := searchTitles(t, c, ctx, lib.ID, "Community"); !slices.Equal(titles, []string{"Community Title"}) {
		t.Errorf("search for the community title = %v, want only the other book", titles)
	}
	if got := unmatched(); !slices.Equal(got, []string{"A/Book"}) {
		t.Errorf("unmatched after the unlink = %v, want the unlinked book", got)
	}
	// The other book keeps its match.
	if o := mustBook(t, c, ctx, lib.ID, "A/Other"); o.Title != "Community Title" {
		t.Errorf("other book's title = %q", o.Title)
	}
	if a := anchorOf(t, c, ctx, lib.ID, "A/Other"); a.WorkID != "the-work" {
		t.Errorf("other book's anchor = %+v", a)
	}
	if _, err := c.Cover(ctx, lib.ID, "A/Other"); err != nil {
		t.Errorf("other book's cover = %v", err)
	}

	// With the rest of an edit: the clear first, then the edit's own value.
	if err := c.EditBook(ctx, lib.ID, "A/Other", BookEdit{Match: &MatchAnchor{}, Set: map[string]string{FieldTitle: "Mine"}}); err != nil {
		t.Fatal(err)
	}
	if o := mustBook(t, c, ctx, lib.ID, "A/Other"); o.Title != "Mine" {
		t.Errorf("title after an unlink with an edit = %q, want the edit's", o.Title)
	}
	// Unlinking a book with nothing to clear is no error.
	if err := c.EditBook(ctx, lib.ID, "A/Book", BookEdit{Match: &MatchAnchor{}}); err != nil {
		t.Fatal(err)
	}
}

// TestUnlinkWithASeriesEditReadsTheUnlinkedBook: the clear comes first in the
// edit, so an admin's series change sent with an unlink swaps against the book's
// own series, never listing the community's main series (about to go) as one of
// the admin's.
func TestUnlinkWithASeriesEditReadsTheUnlinkedBook(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "A/Book")); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, "A/Book", BookEdit{
		Set: map[string]string{FieldSeries: "Comm Series", FieldSeriesIndex: "4",
			FieldMoreSeries: `[{"name":"Side","position":2}]`},
		Match:  &MatchAnchor{WorkID: "the-work"},
		Source: SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, "A/Book"); len(b.SeriesList) != 2 {
		t.Fatalf("matched book's series = %+v, want two", b.SeriesList)
	}

	if err := c.EditBook(ctx, lib.ID, "A/Book", BookEdit{Match: &MatchAnchor{}, Set: map[string]string{FieldSeries: "Side"}}); err != nil {
		t.Fatal(err)
	}
	b := mustBook(t, c, ctx, lib.ID, "A/Book")
	if b.Series != "Side" || len(b.SeriesList) != 0 {
		t.Errorf("unlinked book's series = %q %+v, want Side alone (no community series kept as the admin's)", b.Series, b.SeriesList)
	}
	if a := anchorOf(t, c, ctx, lib.ID, "A/Book"); a != (MatchAnchor{}) {
		t.Errorf("anchor after the unlink = %+v, want none", a)
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

// TestAnchorWrittenOnlyIfStillTheOne: an edit with MatchIf records its match only
// while the book's recorded match is still MatchIf (work, recording and offered
// identifiers), checked in the edit's transaction; else nothing of the edit is
// written (an ASIN taken from the old match would outrank the new one,
// server#354) and EditBook returns ErrMatchedSince. A zero MatchIf records one
// only while the book has none, and no ASIN or ISBN either.
func TestAnchorWrittenOnlyIfStillTheOne(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Book"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	anchorBook(t, c, ctx, lib.ID, p, "the-work")
	read := anchorOf(t, c, ctx, lib.ID, p)
	edit := func(title string, was *MatchAnchor) error {
		t.Helper()
		return c.EditBook(ctx, lib.ID, p, BookEdit{
			Set:     map[string]string{FieldTitle: title, FieldASIN: "B0OLDMATCH"},
			Match:   &MatchAnchor{WorkID: "the-work", RecordingID: "rec-1", OfferedASIN: "B0FILLED01", How: MatchHowAuto, RunID: 3},
			MatchIf: was, Source: SourceCommunity,
		})
	}
	field := func(name string) string {
		t.Helper()
		st, err := c.BookMatchState(ctx, lib.ID, p)
		if err != nil {
			t.Fatal(err)
		}
		return st.Fields[name].Value
	}
	stale := func(label string, was *MatchAnchor) {
		t.Helper()
		if err := edit("Stale "+label, was); !errors.Is(err, ErrMatchedSince) {
			t.Fatalf("%s: err = %v, want ErrMatchedSince", label, err)
		}
	}
	// Matched by hand to another work since the plan read it: the match stays,
	// and nothing else of the edit goes in (no title, no old ASIN).
	anchorBook(t, c, ctx, lib.ID, p, "hand-work")
	stale("hand", &read)
	if a := anchorOf(t, c, ctx, lib.ID, p); a.WorkID != "hand-work" || a.How != MatchHowAccepted {
		t.Fatalf("a hand match since = %+v, want it kept", a)
	}
	if got := field(FieldTitle); got == "Stale hand" {
		t.Fatalf("title = %q, want the edit rolled back", got)
	}
	if got := field(FieldASIN); got == "B0OLDMATCH" {
		t.Fatalf("asin = %q, want the old match's ASIN not written", got)
	}
	// The same work but other offered identifiers (accepted again by hand): kept.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{
		Match: &MatchAnchor{WorkID: "the-work", RecordingID: "rec-1", OfferedASIN: "B0HAND0001"}, Source: SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	stale("re-accept", &read)
	if a := anchorOf(t, c, ctx, lib.ID, p); a.OfferedASIN != "B0HAND0001" || a.How != MatchHowAccepted {
		t.Fatalf("a re-accept since = %+v, want it kept", a)
	}
	// Still the one read: written, with the rest of the edit, the match keeping
	// how it was made (TestBookkeepingKeepsTheMatchsProvenance).
	read = anchorOf(t, c, ctx, lib.ID, p)
	if err := edit("Written", &read); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a.OfferedASIN != "B0FILLED01" || a.How != MatchHowAccepted {
		t.Fatalf("still the one read = %+v, want it written, still accepted", a)
	}
	if got := field(FieldTitle); got != "Written" {
		t.Fatalf("title = %q, want the edit applied", got)
	}
	// A zero MatchIf (none read): never over a match the book has now.
	stale("none", &MatchAnchor{})
	if a := anchorOf(t, c, ctx, lib.ID, p); a.OfferedASIN != "B0FILLED01" {
		t.Fatalf("none read, one there = %+v, want it kept", a)
	}
	if got := field(FieldTitle); got != "Written" {
		t.Fatalf("title = %q, want the stale edit rolled back", got)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Match: &MatchAnchor{}}); err != nil { // Unlink
		t.Fatal(err)
	}
	if err := edit("Fresh", &MatchAnchor{}); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a.WorkID != "the-work" || a.OfferedASIN != "B0FILLED01" {
		t.Fatalf("none read, none there = %+v, want it written", a)
	}
	// A zero MatchIf is an unmatched book: an ASIN set by hand since, with no
	// match recorded, is kept too, and no match goes in beside it.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Match: &MatchAnchor{}}); err != nil { // Unlink
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{FieldASIN: "B0BYHAND01"}}); err != nil {
		t.Fatal(err)
	}
	stale("an ASIN by hand", &MatchAnchor{})
	if got := field(FieldASIN); got != "B0BYHAND01" {
		t.Fatalf("asin = %q, want the one set by hand kept", got)
	}
	if a := anchorOf(t, c, ctx, lib.ID, p); a.WorkID != "" {
		t.Fatalf("an ASIN by hand, none recorded = %+v, want no match written", a)
	}
}

// TestAnchorWrittenOnlyIfStillItsASIN: a zero MatchIf beside MatchIfASIN (an
// unknown_asin run's book, matched by its own ASIN alone) records a match while
// the book still has that ASIN (compared as normalized) and the unknown-ASIN
// report still holds the ASIN as unknown; an ISBN it gained since changes
// nothing (/meta asks by the ASIN alone, server#392), an ASIN it gained since is
// kept, as is an ASIN the community knows now, and nothing of the edit goes in
// (server#392).
func TestAnchorWrittenOnlyIfStillItsASIN(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	books := map[string]string{"A/Same": "B0NOPE0001", "A/Other": "B0NOPE0002", "A/ISBN": "B0NOPE0003", "A/Plain": "B0NOPE0004",
		"A/Pinned": "B0NOPE0005", "A/Learned": "B0NOPE0006", "A/Unchecked": "B0NOPE0007"}
	var unknown []ASINCheck
	for p, asin := range books {
		b := scannedBook(lib.ID, p)
		b.ASIN = asin
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
		if p != "A/Unchecked" {
			unknown = append(unknown, ASINCheck{ASIN: asin, Unknown: true})
		}
	}
	// The report found each unknown (but the one it never looked up).
	if err := c.SaveASINChecks(ctx, unknown, "2026-11-01T00:00:00.000Z"); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, "A/Other", BookEdit{Set: map[string]string{FieldASIN: "B0BYHAND01"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, "A/ISBN", BookEdit{Set: map[string]string{FieldISBN: "9780553418026"}}); err != nil {
		t.Fatal(err)
	}
	edit := func(p, asin string) error {
		t.Helper()
		return c.EditBook(ctx, lib.ID, p, BookEdit{
			Set:   map[string]string{FieldTitle: "Matched"},
			Match: &MatchAnchor{WorkID: "the-work", How: MatchHowAuto}, MatchIf: &MatchAnchor{}, MatchIfASIN: asin,
			Source: SourceCommunity,
		})
	}
	title := func(p string) string {
		t.Helper()
		st, err := c.BookMatchState(ctx, lib.ID, p)
		if err != nil {
			t.Fatal(err)
		}
		return st.Fields[FieldTitle].Value
	}
	// Allowed: still the ASIN read (another case of it is the same ASIN).
	if err := edit("A/Same", " b0nope0001"); err != nil {
		t.Fatal(err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, "A/Same"); a.WorkID != "the-work" || title("A/Same") != "Matched" {
		t.Fatalf("still its ASIN: match %+v, title %q, want both written", a, title("A/Same"))
	}
	// Allowed: an ISBN gained since (the edit leaves it).
	if err := edit("A/ISBN", "B0NOPE0003"); err != nil {
		t.Fatal(err)
	}
	if st, err := c.BookMatchState(ctx, lib.ID, "A/ISBN"); err != nil || st.Anchor.WorkID != "the-work" ||
		st.Fields[FieldISBN].Value != "9780553418026" {
		t.Fatalf("an ISBN gained since: %+v (%v), want the match written beside the ISBN", st, err)
	}
	// Denied: another ASIN set since.
	if err := edit("A/Other", "B0NOPE0002"); !errors.Is(err, ErrMatchedSince) {
		t.Fatalf("another ASIN since: err = %v, want ErrMatchedSince", err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, "A/Other"); a.WorkID != "" || title("A/Other") == "Matched" {
		t.Fatalf("another ASIN since: match %+v, title %q, want nothing written", a, title("A/Other"))
	}
	// Denied: a lookup found the ASIN since (the community knows it now, so it
	// names the book's record), or the report holds nothing about it.
	if err := c.ClearUnknownASIN(ctx, "B0NOPE0006"); err != nil {
		t.Fatal(err)
	}
	for p, asin := range map[string]string{"A/Learned": "B0NOPE0006", "A/Unchecked": "B0NOPE0007"} {
		if st, err := c.BookMatchState(ctx, lib.ID, p); err != nil || st.ASINUnknown {
			t.Fatalf("%s: ASINUnknown = %v (%v), want false", p, st.ASINUnknown, err)
		}
		if err := edit(p, asin); !errors.Is(err, ErrMatchedSince) {
			t.Fatalf("%s: err = %v, want ErrMatchedSince", p, err)
		}
		if a := anchorOf(t, c, ctx, lib.ID, p); a.WorkID != "" || title(p) == "Matched" {
			t.Fatalf("%s: match %+v, title %q, want nothing written", p, a, title(p))
		}
	}
	if st, err := c.BookMatchState(ctx, lib.ID, "A/Plain"); err != nil || !st.ASINUnknown {
		t.Fatalf("A/Plain: ASINUnknown = %v (%v), want true while the report holds it unknown", st.ASINUnknown, err)
	}
	// Without MatchIfASIN a zero MatchIf keeps its rule: a book with an ASIN is matched.
	if err := edit("A/Plain", ""); !errors.Is(err, ErrMatchedSince) {
		t.Fatalf("no MatchIfASIN: err = %v, want ErrMatchedSince", err)
	}
	// An admin typed the same ASIN in after the plan read the book: an edit
	// replacing it is refused (the admin's ASIN is never replaced), one that
	// leaves it records the match.
	if err := c.EditBook(ctx, lib.ID, "A/Pinned", BookEdit{Set: map[string]string{FieldASIN: "B0NOPE0005"}}); err != nil {
		t.Fatal(err)
	}
	replace := BookEdit{
		Set:   map[string]string{FieldASIN: "B0UK000001", FieldTitle: "Matched"},
		Match: &MatchAnchor{WorkID: "the-work", How: MatchHowAuto}, MatchIf: &MatchAnchor{}, MatchIfASIN: "B0NOPE0005",
		Source: SourceCommunity,
	}
	if err := c.EditBook(ctx, lib.ID, "A/Pinned", replace); !errors.Is(err, ErrMatchedSince) {
		t.Fatalf("replacing an admin's ASIN: err = %v, want ErrMatchedSince", err)
	}
	if st, err := c.BookMatchState(ctx, lib.ID, "A/Pinned"); err != nil || st.Fields[FieldASIN].Value != "B0NOPE0005" ||
		st.Fields[FieldASIN].Source != SourceEdited || st.Anchor.WorkID != "" {
		t.Fatalf("admin's ASIN: %+v / %+v (%v), want kept and no match", st.Fields[FieldASIN], st.Anchor, err)
	}
	if err := edit("A/Pinned", "B0NOPE0005"); err != nil {
		t.Fatalf("keeping an admin's ASIN: %v", err)
	}
	if a := anchorOf(t, c, ctx, lib.ID, "A/Pinned"); a.WorkID != "the-work" {
		t.Fatalf("keeping an admin's ASIN: match %+v, want recorded", a)
	}
}

// TestWorkSources: a book with a recorded match has its match's work,
// whatever its identifiers resolved to; one known by its identifiers alone
// has none, but the time the upcoming job resolved them; a book with neither,
// or none at the path, is left out.
func TestWorkSources(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	if err := f.c.SaveUpcomingOwned(f.ctx, []UpcomingOwned{
		{Ref: Ref{LibraryID: f.main.ID, Path: "Kemper/One"}, IDKey: "a:B0KEMPER", WorkID: "w-one", CheckedAt: "2026-11-01T00:00:00.000Z"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := f.c.WorkSources(f.ctx, []Ref{
		{LibraryID: f.main.ID, Path: "Bray/One"}, {LibraryID: f.main.ID, Path: "Rematched/Two"},
		{LibraryID: f.main.ID, Path: "Kemper/One"}, {LibraryID: f.other.ID, Path: "Abridged/One"},
		{LibraryID: f.main.ID, Path: "Else/Three"}, {LibraryID: f.main.ID, Path: "Nowhere"},
	})
	want := map[Ref]WorkSource{
		{LibraryID: f.main.ID, Path: "Bray/One"}:      {MatchWorkID: "w-one", ResolvedAt: "now"},
		{LibraryID: f.main.ID, Path: "Rematched/Two"}: {MatchWorkID: "w-two", ResolvedAt: "now"},
		{LibraryID: f.main.ID, Path: "Kemper/One"}:    {ResolvedAt: "2026-11-01T00:00:00.000Z"},
		{LibraryID: f.other.ID, Path: "Abridged/One"}: {MatchWorkID: "w-one"},
	}
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("work sources = %v, %v; want %v", got, err, want)
	}
	if got, err = f.c.WorkSources(f.ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("no refs = %v, %v", got, err)
	}
}

// TestMatchWorkIDs: only the books with a recorded match, each its match's
// work; one known by its identifiers alone (the fixture's Kemper/One, the
// upcoming job's resolution) is left out, as are a book with neither and none
// at the path. Each is WorkSources' MatchWorkID, so /meta's placement and the
// Bridge's estimate never tell a match apart differently.
func TestMatchWorkIDs(t *testing.T) {
	t.Parallel()
	f := newRecordingsFixture(t)
	refs := []Ref{
		{LibraryID: f.main.ID, Path: "Bray/One"}, {LibraryID: f.main.ID, Path: "Rematched/Two"},
		{LibraryID: f.main.ID, Path: "Kemper/One"}, {LibraryID: f.other.ID, Path: "Abridged/One"},
		{LibraryID: f.main.ID, Path: "Else/Three"}, {LibraryID: f.main.ID, Path: "Nowhere"},
	}
	got, err := f.c.MatchWorkIDs(f.ctx, refs)
	want := map[Ref]string{
		{LibraryID: f.main.ID, Path: "Bray/One"}:      "w-one",
		{LibraryID: f.main.ID, Path: "Rematched/Two"}: "w-two",
		{LibraryID: f.other.ID, Path: "Abridged/One"}: "w-one",
	}
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("match work ids = %v, %v; want %v", got, err, want)
	}
	sources, err := f.c.WorkSources(f.ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if got[ref] != sources[ref].MatchWorkID {
			t.Errorf("%v: match work id %q, WorkSources' %q", ref, got[ref], sources[ref].MatchWorkID)
		}
	}
}

// matchProvenance is the book's recorded match's how, run, editor and time, as
// stored (run and editor NULL as 0).
func matchProvenance(t *testing.T, c *Catalog, ctx context.Context, libID int64, path string) (how string, runID, setBy int64, setAt string) {
	t.Helper()
	if err := c.db.QueryRowContext(ctx, `SELECT how, COALESCE(run_id, 0), COALESCE(set_by, 0), set_at
	     FROM book_matches WHERE library_id = ? AND path = ?`, libID, path).Scan(&how, &runID, &setBy, &setAt); err != nil {
		t.Fatal(err)
	}
	return how, runID, setBy, setAt
}

// TestBookkeepingKeepsTheMatchsProvenance (server#355 items 1-2): a run's
// write of a match it read (MatchIf naming one: a refresh's heal or new
// identifier, a repick's ASIN) rewrites the work, recording and offered
// identifiers only, so the match keeps how, by whom, when and by which run it
// was made, whatever the edit's own say. An accept by hand (no MatchIf) is a
// match made again: its provenance replaces the old one's.
func TestBookkeepingKeepsTheMatchsProvenance(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	uid := seedUser(t, c, ctx)
	const p = "A/Book"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{
		Match: &MatchAnchor{WorkID: "the-work", RecordingID: "rec-1"}, Source: SourceCommunity, UserID: uid,
	}); err != nil {
		t.Fatal(err)
	}
	how, run, by, at := matchProvenance(t, c, ctx, lib.ID, p)
	if how != MatchHowAccepted || run != 0 || by != uid || at == "" {
		t.Fatalf("accepted = %s run %d by %d at %q", how, run, by, at)
	}
	bookkeep := func(label string, a MatchAnchor) {
		t.Helper()
		was := anchorOf(t, c, ctx, lib.ID, p)
		a.How, a.RunID = MatchHowAuto, 42
		if err := c.EditBook(ctx, lib.ID, p, BookEdit{
			Set: map[string]string{FieldDescription: label}, Match: &a, MatchIf: &was, Source: SourceCommunity,
		}); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		// A heal to another work id moves the time alone: the upcoming job
		// reads a change of work by it.
		heal := a.WorkID != was.WorkID
		h, r, b, s := matchProvenance(t, c, ctx, lib.ID, p)
		if h != how || r != run || b != by || (s != at) != heal {
			t.Fatalf("%s: provenance = %s run %d by %d at %q, want %s run %d by %d kept (at %q, moved %v)",
				label, h, r, b, s, how, run, by, at, heal)
		}
		at = s
		if got := anchorOf(t, c, ctx, lib.ID, p); got.WorkID != a.WorkID || got.OfferedASIN != a.OfferedASIN {
			t.Fatalf("%s: anchor = %+v, want %+v written", label, got, a)
		}
	}
	// A refresh's new identifier, then a repick's ASIN: the same match.
	bookkeep("identifier", MatchAnchor{WorkID: "the-work", RecordingID: "rec-1", OfferedASIN: "B0FILLED01"})
	bookkeep("repick", MatchAnchor{WorkID: "the-work", RecordingID: "rec-1", OfferedASIN: "B0UKREPICK"})
	// A heal: the work merged into another; still the match the admin made,
	// its time the heal's.
	bookkeep("heal", MatchAnchor{WorkID: "merged-work", RecordingID: "rec-1", OfferedASIN: "B0UKREPICK"})

	// The match accepted again by hand: made anew, by its run-less editor now.
	other := seedUserNamed(t, c, ctx, "other")
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{
		Match: &MatchAnchor{WorkID: "merged-work", RecordingID: "rec-1"}, Source: SourceCommunity, UserID: other,
	}); err != nil {
		t.Fatal(err)
	}
	if h, r, b, s := matchProvenance(t, c, ctx, lib.ID, p); h != MatchHowAccepted || r != 0 || b != other || s == at {
		t.Fatalf("re-accepted = %s run %d by %d at %q, want the new accept's", h, r, b, s)
	}
}

// TestOfferedLeavesTheBooksOwnIdentifier (server#355 item 3): an identifier a
// match offers that the book already holds of its own (its files', an
// enrichment's, an admin's) is not recorded as offered, so a refresh fills it
// once the book loses it; one that is a community value (an earlier match's),
// or that the book doesn't hold, is.
func TestOfferedLeavesTheBooksOwnIdentifier(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, p := range []string{"A/Edited", "A/Enriched", "A/Community"} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EditBook(ctx, lib.ID, "A/Edited", BookEdit{Set: map[string]string{FieldASIN: "B0OWN00001"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.BackfillASIN(ctx, lib.ID, "A/Enriched", "B0OWN00001"); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, "A/Community", BookEdit{
		Set: map[string]string{FieldASIN: "B0OWN00001"}, Source: SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	accept := func(p string) MatchAnchor {
		t.Helper()
		if err := c.EditBook(ctx, lib.ID, p, BookEdit{
			Match:  &MatchAnchor{WorkID: "w", OfferedASIN: "b0own00001", OfferedISBN: "9780000000002"},
			Source: SourceCommunity,
		}); err != nil {
			t.Fatal(err)
		}
		return anchorOf(t, c, ctx, lib.ID, p)
	}
	for _, p := range []string{"A/Edited", "A/Enriched"} {
		if a := accept(p); a.OfferedASIN != "" || a.OfferedISBN != "9780000000002" {
			t.Errorf("%s: offered = %q %q, want the book's own ASIN left out, the ISBN kept", p, a.OfferedASIN, a.OfferedISBN)
		}
	}
	if a := accept("A/Community"); a.OfferedASIN != "B0OWN00001" || a.OfferedISBN != "9780000000002" {
		t.Errorf("community ASIN: offered = %q %q, want both recorded", a.OfferedASIN, a.OfferedISBN)
	}
}

// TestOfferedReadsTheBookAsTheEditLeavesIt (server#355): the book's own
// identifiers are read as the match's own edit leaves them. One the edit's
// revert uncovers (an enrichment's under an earlier match's ASIN) is the
// book's own, so not recorded as offered; one the edit writes as a community
// value over the book's own, or uncovers nothing by reverting, is recorded;
// and a community edit's revert never uncovers what an admin's edit hides.
func TestOfferedReadsTheBookAsTheEditLeavesIt(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const own, other = "B0OWN00001", "B0OTHER001"
	community := func(asin string) BookEdit {
		return BookEdit{Set: map[string]string{FieldASIN: asin}, Source: SourceCommunity}
	}
	for p, before := range map[string][]BookEdit{
		"A/Uncovered": {community(other)},                           // the enrichment's ASIN under an earlier match's
		"A/Bare":      {community(own)},                             // an earlier match's ASIN over nothing
		"A/Replaced":  nil,                                          // the enrichment's ASIN alone
		"A/Edited":    {{Set: map[string]string{FieldASIN: other}}}, // an admin's ASIN over the enrichment's
	} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
			t.Fatal(err)
		}
		if p != "A/Bare" {
			if err := c.BackfillASIN(ctx, lib.ID, p, own); err != nil {
				t.Fatal(err)
			}
		}
		for _, e := range before {
			if err := c.EditBook(ctx, lib.ID, p, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	accept := func(p, offered string, edit BookEdit) string {
		t.Helper()
		edit.Match, edit.Source = &MatchAnchor{WorkID: "w", OfferedASIN: offered}, SourceCommunity
		if err := c.EditBook(ctx, lib.ID, p, edit); err != nil {
			t.Fatal(err)
		}
		return anchorOf(t, c, ctx, lib.ID, p).OfferedASIN
	}
	revert := BookEdit{Revert: []string{FieldASIN}}
	if got := accept("A/Uncovered", own, revert); got != "" {
		t.Errorf("uncovered by the revert: offered = %q, want the book's own ASIN left out", got)
	}
	if st, err := c.BookMatchState(ctx, lib.ID, "A/Uncovered"); err != nil || st.Fields[FieldASIN].Value != own || st.Fields[FieldASIN].Locked {
		t.Fatalf("uncovered = %+v %v, want the enrichment's ASIN showing", st.Fields[FieldASIN], err)
	}
	if got := accept("A/Bare", own, revert); got != own {
		t.Errorf("reverted to nothing: offered = %q, want it recorded", got)
	}
	if got := accept("A/Replaced", other, BookEdit{Set: map[string]string{FieldASIN: other}}); got != other {
		t.Errorf("written over the book's own: offered = %q, want it recorded", got)
	}
	if got := accept("A/Edited", own, revert); got != own {
		t.Errorf("an admin's ASIN kept by the revert: offered = %q, want the hidden one recorded", got)
	}
}

// TestUnlinkMarksTheRunsThatAppliedIt (server#331 item 6): an Unlink marks the
// book's applied items of every kept run unlinked (still applied, so the run's
// counts stay what its apply did), leaving its other items and other books'
// alone; an Unlink of a book with nothing to take back marks nothing.
func TestUnlinkMarksTheRunsThatAppliedIt(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, p := range []string{"A/Book", "A/Other"} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
			t.Fatal(err)
		}
		anchorBook(t, c, ctx, lib.ID, p, "the-work")
	}
	run, err := c.StartMatchRun(ctx, MatchRun{Mode: MatchModeMatch, Total: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range []MatchRunItem{
		{LibraryID: lib.ID, Path: "A/Book", Outcome: OutcomeAuto},
		{LibraryID: lib.ID, Path: "A/Book", Outcome: OutcomeReview},
		{LibraryID: lib.ID, Path: "A/Other", Outcome: OutcomeAuto},
	} {
		if err := c.RecordMatchItem(ctx, run, &it); err != nil {
			t.Fatal(err)
		}
	}
	items := func() []MatchRunItem {
		t.Helper()
		got, _, err := c.ListMatchRunItems(ctx, run, "", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(got, func(a, b MatchRunItem) int { return int(a.ID - b.ID) })
		return got
	}
	all := items()
	if err := c.FinishMatching(ctx, run, MatchReady, ""); err != nil {
		t.Fatal(err)
	}
	for _, it := range []MatchRunItem{all[0], all[2]} {
		if err := c.MarkMatchItem(ctx, run, it.ID, ItemApplied, "cover_failed"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EditBook(ctx, lib.ID, "A/Book", BookEdit{Match: &MatchAnchor{}}); err != nil {
		t.Fatal(err)
	}
	got := items()
	want := []struct{ applied, detail string }{
		{ItemApplied, ItemDetailUnlinked}, {"", ""}, {ItemApplied, "cover_failed"},
	}
	for i, w := range want {
		if got[i].Applied != w.applied || got[i].Detail != w.detail {
			t.Errorf("item %d (%s) = %q %q, want %q %q", i, got[i].Path, got[i].Applied, got[i].Detail, w.applied, w.detail)
		}
	}
	if r, _ := c.GetMatchRun(ctx, run); r.Counts.Applied != 2 {
		t.Errorf("applied count = %d, want the apply's 2 kept", r.Counts.Applied)
	}
	// Nothing left to take back: the other book's item is not touched by a
	// second Unlink of this one.
	if err := c.EditBook(ctx, lib.ID, "A/Book", BookEdit{Match: &MatchAnchor{}}); err != nil {
		t.Fatal(err)
	}
	if got := items(); got[2].Detail != "cover_failed" {
		t.Errorf("other book's item = %+v, want it as it was", got[2])
	}
}

// TestUnlinkWithAnEditRebuildsOnce (server#331 item 6): an Unlink sent with
// field edits rebuilds the book once, at the end of the edit, and the book
// reads unlinked with the edit's values; one with a label edit (which reads
// the books row) rebuilds first, so it adds to the unlinked book's labels.
func TestUnlinkWithAnEditRebuildsOnce(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, p := range []string{"A/Book", "A/Labels"} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
			t.Fatal(err)
		}
		anchorBook(t, c, ctx, lib.ID, p, "the-work")
		if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{FieldGenres: `["Community Genre"]`}, Source: SourceCommunity}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.ExecContext(ctx, `CREATE TABLE rebuilds(n INTEGER);
		INSERT INTO rebuilds VALUES (0);
		CREATE TRIGGER count_rebuilds AFTER UPDATE OF title ON books BEGIN UPDATE rebuilds SET n = n + 1; END`); err != nil {
		t.Fatal(err)
	}
	rebuilds := func() int {
		t.Helper()
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT n FROM rebuilds`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		_, _ = c.db.ExecContext(ctx, `UPDATE rebuilds SET n = 0`)
		return n
	}
	if err := c.EditBook(ctx, lib.ID, "A/Book", BookEdit{Match: &MatchAnchor{}, Set: map[string]string{FieldNarrator: "Mine"}}); err != nil {
		t.Fatal(err)
	}
	if n := rebuilds(); n != 1 {
		t.Errorf("unlink with a field edit rebuilt the book %d times, want once", n)
	}
	b := mustBook(t, c, ctx, lib.ID, "A/Book")
	if b.Title != "Original Name" || b.Narrator != "Mine" || len(b.Genres) != 0 {
		t.Errorf("book = title %q narrator %q genres %v, want unlinked with the edit", b.Title, b.Narrator, b.Genres)
	}
	if err := c.EditBook(ctx, lib.ID, "A/Labels", BookEdit{Match: &MatchAnchor{}, Add: map[string][]string{FieldTags: {"Mine"}}}); err != nil {
		t.Fatal(err)
	}
	b = mustBook(t, c, ctx, lib.ID, "A/Labels")
	if b.Title != "Original Name" || len(b.Genres) != 0 || !slices.Equal(b.Tags, []string{"Mine"}) {
		t.Errorf("labels book = title %q genres %v tags %v, want unlinked with the tag", b.Title, b.Genres, b.Tags)
	}
}

// TestFieldShowsTheEnrichmentUnderAnOverride (server#350 item 1): a field's
// Enrichment is the Bridge's enrichment an override hides (what an Unlink
// leaves the book with), omitted while none does or there is none.
func TestFieldShowsTheEnrichmentUnderAnOverride(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, p := range []string{"A/Enriched", "A/Plain"} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.BackfillASIN(ctx, lib.ID, "A/Enriched", "B0ENRICHED"); err != nil {
		t.Fatal(err)
	}
	asin := func(p string) FieldValue {
		t.Helper()
		st, err := c.BookMatchState(ctx, lib.ID, p)
		if err != nil {
			t.Fatal(err)
		}
		return st.Fields[FieldASIN]
	}
	if f := asin("A/Enriched"); f.Value != "B0ENRICHED" || f.Enrichment != "" {
		t.Fatalf("enriched = %+v, want the enrichment as the value, none under it", f)
	}
	for _, p := range []string{"A/Enriched", "A/Plain"} {
		if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{FieldASIN: "B0MATCHED1"}, Source: SourceCommunity}); err != nil {
			t.Fatal(err)
		}
	}
	if f := asin("A/Enriched"); f.Value != "B0MATCHED1" || f.Enrichment != "B0ENRICHED" {
		t.Errorf("overridden enrichment = %+v, want it under the community value", f)
	}
	if f := asin("A/Plain"); f.Enrichment != "" {
		t.Errorf("no enrichment = %+v, want none", f)
	}
}

// TestKeepEditedLeavesAdminEdits: a match run's write (KeepEdited) leaves any
// field an admin edited since it read the book, not only the title, and still
// writes the others; without the flag a community edit replaces it.
func TestKeepEditedLeavesAdminEdits(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Book"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{FieldDescription: "Mine"}}); err != nil {
		t.Fatal(err)
	}
	override := func(field string) string {
		t.Helper()
		var v, src string
		if err := c.db.QueryRowContext(ctx, `SELECT value, source FROM book_overrides
			WHERE library_id = ? AND path = ? AND field = ?`, lib.ID, p, field).Scan(&v, &src); err != nil {
			t.Fatal(err)
		}
		return v + "|" + src
	}
	theirs := map[string]string{FieldDescription: "Theirs", FieldTitle: "Their Title"}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: theirs, Source: SourceCommunity, KeepEdited: true}); err != nil {
		t.Fatal(err)
	}
	if got := override(FieldDescription); got != "Mine|"+SourceEdited {
		t.Errorf("description after a run's write = %q, want the admin's", got)
	}
	if got := override(FieldTitle); got != "Their Title|"+SourceCommunity {
		t.Errorf("title after a run's write = %q", got)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: theirs, Source: SourceCommunity}); err != nil {
		t.Fatal(err)
	}
	if got := override(FieldDescription); got != "Theirs|"+SourceCommunity {
		t.Errorf("description after an accepted community edit = %q", got)
	}
}

// TestRevertCommunityTitleLocksTheFiles: an admin's revert of a community
// title stores the title underneath as an edited override (a later fill keeps
// it), unless that title is one an admin couldn't type in (too long), which is
// reverted as before.
func TestRevertCommunityTitleLocksTheFiles(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	long := strings.Repeat("Long Title ", 60)
	for path, title := range map[string]string{"A/Short": "Original Name", "A/Long": long} {
		b := scannedBook(lib.ID, path)
		b.Title = title
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := c.EditBook(ctx, lib.ID, path, BookEdit{Set: map[string]string{FieldTitle: "Their Title"}, Source: SourceCommunity}); err != nil {
			t.Fatal(err)
		}
		if err := c.EditBook(ctx, lib.ID, path, BookEdit{Revert: []string{FieldTitle}}); err != nil {
			t.Fatal(err)
		}
	}
	var v, src string
	if err := c.db.QueryRowContext(ctx, `SELECT value, source FROM book_overrides
		WHERE library_id = ? AND path = 'A/Short' AND field = ?`, lib.ID, FieldTitle).Scan(&v, &src); err != nil {
		t.Fatal(err)
	}
	if v != "Original Name" || src != SourceEdited {
		t.Errorf("reverted title override = %q (%s), want the files' title, edited", v, src)
	}
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM book_overrides
		WHERE library_id = ? AND path = 'A/Long' AND field = ?`, lib.ID, FieldTitle).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a revert onto an over-long title left %d overrides, want a plain revert", n)
	}
}

package matchrun

import (
	"context"
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// listing is a store listing the community holds with no ASIN or ISBN (a Guild
// book): The Martian's work, its one recording of the given length, metaserve
// finding the author in full and the book's series position.
func listing(work string, score, runtimeMin int) meta.MatchCandidate {
	c := cand(work, score, "")
	c.Recordings[0].ASINs, c.Recordings[0].ASINRefs, c.Recordings[0].ASINRegion = nil, nil, ""
	c.Recordings[0].ISBNs, c.Recordings[0].RuntimeMin = nil, runtimeMin
	c.Reasons = &meta.MatchReasons{Author: "full", Series: "position"}
	return c
}

// TestClassifyStoreListing: a match with no identifier to attach is confident
// only on firmer ground (LibraForge E6): score 90 and a plain 10-point lead, the
// author in full, the series at the book's position and the recording within 2%
// of the book's length, both known. Each denial alone keeps it for review, and the
// identifier rule is unchanged.
func TestClassifyStoreListing(t *testing.T) {
	t.Parallel()
	const book = 600 * 60.0 // seconds
	ok := listing("w", 92, 609)
	withReasons := func(author, series string) meta.MatchCandidate {
		c := ok
		c.Reasons = &meta.MatchReasons{Author: author, Series: series}
		return c
	}
	noReasons := ok
	noReasons.Reasons = nil
	runnerUp := func(score int) meta.MatchCandidate {
		return meta.MatchCandidate{Score: score, Reasons: &meta.MatchReasons{}}
	}
	proposal := func(c meta.MatchCandidate) catalog.MatchProposal { return Propose(&c, &c.Recordings[0]) }
	withScore := func(score int) meta.MatchCandidate {
		c := ok
		c.Score = score
		return c
	}
	for name, tc := range map[string]struct {
		cands   []meta.MatchCandidate
		seconds float64
		runtime int // the proposal's recording, minutes
		want    string
	}{
		"allowed: 92, lead 12, full, position, 1.5% off": {[]meta.MatchCandidate{ok, runnerUp(80)}, book, 609, catalog.OutcomeAuto},
		"allowed: alone":                     {[]meta.MatchCandidate{ok}, book, 609, catalog.OutcomeAuto},
		"denied: the author's surname only":  {[]meta.MatchCandidate{withReasons("surname", "position"), runnerUp(80)}, book, 609, catalog.OutcomeReview},
		"denied: the series name only":       {[]meta.MatchCandidate{withReasons("full", "name"), runnerUp(80)}, book, 609, catalog.OutcomeReview},
		"denied: 2.5% off":                   {[]meta.MatchCandidate{ok, runnerUp(80)}, book, 615, catalog.OutcomeReview},
		"denied: the book's length unknown":  {[]meta.MatchCandidate{ok, runnerUp(80)}, 0, 609, catalog.OutcomeReview},
		"denied: the recording's unknown":    {[]meta.MatchCandidate{ok, runnerUp(80)}, book, 0, catalog.OutcomeReview},
		"denied: score 89":                   {[]meta.MatchCandidate{withScore(89), runnerUp(70)}, book, 609, catalog.OutcomeReview},
		"denied: lead 9":                     {[]meta.MatchCandidate{ok, runnerUp(83)}, book, 609, catalog.OutcomeReview},
		"denied: no reasons (older service)": {[]meta.MatchCandidate{noReasons, runnerUp(80)}, book, 609, catalog.OutcomeReview},
	} {
		p := proposal(tc.cands[0])
		p.RuntimeMin = tc.runtime
		if p.Values[catalog.FieldASIN] != "" || p.Values[catalog.FieldISBN] != "" {
			t.Fatalf("%s: the listing proposes an identifier: %v", name, p.Values)
		}
		if got, _, _ := Classify(tc.cands, p, tc.seconds, false); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
	// An identified match keeps its own rule: 5% off is within PlausibleRuntimeOff,
	// and its series and author reasons are not asked for.
	ids := catalog.MatchProposal{Values: map[string]string{"asin": "B0UK000001"}, RuntimeMin: 630}
	if got, _, _ := Classify([]meta.MatchCandidate{withReasons("surname", "none"), runnerUp(80)}, ids, book, false); got != catalog.OutcomeAuto {
		t.Errorf("identified, 5%% off: %s, want auto", got)
	}
}

// TestPlanRecordsTheAnchor: every scope records the proposal's work and recording
// when the book has none or another one, nothing when it has this one; a refresh
// only heals an anchor the book has, never adds one.
func TestPlanRecordsTheAnchor(t *testing.T) {
	t.Parallel()
	c := listing("the-martian", 95, 634)
	p := Propose(&c, &c.Recordings[0])
	for _, scope := range Scopes {
		st := state(nil, nil, false)
		w := Plan(scope, st, p)
		if w.Anchor == nil || w.Anchor.WorkID != "the-martian" || w.Anchor.RecordingID != "bray" || w.Empty() {
			t.Errorf("%s: anchor = %+v", scope, w.Anchor)
		}
		st.Anchor = catalog.MatchAnchor{WorkID: "the-martian", RecordingID: "bray", How: catalog.MatchHowAuto}
		if w := Plan(scope, st, p); w.Anchor != nil {
			t.Errorf("%s: the same anchor again = %+v", scope, w.Anchor)
		}
		st.Anchor.RecordingID = "other"
		if w := Plan(scope, st, p); w.Anchor == nil {
			t.Errorf("%s: another recording planned no anchor", scope)
		}
	}
	if w := Plan(ScopeIDs, state(nil, nil, false), p); len(w.Set) != 0 || w.Cover || w.Empty() || !changeOf(state(nil, nil, false), w).Match {
		t.Errorf("ids on a listing = %+v, want the anchor alone, shown as a match", w)
	}
	if w := planRefresh(ScopeFill, state(nil, nil, false), p); w.Anchor != nil {
		t.Errorf("a refresh added an anchor: %+v", w.Anchor)
	}
	st := state(nil, nil, false)
	st.Anchor = catalog.MatchAnchor{WorkID: "old-slug", How: catalog.MatchHowAccepted}
	if w := planRefresh(ScopeFill, st, p); w.Anchor == nil || w.Anchor.WorkID != "the-martian" {
		t.Errorf("a refresh did not heal a moved anchor: %+v", w.Anchor)
	}
}

// anchor records a community match with no identifier on path, as the match
// dialog's accept does.
func (e *env) anchor(t *testing.T, path, work, recording string) {
	t.Helper()
	if err := e.cat.EditBook(context.Background(), e.lib, path, catalog.BookEdit{
		Match: &catalog.MatchAnchor{WorkID: work, RecordingID: recording}, Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) anchorOf(t *testing.T, path string) catalog.MatchAnchor {
	t.Helper()
	st, err := e.cat.BookMatchState(context.Background(), e.lib, path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Anchor
}

// TestRunAppliesAStoreListing (server#320): a store listing that fits is applied
// unasked and the book is matched by its recorded work (how auto, the run's id),
// even under ids, which has no identifier to write; a listing held for review and
// included is recorded as accepted. Both leave Not matched, and the next run
// doesn't offer them.
func TestRunAppliesAStoreListing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Fits", "Fits", "", nil)
	e.book(t, "A/Off", "Off", "", nil)
	e.matcher.byTitle["Fits"] = []meta.MatchCandidate{listing("the-martian", 95, 634)}
	e.matcher.byTitle["Off"] = []meta.MatchCandidate{listing("artemis", 95, 600)} // 5% off: review

	run := e.start(t, catalog.MatchModeMatch, "")
	auto := e.items(t, run, catalog.OutcomeAuto)
	if len(auto) != 1 || auto[0].Path != "A/Fits" {
		t.Fatalf("auto = %+v", auto)
	}
	if ch := auto[0].Changes[ScopeIDs]; !ch.Match || len(ch.Fields) != 0 || ch.Cover {
		t.Fatalf("ids change = %+v, want the match alone", ch)
	}
	review := e.items(t, run, catalog.OutcomeReview)
	if len(review) != 1 || review[0].Path != "A/Off" {
		t.Fatalf("review = %+v", review)
	}
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeIDs, Include: []int64{review[0].ID}, UserID: e.admin}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	if run, _ = e.cat.GetMatchRun(ctx, run.ID); run.Counts.Applied != 2 {
		t.Fatalf("applied run = %+v", run)
	}
	if a := e.anchorOf(t, "A/Fits"); a.WorkID != "the-martian" || a.RecordingID != "bray" || a.How != catalog.MatchHowAuto {
		t.Errorf("fits anchor = %+v", a)
	}
	if a := e.anchorOf(t, "A/Off"); a.WorkID != "artemis" || a.How != catalog.MatchHowAccepted {
		t.Errorf("off anchor = %+v", a)
	}
	var runID int64
	if err := e.db.QueryRowContext(ctx, `SELECT run_id FROM book_matches WHERE path = 'A/Fits'`).Scan(&runID); err != nil || runID != run.ID {
		t.Errorf("run_id = %d %v, want %d", runID, err, run.ID)
	}
	// Matched now: the items read as applied, not matched since, and the next run
	// has nothing to offer.
	for _, it := range e.items(t, run, "") {
		if it.MatchedSince || it.Applied != catalog.ItemApplied {
			t.Errorf("%s = %+v", it.Path, it)
		}
	}
	if next := e.start(t, catalog.MatchModeMatch, ""); next.Total != 0 {
		t.Errorf("next run total = %d, want 0", next.Total)
	}
}

// TestApplyLeavesABookAnchoredSince: a run's book matched since by a store
// listing (no identifier, just the recorded work) is matched since: no changes,
// and the apply skips it.
func TestApplyLeavesABookAnchoredSince(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/One", "One", "", nil)
	e.matcher.byTitle["One"] = []meta.MatchCandidate{cand("the-martian", 97, "")}
	run := e.start(t, catalog.MatchModeMatch, "")
	e.anchor(t, "A/One", "by-hand", "")
	items := e.items(t, run, catalog.OutcomeAuto)
	if len(items) != 1 || !items[0].MatchedSince || items[0].Changes[ScopeOverwrite].Match {
		t.Fatalf("items = %+v", items)
	}
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeOverwrite}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	if it := e.items(t, run, catalog.OutcomeAuto)[0]; it.Applied != catalog.ItemSkipped || it.Detail != "matched_since" {
		t.Fatalf("item = %+v", it)
	}
	if a := e.anchorOf(t, "A/One"); a.WorkID != "by-hand" {
		t.Fatalf("anchor = %+v, want the hand match kept", a)
	}
}

// TestRefreshByAnchor: a book matched with no identifier is refreshed by its
// recorded work: a work merged since (the lookup followed its redirect) is a
// change by itself, whose apply heals the anchor and keeps how it was made; an
// ASIN the recording gained fills in. A work the community no longer knows is no
// match, detail work_unknown, and the anchor stays. A book matched by its own
// ASIN is never given an anchor by a refresh, but one it has is healed.
func TestRefreshByAnchor(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Merged", "Merged", "", nil)
	e.book(t, "A/Gone", "Gone", "", nil)
	e.book(t, "A/Tagged", "Tagged", "B0US000001", nil)
	e.book(t, "A/TaggedAnchored", "Tagged Anchored", "B0US000002", nil)
	e.anchor(t, "A/Merged", "old-slug", "bray")
	e.anchor(t, "A/Gone", "dropped", "")
	e.anchor(t, "A/TaggedAnchored", "old-tagged", "")
	merged := cand("new-slug", 100, "")
	merged.Recordings[0].ASINs = []string{"B0US000009"}
	merged.Recordings[0].ASINRefs = []meta.ASINRef{{Region: "us", ASIN: "B0US000009"}}
	merged.Recordings[0].ISBNs = nil
	e.matcher.byWork["old-slug"] = []meta.MatchCandidate{merged}
	tagged := cand("the-martian", 100, "")
	tagged.RecordingID = "bray"
	e.matcher.byASIN["B0US000001"] = []meta.MatchCandidate{tagged}
	e.matcher.byASIN["B0US000002"] = []meta.MatchCandidate{tagged}

	run := e.start(t, catalog.MatchModeRefresh, "us")
	if run.Total != 4 {
		t.Fatalf("refresh total = %d, want the 4 matched books", run.Total)
	}
	for _, q := range e.matcher.queries {
		if q.WorkID != "" && (q.ASIN != "" || q.ISBN != "" || q.Title != "" || (q.WorkID == "old-slug" && q.RecordingID != "bray")) {
			t.Fatalf("work query = %+v, want the work and recording alone", q)
		}
	}
	none := e.items(t, run, catalog.OutcomeNone)
	if len(none) != 1 || none[0].Path != "A/Gone" || none[0].Detail != DetailWorkUnknown {
		t.Fatalf("none = %+v", none)
	}
	byPath := map[string]ItemView{}
	for _, it := range e.items(t, run, catalog.OutcomeAuto) {
		byPath[it.Path] = it
	}
	m, ok := byPath["A/Merged"]
	if !ok || m.Proposal.FoundBy != "work:old-slug" || !m.Changes[ScopeFill].Match ||
		!slices.Contains(m.Changes[ScopeFill].Fields, catalog.FieldASIN) {
		t.Fatalf("merged item = %+v", m)
	}
	if it, ok := byPath["A/Tagged"]; ok && it.Changes[ScopeFill].Match {
		t.Fatalf("a refresh offered an anchor to a book matched by its ASIN: %+v", it)
	}
	if it := byPath["A/TaggedAnchored"]; !it.Changes[ScopeFill].Match {
		t.Fatalf("tagged anchored item = %+v, want its anchor healed", it)
	}

	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill, UserID: e.admin}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	if a := e.anchorOf(t, "A/Merged"); a.WorkID != "new-slug" || a.How != catalog.MatchHowAccepted {
		t.Errorf("merged anchor = %+v, want healed, still accepted", a)
	}
	st, _ := e.cat.BookMatchState(ctx, e.lib, "A/Merged")
	if st.Fields[catalog.FieldASIN].Value != "B0US000009" {
		t.Errorf("merged asin = %+v, want the gained one filled", st.Fields[catalog.FieldASIN])
	}
	if a := e.anchorOf(t, "A/Gone"); a.WorkID != "dropped" {
		t.Errorf("gone anchor = %+v, want kept", a)
	}
	if a := e.anchorOf(t, "A/Tagged"); a.WorkID != "" {
		t.Errorf("tagged anchor = %+v, want none", a)
	}
	if a := e.anchorOf(t, "A/TaggedAnchored"); a.WorkID != "the-martian" {
		t.Errorf("tagged anchored = %+v, want healed", a)
	}
}

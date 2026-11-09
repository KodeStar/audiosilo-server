package matchrun

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	// Unlinked (the console's Unlink), the book is unmatched again: the next run
	// asks about it, and may well find the same work.
	if err := e.cat.EditBook(ctx, e.lib, "A/Fits", catalog.BookEdit{Match: &catalog.MatchAnchor{}}); err != nil {
		t.Fatal(err)
	}
	again := e.start(t, catalog.MatchModeMatch, "")
	if again.Total != 1 {
		t.Errorf("run after the unlink total = %d, want 1", again.Total)
	}
	if auto := e.items(t, again, catalog.OutcomeAuto); len(auto) != 1 || auto[0].Path != "A/Fits" || !auto[0].Changes[ScopeIDs].Match {
		t.Errorf("run after the unlink = %+v, want the work proposed again", auto)
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
	if !ok || m.Proposal.FoundBy != "work:old-slug/bray" || !m.Changes[ScopeFill].Match ||
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

// workAnswer is the by-work answer for a refresh of a book whose recorded match is
// (work, recording): the work named, score 100, no reasons, the recording kept.
func workAnswer(work, recording string) meta.MatchCandidate {
	c := cand(work, 100, "")
	c.RecordingID, c.Reasons = recording, nil
	return c
}

func (e *env) field(t *testing.T, path, field string) catalog.FieldValue {
	t.Helper()
	st, err := e.cat.BookMatchState(context.Background(), e.lib, path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Fields[field]
}

// TestRefreshByWorkKeepsAnIdentifierGainedSince: a refresh found by a book's
// recorded match carries its recording's ASIN and ISBN, but a book that gained an
// identifier of its own between the run and the apply (the Bridge's backfill, an
// enrichment) keeps it: neither is written, under any scope, and the review
// offers none.
func TestRefreshByWorkKeepsAnIdentifierGainedSince(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/GainsASIN", "The Martian", "", nil)
	e.book(t, "A/GainsISBN", "The Martian", "", nil)
	e.anchor(t, "A/GainsASIN", "the-martian", "bray")
	e.anchor(t, "A/GainsISBN", "the-martian", "bray")
	e.matcher.byWork["the-martian"] = []meta.MatchCandidate{workAnswer("the-martian", "bray")}
	run := e.start(t, catalog.MatchModeRefresh, "")
	if run.Counts.Auto != 2 {
		t.Fatalf("run = %+v", run)
	}
	if err := e.cat.BackfillASIN(ctx, e.lib, "A/GainsASIN", "B0MINE0001"); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.SetEnrichment(ctx, e.lib, "A/GainsISBN", "", "9780000000002"); err != nil {
		t.Fatal(err)
	}
	for _, it := range e.items(t, run, catalog.OutcomeAuto) {
		for scope, ch := range it.Changes {
			if slices.Contains(ch.Fields, catalog.FieldASIN) || slices.Contains(ch.Fields, catalog.FieldISBN) {
				t.Fatalf("%s %s change = %+v, want no identifier", it.Path, scope, ch)
			}
		}
	}
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeOverwrite, UserID: e.admin}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	if a, i := e.field(t, "A/GainsASIN", catalog.FieldASIN), e.field(t, "A/GainsASIN", catalog.FieldISBN); a.Value != "B0MINE0001" || i.Value != "" {
		t.Errorf("gains ASIN: asin %+v isbn %+v, want its own ASIN alone", a, i)
	}
	if a, i := e.field(t, "A/GainsISBN", catalog.FieldASIN), e.field(t, "A/GainsISBN", catalog.FieldISBN); a.Value != "" || i.Value != "9780000000002" {
		t.Errorf("gains ISBN: asin %+v isbn %+v, want its own ISBN alone", a, i)
	}
	if n := e.field(t, "A/GainsASIN", catalog.FieldNarrator); n.Value != "R. C. Bray" {
		t.Errorf("narrator = %+v, want the details still refreshed", n)
	}
}

// TestRefreshByWorkLeavesABookMatchedAgain: a refresh found by a book's recorded
// match (its work and recording) is for a book that still has it: matched again by
// hand since to another recording of the same work, the book keeps that one, its
// narrator included, under any scope.
func TestRefreshByWorkLeavesABookMatchedAgain(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{ScopeFill, ScopeOverwrite} {
		t.Run(scope, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			ctx := context.Background()
			e.book(t, "A/Book", "The Martian", "", nil)
			e.anchor(t, "A/Book", "the-martian", "bray")
			c := workAnswer("the-martian", "bray")
			c.Recordings = append(c.Recordings, meta.MatchRecording{ID: "wheaton", RuntimeMin: 634,
				Narrators: []meta.MetaPersonRef{{Name: "Wil Wheaton"}}, Publisher: "Podium"})
			e.matcher.byWork["the-martian"] = []meta.MatchCandidate{c}
			run := e.start(t, catalog.MatchModeRefresh, "")
			auto := e.items(t, run, catalog.OutcomeAuto)
			if len(auto) != 1 || auto[0].Proposal.FoundBy != "work:the-martian/bray" {
				t.Fatalf("auto = %+v", auto)
			}
			if err := e.cat.EditBook(ctx, e.lib, "A/Book", catalog.BookEdit{
				Set:    map[string]string{catalog.FieldNarrator: "Wil Wheaton"},
				Match:  &catalog.MatchAnchor{WorkID: "the-martian", RecordingID: "wheaton"},
				Source: catalog.SourceCommunity, UserID: e.admin,
			}); err != nil {
				t.Fatal(err)
			}
			if ch := e.items(t, run, catalog.OutcomeAuto)[0].Changes[scope]; len(ch.Fields) != 0 || ch.Match || ch.Cover {
				t.Fatalf("change after the hand match = %+v, want none", ch)
			}
			if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: scope, UserID: e.admin}); err != nil {
				t.Fatal(err)
			}
			e.runner.Wait()
			if a := e.anchorOf(t, "A/Book"); a.RecordingID != "wheaton" {
				t.Errorf("anchor = %+v, want the hand match kept", a)
			}
			if n := e.field(t, "A/Book", catalog.FieldNarrator); n.Value != "Wil Wheaton" {
				t.Errorf("narrator = %+v, want the hand match's", n)
			}
		})
	}
}

// TestRefreshHealStoppedInItsCover: a refresh that heals a merged work, stopped
// while it fetched the cover, stays to apply, and the next apply takes the cover
// though the book's match is already the healed one.
func TestRefreshHealStoppedInItsCover(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Merged", "The Martian", "", no)
	e.anchor(t, "A/Merged", "old-slug", "bray")
	c := workAnswer("new-slug", "bray")
	c.Recordings[0].CoverURL = "https://c/rec.jpg"
	e.matcher.byWork["old-slug"] = []meta.MatchCandidate{c}
	run := e.start(t, catalog.MatchModeRefresh, "")
	save := e.runner.saveCover
	e.runner.saveCover = func(ctx context.Context, _ int64, _, _ string, _ int64, _ catalog.CoverExpect) error {
		_, _ = e.runner.Cancel(context.Background(), run.ID)
		return ctx.Err()
	}
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill, UserID: e.admin}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	it := e.items(t, run, catalog.OutcomeAuto)[0]
	if a := e.anchorOf(t, "A/Merged"); it.Applied != "" || a.WorkID != "new-slug" || !it.Changes[ScopeFill].Cover {
		t.Fatalf("after the stop: item %+v anchor %+v, want healed, the cover still to take", it, a)
	}
	e.runner.saveCover = save
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill, UserID: e.admin}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	if it = e.items(t, run, catalog.OutcomeAuto)[0]; it.Applied != catalog.ItemApplied || e.covers.Load() != 1 {
		t.Fatalf("item = %+v, covers %d; want applied with the cover taken", it.MatchRunItem, e.covers.Load())
	}
}

// TestStoppedMatchKeepsWhatTheAdminDidSince: a store listing's apply stopped in
// its cover (cover_pending: its fields and match went in) is applied again, but
// never over what the admin did since: a book matched again by hand to another
// work is matched since, under any scope; an Unlink stays: it took back the
// fields and cover with the match, so the item writes nothing, not even the
// cover the stop cut short.
func TestStoppedMatchKeepsWhatTheAdminDidSince(t *testing.T) {
	t.Parallel()
	for _, since := range []string{"rematched", "unlinked"} {
		t.Run(since, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			ctx := context.Background()
			e.book(t, "A/Store", "Store", "", no)
			l := listing("run-work", 95, 634)
			l.Recordings[0].CoverURL = "https://c/rec.jpg"
			e.matcher.byTitle["Store"] = []meta.MatchCandidate{l}
			run := e.start(t, catalog.MatchModeMatch, "")
			save := e.runner.saveCover
			e.runner.saveCover = func(ctx context.Context, _ int64, _, _ string, _ int64, _ catalog.CoverExpect) error {
				_, _ = e.runner.Cancel(context.Background(), run.ID)
				return ctx.Err()
			}
			if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill, UserID: e.admin}); err != nil {
				t.Fatal(err)
			}
			e.runner.Wait()
			if it := e.items(t, run, catalog.OutcomeAuto)[0]; it.Detail != DetailCoverPending || e.anchorOf(t, "A/Store").WorkID != "run-work" {
				t.Fatalf("after the stop: %+v", it.MatchRunItem)
			}
			e.runner.saveCover = save
			edit := catalog.BookEdit{Match: &catalog.MatchAnchor{}} // the book aside's Unlink
			if since == "rematched" {
				edit = catalog.BookEdit{Set: map[string]string{catalog.FieldTitle: "Hand Title"},
					Match: &catalog.MatchAnchor{WorkID: "hand-work", RecordingID: "hand-rec"}, Source: catalog.SourceCommunity, UserID: e.admin}
			}
			if err := e.cat.EditBook(ctx, e.lib, "A/Store", edit); err != nil {
				t.Fatal(err)
			}
			it := e.items(t, run, catalog.OutcomeAuto)[0]
			if it.MatchedSince != (since == "rematched") || it.Changes[ScopeIDs].Match || it.Changes[ScopeFill].Match {
				t.Fatalf("review = %+v", it)
			}
			scope := ScopeIDs
			if since == "unlinked" {
				scope = ScopeFill
			}
			if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: scope, UserID: e.admin}); err != nil {
				t.Fatal(err)
			}
			e.runner.Wait()
			a := e.anchorOf(t, "A/Store")
			it = e.items(t, run, catalog.OutcomeAuto)[0]
			switch since {
			case "rematched":
				if a.WorkID != "hand-work" || a.How != catalog.MatchHowAccepted || it.Detail != "matched_since" {
					t.Fatalf("anchor %+v item %+v, want the hand match kept", a, it.MatchRunItem)
				}
			case "unlinked":
				if a.WorkID != "" || it.Applied != catalog.ItemSkipped || e.covers.Load() != 0 {
					t.Fatalf("anchor %+v item %+v covers %d, want unlinked still, nothing written back", a, it.MatchRunItem, e.covers.Load())
				}
			}
		})
	}
}

// TestRefreshByWorkHoldsADramatization: through the real community client, a
// dramatization recording whose cast holds the book's narrator, 15% off the book's
// length, is held for review (length_differs) whether the refresh finds it by the
// book's ASIN, its recorded match or a fallback after an unknown ASIN. A known
// identifier keeps precedence even when its recording fails the length guard.
// The narrator in a dramatization's cast never widens a plain book's length
// guard (meta.DramatizesPlainBook).
func TestRefreshByWorkHoldsADramatization(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("asin") != "B0DRAMA001" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"work":{"id":"drama-work","title":"The Martian"},"recording_id":"rec"}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "drama-work" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":"drama-work","title":"The Martian","authors":[{"id":"a","name":"Andy Weir"}],
			"recordings":[{"id":"rec","runtime_min":540,"publisher":"Some Studio",
			  "narrators":[{"id":"w","name":"Wil Wheaton"},{"id":"fc","name":"Full Cast"}],
			  "asin":[{"region":"us","asin":"B0DRAMA001"}]}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	e := newEnv(t)
	ctx := context.Background()
	e.runner = New(e.cat, meta.NewService(srv.URL, nil), e.runner.saveCover, e.enabled.Load, nil)
	for _, b := range []catalog.Book{
		{LibraryID: e.lib, RelPath: "A/ByASIN", IsFolder: true, Title: "Martian", Author: "Andy Weir",
			Narrator: "Wil Wheaton", ASIN: "B0DRAMA001", Duration: 634 * 60, AddedAt: "2020-01-01"},
		{LibraryID: e.lib, RelPath: "A/ByWork", IsFolder: true, Title: "Martian", Author: "Andy Weir",
			Narrator: "Wil Wheaton", Duration: 634 * 60, AddedAt: "2020-01-01"},
		{LibraryID: e.lib, RelPath: "A/UnknownASIN", IsFolder: true, Title: "Martian", Author: "Andy Weir",
			Narrator: "Wil Wheaton", ASIN: "B0UNKNOWN1", Duration: 634 * 60, AddedAt: "2020-01-01"},
		{LibraryID: e.lib, RelPath: "A/KnownASINAnchored", IsFolder: true, Title: "Martian", Author: "Andy Weir",
			Narrator: "Wil Wheaton", ASIN: "B0DRAMA001", Duration: 634 * 60, AddedAt: "2020-01-01"},
	} {
		if _, err := e.cat.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	e.anchor(t, "A/ByWork", "drama-work", "rec")
	e.anchor(t, "A/UnknownASIN", "drama-work", "rec")
	e.anchor(t, "A/KnownASINAnchored", "unknown-work", "rec")
	run := e.start(t, catalog.MatchModeRefresh, "us")
	items := e.items(t, run, "")
	if len(items) != 4 {
		t.Fatalf("items = %+v", items)
	}
	for _, it := range items {
		if it.Outcome != catalog.OutcomeReview || it.Detail != DetailLengthDiffers || it.Proposal.RecordingID != "rec" {
			t.Errorf("%s = %+v, want the dramatization held for review", it.Path, it.MatchRunItem)
		}
	}
}

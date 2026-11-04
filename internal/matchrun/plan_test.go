package matchrun

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

var martian = meta.MatchCandidate{
	WorkID: "the-martian", Title: "The Martian", Authors: []meta.MetaPersonRef{{Name: "Andy Weir"}},
	FirstPublished: "2011-09-27", Description: "Stranded.", CoverURL: "https://c/work.jpg",
	WebURL: "https://meta/work?id=the-martian",
	Series: []meta.MatchSeries{{Name: "Mars", Position: "1"}},
	Recordings: []meta.MatchRecording{{
		ID: "bray", RuntimeMin: 634, Narrators: []meta.MetaPersonRef{{Name: "R. C. Bray"}},
		ASINs:    []string{"B0UK000001", "B0US000001"},
		ASINRefs: []meta.ASINRef{{Region: "uk", ASIN: "B0UK000001"}, {Region: "us", ASIN: "B0US000001"}}, ASINRegion: "uk",
		ISBNs: []string{"978-0553418026"}, CoverURL: "https://c/rec.jpg", ReleaseDate: "2013-03-22",
	}},
}

func TestPropose(t *testing.T) {
	p := Propose(&martian, &martian.Recordings[0])
	want := map[string]string{
		"title": "The Martian", "author": "Andy Weir", "narrator": "R. C. Bray", "series": "Mars",
		"series_index": "1", "published": "2011", "description": "Stranded.", "asin": "B0UK000001",
		"isbn": "9780553418026",
	}
	if !maps.Equal(p.Values, want) {
		t.Fatalf("values = %v, want %v", p.Values, want)
	}
	if p.ASINRegion != "uk" || p.CoverURL != "https://c/rec.jpg" || p.RecordingID != "bray" || p.WorkID != "the-martian" ||
		p.Narrators != "R. C. Bray" || p.RuntimeMin != 634 {
		t.Fatalf("proposal = %+v", p)
	}

	// No recording: the work's own values, its cover, the year from the work.
	p = Propose(&martian, nil)
	if p.Values["asin"] != "" || p.Values["narrator"] != "" || p.ASINRegion != "" || p.CoverURL != "https://c/work.jpg" ||
		p.Values["published"] != "2011" {
		t.Fatalf("no recording = %+v", p)
	}

	// A value an override would refuse is left out; the year falls back to the
	// recording's release.
	odd := martian
	odd.FirstPublished = ""
	odd.Series = []meta.MatchSeries{{Name: "Mars", Position: "one"}}
	p = Propose(&odd, &odd.Recordings[0])
	if _, ok := p.Values["series_index"]; ok || p.Values["published"] != "2013" {
		t.Fatalf("odd = %v", p.Values)
	}
}

// TestProposeEditionFields: the work's subtitle and language and the recording's
// publisher are offered; abridged only when the recording is marked so (the
// community's false is also "not recorded"). Fill fills them on a book without
// them, overwrite replaces a file's, and an admin's own stays either way.
func TestProposeEditionFields(t *testing.T) {
	t.Parallel()
	c := martian
	c.Subtitle, c.Language = "A Novel", "en"
	c.Recordings = slices.Clone(martian.Recordings)
	c.Recordings[0].Publisher = "Podium Publishing"
	p := Propose(&c, &c.Recordings[0])
	for field, want := range map[string]string{"subtitle": "A Novel", "language": "en", "publisher": "Podium Publishing"} {
		if p.Values[field] != want {
			t.Errorf("%s = %q, want %q", field, p.Values[field], want)
		}
	}
	if _, ok := p.Values["abridged"]; ok {
		t.Errorf("an unmarked recording offered abridged: %v", p.Values)
	}
	c.Recordings[0].Abridged = true
	if p = Propose(&c, &c.Recordings[0]); p.Values["abridged"] != "true" {
		t.Errorf("abridged = %q, want true", p.Values["abridged"])
	}
	if p := Propose(&c, nil); p.Values["publisher"] != "" || p.Values["subtitle"] != "A Novel" {
		t.Errorf("no recording: %v, want the work's subtitle and no publisher", p.Values)
	}

	st := state(map[string]string{"title": "The Martian", "language": "English", "publisher": "Mine"}, []string{"publisher"}, false)
	set, _ := planSet(ScopeFill, st, p)
	if set["subtitle"] != "A Novel" || set["abridged"] != "true" || set["language"] != "" || set["publisher"] != "" {
		t.Errorf("fill = %v, want the subtitle and abridged only", set)
	}
	set, _ = planSet(ScopeOverwrite, st, p)
	if set["language"] != "en" || set["publisher"] != "" {
		t.Errorf("overwrite = %v, want the language replaced and the admin's publisher kept", set)
	}
}

// TestProposeSeriesForTheRecording: the series proposed are the chosen
// recording's (an edition named for it first, as the community service's
// candidate lists them per recording); with no recording, the work's own.
func TestProposeSeriesForTheRecording(t *testing.T) {
	t.Parallel()
	hp := meta.MatchSeries{ID: "harry-potter", Name: "Harry Potter", Position: "2"}
	cast := meta.MatchSeries{ID: "hp-full-cast", Name: "Harry Potter (Full-Cast Editions)", Position: "2"}
	c := meta.MatchCandidate{
		WorkID: "chamber", Title: "Harry Potter and the Chamber of Secrets", Series: []meta.MatchSeries{hp},
		Recordings: []meta.MatchRecording{{ID: "dale", Series: []meta.MatchSeries{hp}}, {ID: "cast", Series: []meta.MatchSeries{cast, hp}}},
	}
	for _, tc := range []struct {
		rec              *meta.MatchRecording
		series, moreJSON string
	}{
		{&c.Recordings[0], "Harry Potter", ""},
		{nil, "Harry Potter", ""},
		{&c.Recordings[1], "Harry Potter (Full-Cast Editions)", `[{"name":"Harry Potter","position":2}]`},
	} {
		p := Propose(&c, tc.rec)
		if p.Values["series"] != tc.series || p.Values["series_index"] != "2" || p.Values["more_series"] != tc.moreJSON {
			t.Errorf("recording %v: values = %v, want series %q more %q", tc.rec, p.Values, tc.series, tc.moreJSON)
		}
	}
}

// planSet is Plan's fields to set and its cover, for the tests that write no
// revert.
func planSet(scope string, st *catalog.MatchState, p catalog.MatchProposal) (map[string]string, bool) {
	w := Plan(scope, st, p)
	return w.Set, w.Cover
}

// state is a book as Plan sees it: field -> value (source tag), with overrides.
func state(values map[string]string, edited []string, coverMissing bool) *catalog.MatchState {
	st := &catalog.MatchState{Fields: map[string]catalog.FieldValue{}, CoverMissing: coverMissing}
	for _, f := range catalog.OverrideFields {
		fv := catalog.FieldValue{Value: values[f]}
		if fv.Value != "" {
			fv.Source = catalog.SourceTag
		}
		if slices.Contains(edited, f) {
			fv.Source, fv.Locked = catalog.SourceEdited, true
		}
		st.Fields[f] = fv
	}
	return st
}

func keys(m map[string]string) []string {
	out := slices.Collect(maps.Keys(m))
	slices.Sort(out)
	return out
}

func TestPlan(t *testing.T) {
	p := Propose(&martian, &martian.Recordings[0])
	// A typical unmatched book: a title and an author from the tags (the title
	// with fluff), a narrator the admin typed themselves, nothing else.
	book := map[string]string{"title": "The Martian (Unabridged)", "author": "Andy Weir", "narrator": "Bray"}
	st := state(book, []string{"narrator"}, true)

	for _, tc := range []struct {
		scope string
		want  []string
		cover bool
	}{
		{ScopeIDs, []string{"asin", "isbn"}, false},
		// Fill tidies the cluttered tag title (tidyTitle).
		{ScopeFill, []string{"asin", "description", "isbn", "published", "series", "series_index", "title"}, true},
		// Overwrite replaces the tag title too, never the admin's narrator.
		{ScopeOverwrite, []string{"asin", "description", "isbn", "published", "series", "series_index", "title"}, true},
	} {
		set, cover := planSet(tc.scope, st, p)
		if got := keys(set); !slices.Equal(got, tc.want) || cover != tc.cover {
			t.Errorf("%s: set %v cover %v, want %v %v", tc.scope, got, cover, tc.want, tc.cover)
		}
	}

	// A book with a cover keeps it; a value the book already has isn't "set".
	st = state(map[string]string{"title": "The Martian", "series": "Mars"}, nil, false)
	set, cover := planSet(ScopeFill, st, p)
	if cover || set["title"] != "" || set["series"] != "" || set["series_index"] != "1" {
		t.Fatalf("fill over a cover = %v %v", set, cover)
	}

	// A position never lands beside another series: the book's own "Red Planet"
	// (kept by fill) has no place for Mars's "1".
	st = state(map[string]string{"title": "The Martian", "series": "Red Planet"}, nil, false)
	set, _ = planSet(ScopeFill, st, p)
	if _, ok := set["series_index"]; ok {
		t.Fatalf("fill put Mars #1 beside Red Planet: %v", set)
	}
	// ...but overwrite, which replaces the series too, sets both.
	set, _ = planSet(ScopeOverwrite, st, p)
	if set["series"] != "Mars" || set["series_index"] != "1" {
		t.Fatalf("overwrite = %v", set)
	}
	// An admin-edited series stays, and so the position stays off it.
	st = state(map[string]string{"title": "The Martian", "series": "Red Planet"}, []string{"series"}, false)
	set, _ = planSet(ScopeOverwrite, st, p)
	if _, ok := set["series_index"]; ok || set["series"] != "" {
		t.Fatalf("overwrite beside an edited series = %v", set)
	}

	// No cover on offer, no cover taken.
	bare := p
	bare.CoverURL = ""
	if _, cover := planSet(ScopeFill, state(nil, nil, true), bare); cover {
		t.Fatal("took a cover there is none of")
	}
}

func TestPlanRepick(t *testing.T) {
	p := catalog.MatchProposal{Values: map[string]string{"asin": "B0UK000001"}}
	community := &catalog.MatchState{Fields: map[string]catalog.FieldValue{
		"asin": {Value: "B0US000001", Source: catalog.SourceCommunity, Locked: true},
	}}
	if got := PlanRepick(community, p); got["asin"] != "B0UK000001" {
		t.Fatalf("repick = %v", got)
	}
	for name, fv := range map[string]catalog.FieldValue{
		"admin's own":    {Value: "B0US000001", Source: catalog.SourceEdited, Locked: true},
		"from the tags":  {Value: "B0US000001", Source: catalog.SourceTag},
		"enrichment":     {Value: "B0US000001", Source: catalog.SourceCommunity},
		"already chosen": {Value: "B0UK000001", Source: catalog.SourceCommunity, Locked: true},
	} {
		st := &catalog.MatchState{Fields: map[string]catalog.FieldValue{"asin": fv}}
		if got := PlanRepick(st, p); len(got) != 0 {
			t.Errorf("%s: repick = %v, want nothing", name, got)
		}
	}

	// The run looked B0US000001 up: applied while the book still has it, but a
	// hand match since (another record, still a community override) keeps its
	// own ASIN (R-34).
	found := p
	found.FoundBy = "asin:B0US000001"
	if got := PlanRepick(community, found); got["asin"] != "B0UK000001" {
		t.Errorf("repick of the ASIN it looked up = %v", got)
	}
	handMatched := &catalog.MatchState{Fields: map[string]catalog.FieldValue{
		"asin": {Value: "B0US000999", Source: catalog.SourceCommunity, Locked: true},
	}}
	if got := PlanRepick(handMatched, found); len(got) != 0 {
		t.Errorf("repick over a hand match = %v, want nothing", got)
	}
	// An item recorded before FoundBy is judged by the source alone, as before.
	if got := PlanRepick(handMatched, p); got["asin"] != "B0UK000001" {
		t.Errorf("repick of an older item = %v", got)
	}
}

func TestClassify(t *testing.T) {
	ids := catalog.MatchProposal{Values: map[string]string{"asin": "B0UK000001"}}
	withRuntime := func(minutes int) catalog.MatchProposal {
		return catalog.MatchProposal{Values: ids.Values, RuntimeMin: minutes}
	}
	cands := func(scores ...int) []meta.MatchCandidate {
		out := make([]meta.MatchCandidate, len(scores))
		for i, s := range scores {
			out[i].Score = s
		}
		return out
	}
	for name, tc := range map[string]struct {
		cands []meta.MatchCandidate
		p     catalog.MatchProposal
		want  string
	}{
		"confident":         {cands(91), ids, catalog.OutcomeAuto},
		"confident, a lead": {cands(95, 85), ids, catalog.OutcomeAuto},
		"too close":         {cands(95, 86), ids, catalog.OutcomeReview},
		"too low":           {cands(89), ids, catalog.OutcomeReview},
		"no identifier":     {cands(100), catalog.MatchProposal{Values: map[string]string{"title": "x"}}, catalog.OutcomeReview},
		"nothing":           {nil, ids, catalog.OutcomeNone},
		// The unabridged The Burning (7h 13m) against a work holding only its
		// dramatization (357 min, 18% short), as metaserve scored it (#129).
		"another edition's length": {cands(90), withRuntime(357), catalog.OutcomeReview},
		"a length within 10%":      {cands(90), withRuntime(400), catalog.OutcomeAuto},
		"a recording of no length": {cands(90), withRuntime(0), catalog.OutcomeAuto},
	} {
		if got, _, _ := Classify(tc.cands, tc.p, 7*3600+13*60); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
	if _, score, runnerUp := Classify(cands(95, 85), ids, 0); score != 95 || runnerUp != 85 {
		t.Fatalf("scores = %d %d", score, runnerUp)
	}
	// Below ReviewFloor metaserve's best candidate is no match, its scores kept.
	scored := func(scores ...int) []meta.MatchCandidate {
		out := cands(scores...)
		for i := range out {
			out[i].Reasons = &meta.MatchReasons{}
		}
		return out
	}
	if got, score, runnerUp := Classify(scored(59, 40), ids, 0); got != catalog.OutcomeNone || score != 59 || runnerUp != 40 {
		t.Fatalf("a weak match: %s %d %d", got, score, runnerUp)
	}
	if got, _, _ := Classify(scored(ReviewFloor), ids, 0); got != catalog.OutcomeReview {
		t.Fatalf("a match at the floor: %s, want review", got)
	}
	// An older metaserve's search scores on its own scale: the floor says nothing.
	if got, _, _ := Classify(cands(59, 40), ids, 0); got != catalog.OutcomeReview {
		t.Fatalf("a fallback's 59: %s, want review", got)
	}
	// A book of unknown length isn't held back by the recording's.
	if got, _, _ := Classify(cands(90), withRuntime(357), 0); got != catalog.OutcomeAuto {
		t.Fatalf("an unknown length: %s", got)
	}
}

// TestPlanSeries: a work in several series (Guards! Guards! is Discworld #8 and
// City Watch #1). The book keeps one main series and gets the others as
// more_series; its position is the community's in the series it keeps.
func TestPlanSeries(t *testing.T) {
	const watch = "Discworld: Ankh-Morpork City Watch"
	guards := meta.MatchCandidate{
		WorkID: "guards-guards", Title: "Guards! Guards!",
		Series: []meta.MatchSeries{{Name: "Discworld", Position: "8"}, {Name: watch, Position: "1"}, {Name: "Omnibus", Position: "1-3"}},
	}
	p := Propose(&guards, nil)
	if got := p.Values["more_series"]; got != `[{"name":"`+watch+`","position":1},{"name":"Omnibus","position":1,"position_end":3}]` {
		t.Fatalf("proposed more_series = %s", got)
	}

	// No series yet: the work's main one, and the rest beside it.
	set, _ := planSet(ScopeFill, state(map[string]string{"title": "Guards! Guards!"}, nil, false), p)
	if set["series"] != "Discworld" || set["series_index"] != "8" ||
		set["more_series"] != `[{"name":"`+watch+`","position":1},{"name":"Omnibus","position":1,"position_end":3}]` {
		t.Fatalf("fill on no series = %v", set)
	}

	// Filed under City Watch by its folder: fill keeps that, numbers it as City
	// Watch numbers it (not Discworld's 8), and adds Discworld beside it.
	st := state(map[string]string{"title": "Guards! Guards!", "series": watch}, nil, false)
	set, _ = planSet(ScopeFill, st, p)
	if _, ok := set["series"]; ok || set["series_index"] != "1" ||
		set["more_series"] != `[{"name":"Discworld","position":8},{"name":"Omnibus","position":1,"position_end":3}]` {
		t.Fatalf("fill on City Watch = %v", set)
	}

	// Overwrite keeps it too (the work is in City Watch, as the match dialog
	// offers it), numbered there.
	st = state(map[string]string{"title": "Guards! Guards!", "series": watch, "series_index": "8"}, nil, false)
	if set, _ := planSet(ScopeOverwrite, st, p); set["series"] != "" || set["series_index"] != "1" ||
		set["more_series"] != `[{"name":"Discworld","position":8},{"name":"Omnibus","position":1,"position_end":3}]` {
		t.Fatalf("overwrite on City Watch = %v", set)
	}

	// A work listed twice in one series (an omnibus counted again) takes its
	// first position there, and the series isn't listed again beside it.
	twice := Propose(&meta.MatchCandidate{WorkID: "gg", Title: "Guards! Guards!",
		Series: []meta.MatchSeries{{Name: "Discworld", Position: "8"}, {Name: "Discworld", Position: "8.5"}}}, nil)
	st = state(map[string]string{"title": "Guards! Guards!", "series": "Discworld"}, nil, false)
	if set, _ := planSet(ScopeFill, st, twice); set["series_index"] != "8" || set["more_series"] != "" {
		t.Fatalf("fill from a work listed twice = %v", set)
	}

	// The admin's own City Watch stays under overwrite, and its position becomes
	// City Watch's, though the tags' 8 happens to be Discworld's.
	st = state(map[string]string{"title": "Guards! Guards!", "series": watch, "series_index": "8"}, []string{"series"}, false)
	if set, _ := planSet(ScopeOverwrite, st, p); set["series_index"] != "1" || set["series"] != "" {
		t.Fatalf("overwrite beside an edited City Watch = %v", set)
	}

	// Filed under a series the community doesn't list ("Watch" for City Watch,
	// say): most likely the main one spelled otherwise, so only the work's other
	// series go beside it.
	st = state(map[string]string{"title": "Guards! Guards!", "series": "Pratchett Discworld"}, nil, false)
	if set, _ := planSet(ScopeFill, st, p); set["more_series"] != `[{"name":"`+watch+`","position":1},{"name":"Omnibus","position":1,"position_end":3}]` {
		t.Fatalf("fill beside an unlisted series = %v", set)
	}

	// The admin's own list stays, under any scope.
	st = state(map[string]string{"title": "Guards! Guards!", "more_series": `[{"name":"Mine","position":2}]`}, []string{"more_series"}, false)
	if set, _ := planSet(ScopeOverwrite, st, p); set["more_series"] != "" {
		t.Fatalf("overwrite replaced the admin's list: %v", set)
	}
	// A list the book already has isn't "set".
	st = state(map[string]string{"title": "Guards! Guards!", "series": "Discworld", "series_index": "8",
		"more_series": `[{"name":"` + watch + `","position":1},{"name":"Omnibus","position":1,"position_end":3}]`}, nil, false)
	if set, _ := planSet(ScopeOverwrite, st, p); len(set) != 0 {
		t.Fatalf("overwrite on a book that has it all = %v", set)
	}
}

// polluted is a book an earlier match filed under every series of its work: a
// full-cast edition as its main series and a narrator's edition beside it, all
// written by the community (scanned: the path's "Harry Potter").
func polluted(edited ...string) *catalog.MatchState {
	st := state(nil, nil, false)
	set := func(field, value, scanned string) {
		fv := catalog.FieldValue{Value: value, Source: catalog.SourceCommunity, Scanned: scanned, Locked: true}
		if slices.Contains(edited, field) {
			fv.Source = catalog.SourceEdited
		}
		st.Fields[field] = fv
	}
	set("series", "Harry Potter (Full-Cast Editions)", "Harry Potter")
	set("series_index", "2", "")
	set("more_series", `[{"name":"Harry Potter (Narrated by Stephen Fry)","position":2}]`, "")
	return st
}

// TestPlanOverwriteReplacesTheSeries: overwrite REPLACES a book's series with the
// community's current ones (here: Harry Potter alone, the editions left out by
// the series rules), reverting a series value the community wrote that it no
// longer offers; fill and ids change nothing a book has. An admin's own series
// stay under every scope.
func TestPlanOverwriteReplacesTheSeries(t *testing.T) {
	t.Parallel()
	p := catalog.MatchProposal{Values: map[string]string{"series": "Harry Potter", "series_index": "2"}}

	w := Plan(ScopeOverwrite, polluted(), p)
	if !maps.Equal(w.Set, map[string]string{"series": "Harry Potter"}) || !slices.Equal(w.Revert, []string{"more_series"}) {
		t.Fatalf("overwrite = set %v revert %v, want Harry Potter set and the edition reverted", w.Set, w.Revert)
	}
	for _, scope := range []string{ScopeFill, ScopeIDs} {
		if w := Plan(scope, polluted(), p); !w.Empty() {
			t.Errorf("%s = %+v, want nothing (the book has series)", scope, w)
		}
	}
	// The admin's own main series stays; the community's other series still go.
	w = Plan(ScopeOverwrite, polluted("series"), p)
	if _, ok := w.Set["series"]; ok || !slices.Equal(w.Revert, []string{"more_series"}) {
		t.Fatalf("edited series = set %v revert %v", w.Set, w.Revert)
	}
	// The admin's own other series stay too.
	if w := Plan(ScopeOverwrite, polluted("more_series"), p); slices.Contains(w.Revert, "more_series") {
		t.Fatalf("edited more_series reverted: %v", w.Revert)
	}

	// The community lists no series for the book any more: every series value it
	// wrote goes, back to the files' ("Harry Potter" from the path).
	w = Plan(ScopeOverwrite, polluted(), catalog.MatchProposal{Values: map[string]string{}})
	if len(w.Set) != 0 || !slices.Equal(w.Revert, []string{"series", "series_index", "more_series"}) {
		t.Fatalf("no community series = set %v revert %v", w.Set, w.Revert)
	}
	// A series from the files is never reverted (there is no override to drop).
	if w := Plan(ScopeOverwrite, state(map[string]string{"series": "Discworld"}, nil, false), catalog.MatchProposal{Values: map[string]string{}}); !w.Empty() {
		t.Fatalf("tag series = %+v, want nothing", w)
	}
}

// TestPlanOverwriteRevertsOnlyWhatItCan: a position the community no longer
// gives goes back to the files' only beside the series the files number (never
// the tags' Red Planet #3 beside Mars), and a list of other series the community
// offers that can't be stored takes none away.
func TestPlanOverwriteRevertsOnlyWhatItCan(t *testing.T) {
	t.Parallel()
	community := func(scannedSeries string) *catalog.MatchState {
		st := state(nil, nil, false)
		st.Fields["series"] = catalog.FieldValue{Value: "Mars (Full-Cast Editions)", Source: catalog.SourceCommunity, Scanned: scannedSeries, Locked: true}
		st.Fields["series_index"] = catalog.FieldValue{Value: "1", Source: catalog.SourceCommunity, Scanned: "3", Locked: true}
		return st
	}
	unnumbered := catalog.MatchProposal{Values: map[string]string{"series": "Mars"}}
	w := Plan(ScopeOverwrite, community("Red Planet"), unnumbered)
	if w.Set["series"] != "Mars" || slices.Contains(w.Revert, "series_index") {
		t.Fatalf("beside another series = set %v revert %v, want Mars set and the position kept", w.Set, w.Revert)
	}
	if w := Plan(ScopeOverwrite, community("Mars"), unnumbered); !slices.Contains(w.Revert, "series_index") {
		t.Fatalf("beside the files' series = revert %v, want the files' position back", w.Revert)
	}

	st := state(map[string]string{"series": "Main"}, nil, false)
	st.Fields["more_series"] = catalog.FieldValue{Value: `[{"name":"Old","position":1}]`, Source: catalog.SourceCommunity, Locked: true}
	tooLong := catalog.MatchProposal{Values: map[string]string{"series": "Main",
		"more_series": `[{"name":"` + strings.Repeat("x", 5000) + `","position":1}]`}}
	if w := Plan(ScopeOverwrite, st, tooLong); slices.Contains(w.Revert, "more_series") {
		t.Fatalf("a list that can't be stored = revert %v, want the book's kept", w.Revert)
	}
}

func TestLengthFits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		runtimeMin int
		seconds    float64
		want       bool
	}{
		{634, 634 * 60, true},
		{600, 634 * 60, true},  // 5% short
		{357, 433 * 60, false}, // the dramatization against the unabridged book
		{0, 634 * 60, true},    // the recording states no runtime
		{634, 0, true},         // the book's length is unknown
	} {
		if got := lengthFits(catalog.MatchProposal{RuntimeMin: tc.runtimeMin}, tc.seconds); got != tc.want {
			t.Errorf("lengthFits(%d min, %v s) = %v, want %v", tc.runtimeMin, tc.seconds, got, tc.want)
		}
	}
}

// TestPlanSeriesRange: a community position that is a range (an omnibus, "1-3")
// is written as the range, the main series' position or another series' entry,
// and the review shows it with its end.
func TestPlanSeriesRange(t *testing.T) {
	omnibus := meta.MatchCandidate{
		WorkID: "saga-1-3", Title: "Saga: Books 1-3",
		Series: []meta.MatchSeries{{Name: "Saga", Position: "1-3"}, {Name: "Box Sets", Position: "2 - 4"}, {Name: "Huge", Position: "1-500"}},
	}
	p := Propose(&omnibus, nil)
	if p.Values["series_index"] != "1-3" ||
		p.Values["more_series"] != `[{"name":"Box Sets","position":2,"position_end":4},{"name":"Huge","position":0}]` {
		t.Fatalf("proposed = %v", p.Values)
	}
	st := state(map[string]string{"title": "Saga Omnibus"}, nil, false)
	w := Plan(ScopeFill, st, p)
	if w.Set["series"] != "Saga" || w.Set["series_index"] != "1-3" {
		t.Fatalf("fill = %v", w.Set)
	}
	ch := changeOf(st, w)
	if ch.Series == nil || ch.Series.To[0] != (catalog.SeriesRef{Name: "Saga", Position: 1, PositionEnd: 3}) {
		t.Fatalf("review = %+v", ch.Series)
	}
	// Filed under Box Sets already: its position there is the range 2-4.
	st = state(map[string]string{"title": "Saga Omnibus", "series": "Box Sets"}, nil, false)
	if set, _ := planSet(ScopeFill, st, p); set["series_index"] != "2-4" ||
		set["more_series"] != `[{"name":"Saga","position":1,"position_end":3},{"name":"Huge","position":0}]` {
		t.Fatalf("fill under Box Sets = %v", set)
	}
}

// TestClassifyIdentifierHit: the ASIN or ISBN the query sent finding a work
// makes it the book whatever the runner-up scores (Dune found by its ASIN beside
// a 95 for another Dune), still within the length gate and with an identifier
// to attach; a runner-up found by the identifier too would be a duplicate in the
// community data, held for review.
func TestClassifyIdentifierHit(t *testing.T) {
	t.Parallel()
	ids := catalog.MatchProposal{Values: map[string]string{"asin": "B0UK000001"}}
	hit := func(score int, by string) meta.MatchCandidate {
		return meta.MatchCandidate{Score: score, Reasons: &meta.MatchReasons{Identifier: by}}
	}
	plain := func(score int) meta.MatchCandidate {
		return meta.MatchCandidate{Score: score, Reasons: &meta.MatchReasons{}}
	}
	for name, tc := range map[string]struct {
		cands []meta.MatchCandidate
		p     catalog.MatchProposal
		want  string
	}{
		"an identifier hit, a close runner-up":  {[]meta.MatchCandidate{hit(100, "asin"), plain(95)}, ids, catalog.OutcomeAuto},
		"an identifier hit alone":               {[]meta.MatchCandidate{hit(100, "isbn")}, ids, catalog.OutcomeAuto},
		"two identifier hits":                   {[]meta.MatchCandidate{hit(100, "asin"), hit(100, "asin")}, ids, catalog.OutcomeReview},
		"an identifier hit of another length":   {[]meta.MatchCandidate{hit(100, "asin"), plain(95)}, catalog.MatchProposal{Values: ids.Values, RuntimeMin: 300}, catalog.OutcomeReview},
		"an identifier hit with nothing to set": {[]meta.MatchCandidate{hit(100, "asin")}, catalog.MatchProposal{Values: map[string]string{}}, catalog.OutcomeReview},
		"no identifier, a close runner-up":      {[]meta.MatchCandidate{plain(100), plain(95)}, ids, catalog.OutcomeReview},
		"a hit behind a best with no reasons":   {[]meta.MatchCandidate{{Score: 100}, hit(100, "asin")}, ids, catalog.OutcomeReview},
		"a hit, a runner-up with no reasons":    {[]meta.MatchCandidate{hit(100, "asin"), {Score: 95}}, ids, catalog.OutcomeAuto},
	} {
		if got, _, _ := Classify(tc.cands, tc.p, 600*60); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

// TestTidyTitle: fill replaces a title only when the files add nothing but
// clutter to the community's; a real extra word, a number no position states, an
// edited title or a title the community one isn't in stays.
func TestTidyTitle(t *testing.T) {
	t.Parallel()
	prop := func(title, subtitle, series, index string, more ...catalog.SeriesRef) catalog.MatchProposal {
		v := map[string]string{"title": title, "subtitle": subtitle, "series": series, "series_index": index}
		if len(more) > 0 {
			v["more_series"] = catalog.EncodeMoreSeries(more)
		}
		return catalog.MatchProposal{Values: v}
	}
	for _, tc := range []struct {
		file string
		p    catalog.MatchProposal
		want string
	}{
		{"Mystic Knight: Emergence: (The Mystic Knight Book 1)", prop("Emergence", "", "The Mystic Knight", "1"), "Emergence"},
		{"Primal Hunter 3: Book III: LitRPG/Epic Progression (Unabridged)", prop("Primal Hunter 3", "", "The Primal Hunter", "3"), "Primal Hunter 3"},
		{"The Way of Kings: Book One of the Stormlight Archive", prop("The Way of Kings", "", "The Stormlight Archive", "1"), "The Way of Kings"},
		{"Sharpe's Eagle - Sharpe, Book 8", prop("Sharpe's Eagle", "", "Richard Sharpe", "", catalog.SeriesAt("Sharpe", "8")), "Sharpe's Eagle"},
		{"A Parade of Horribles - Dungeon Crawler Carl, Book 8", prop("A Parade of Horribles", "Dungeon Crawler Carl, Book 8", "", ""), "A Parade of Horribles"},
		{"the martian", prop("The Martian", "", "", ""), "The Martian"},
		{"THE MARTIAN", prop("The Martian", "", "", ""), "The Martian"},
		{"Dune (Unabridged), Vol. 1st", prop("Dune", "", "", "1"), "Dune"},
		// A position with a decimal point, and a range: each of its numbers is stated.
		{"Secret History - Mistborn, Book 3.5", prop("Secret History", "", "Mistborn", "3.5"), "Secret History"},
		{"The Bands of Mourning Vol 1-3", prop("The Bands of Mourning", "", "Mistborn", "1-3"), "The Bands of Mourning"},
		// The series as the catalogue dresses it: a trailing Series, an ordering note.
		{"Emergence: Dragon Heart, Book 1", prop("Emergence", "", "Dragon Heart Series", "1"), "Emergence"},
		{"Memory: Vorkosigan Saga, Book 11", prop("Memory", "", "Vorkosigan Saga (Chronological)", "11"), "Memory"},
		// Named twice, without its article; a series inside another comes out whole.
		{"Sharpe: Sharpe's Eagle (Sharpe Book 8)", prop("Sharpe's Eagle", "", "The Sharpe", "8"), "Sharpe's Eagle"},
		{"Emergence: The Mystic Knight Saga, Book 1", prop("Emergence", "", "Mystic Knight", "1", catalog.SeriesAt("The Mystic Knight Saga", "1")), "Emergence"},
		{"Dune: Deluxe Edition", prop("Dune", "", "", ""), ""},
		// A number no position states: another volume, or the book's own title.
		{"Dune (Unabridged), Vol. 1st", prop("Dune", "", "", ""), ""},
		{"Primal Hunter 3", prop("Primal Hunter", "", "The Primal Hunter", "1"), ""},
		{"Divergent, Book 2", prop("Divergent", "", "Divergent", "1"), ""},
		{"The Stand, Volume 2", prop("The Stand", "", "", ""), ""},
		{"Fahrenheit 451", prop("Fahrenheit", "", "", ""), ""},
		{"Blade Runner 2049", prop("Blade Runner", "", "", ""), ""},
		{"Malcolm X", prop("Malcolm", "", "", ""), ""},
		{"I, Robot", prop("Robot", "", "", ""), ""},
		{"Alien³", prop("Alien", "", "", ""), ""},
		// A real word left over: another book, another edition, a split of the
		// book, a subtitle the community doesn't have.
		{"Dune Messiah", prop("Dune", "", "Dune", ""), ""},
		{"The Martian (Dramatized Adaptation)", prop("The Martian", "", "", ""), ""},
		{"Dune (Abridged)", prop("Dune", "", "", ""), ""},
		{"The Way of Kings, Part 1", prop("The Way of Kings", "", "The Stormlight Archive", "1"), ""},
		{"Emergence: A Tale of Knights", prop("Emergence", "", "The Mystic Knight", ""), ""},
		{"Dune Mix", prop("Dune", "", "", ""), ""},
		// The community title isn't in it, or it is the title already, or only its
		// style differs.
		{"Harry Potter 1", prop("Harry Potter and the Philosopher's Stone", "", "Harry Potter", "1"), ""},
		{"Emergence", prop("Emergence", "", "", ""), ""},
		{"Emergence (Unabridged)", prop("", "", "", ""), ""},
		{"Blood and Gold", prop("Blood And Gold", "", "", ""), ""},
		{"Dune™", prop("Dune", "", "", ""), ""},
		// Marks spell a word: these two are not the same word.
		{"कोमा", prop("काम", "", "", ""), ""},
	} {
		st := state(map[string]string{"title": tc.file}, nil, false)
		if got := tidyTitle(st, tc.p); got != tc.want {
			t.Errorf("%q under %v: %q, want %q", tc.file, tc.p.Values, got, tc.want)
		}
	}
	// A book's own series counts as clutter too, set or scanned, its position with
	// it, in its main series or another.
	const fool = "Golden Fool - Tawny Man 2"
	st := state(map[string]string{"title": fool, "series": "Tawny Man", "series_index": "2"}, nil, false)
	if got := tidyTitle(st, prop("Golden Fool", "", "", "")); got != "Golden Fool" {
		t.Errorf("the book's own series: %q", got)
	}
	st = state(map[string]string{"title": fool, "series": "Elderlings", "series_index": "15"}, nil, false)
	st.Fields[catalog.FieldSeries] = catalog.FieldValue{Value: "Elderlings", Scanned: "Tawny Man"}
	st.Fields[catalog.FieldSeriesIndex] = catalog.FieldValue{Value: "15", Scanned: "2"}
	if got := tidyTitle(st, prop("Golden Fool", "", "", "")); got != "Golden Fool" {
		t.Errorf("the book's scanned series: %q", got)
	}
	st = state(map[string]string{"title": fool, "more_series": catalog.EncodeMoreSeries([]catalog.SeriesRef{catalog.SeriesAt("Tawny Man", "2")})}, nil, false)
	if got := tidyTitle(st, prop("Golden Fool", "", "", "")); got != "Golden Fool" {
		t.Errorf("the book's own other series: %q", got)
	}
	st = state(map[string]string{"title": fool, "series": "Tawny Man"}, nil, false)
	if got := tidyTitle(st, prop("Golden Fool", "", "", "")); got != "" {
		t.Errorf("its series, no position stating the 2: %q", got)
	}
}

// TestPlanTidiesOnlyUnderFill: the tidy is fill's; ids writes no title, an
// admin's edited title is never touched under any scope, and overwrite
// replaces the title as it always did.
func TestPlanTidiesOnlyUnderFill(t *testing.T) {
	t.Parallel()
	p := catalog.MatchProposal{Values: map[string]string{"title": "Emergence", "series": "The Mystic Knight", "series_index": "1", "asin": "B0EM000001"}}
	cluttered := map[string]string{"title": "Emergence (The Mystic Knight Book 1)"}
	if set, _ := planSet(ScopeFill, state(cluttered, nil, false), p); set["title"] != "Emergence" {
		t.Errorf("fill: %v", set)
	}
	if set, _ := planSet(ScopeIDs, state(cluttered, nil, false), p); set["title"] != "" {
		t.Errorf("ids tidied the title: %v", set)
	}
	for _, scope := range Scopes {
		if set, _ := planSet(scope, state(cluttered, []string{"title"}, false), p); set["title"] != "" {
			t.Errorf("%s replaced an edited title: %v", scope, set)
		}
	}
	extra := map[string]string{"title": "Emergence: The Knight's Vigil"}
	if set, _ := planSet(ScopeFill, state(extra, nil, false), p); set["title"] != "" {
		t.Errorf("fill replaced a title with real extra words: %v", set)
	}
	if set, _ := planSet(ScopeOverwrite, state(extra, nil, false), p); set["title"] != "Emergence" {
		t.Errorf("overwrite: %v", set)
	}
}

// TestQueryForLanguage: the book's own language wins over the marketplace's,
// and a marketplace with no known language sends none.
func TestQueryForLanguage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ lang, tag, region, want string }{
		{"", "", "uk", "en"}, {"", "", "de", "de"}, {"fr", "", "uk", "fr"}, {" ", "", "jp", "ja"}, {"", "", "", ""},
		{"", "", "zz", ""},
		// The tags' language comes after the book's own and before the region's.
		{"", "German", "uk", "german"}, {"it", "German", "uk", "it"}, {"", "und", "uk", "en"},
		// A placeholder says nothing, so the region's still counts.
		{"", "N/A", "uk", "en"}, {"", "XXX", "de", "de"},
	} {
		if got := QueryFor(&catalog.Book{Title: "X", Language: tc.lang, TagLanguage: tc.tag}, tc.region).Language; got != tc.want {
			t.Errorf("language %q, tag %q on %q: %q, want %q", tc.lang, tc.tag, tc.region, got, tc.want)
		}
	}
	if got := QueryFor(&catalog.Book{Narrator: "Kate Reading & Michael Kramer"}, "").Narrators; !slices.Equal(got, []string{"Kate Reading", "Michael Kramer"}) {
		t.Errorf("narrators = %q", got)
	}
}

// TestQueryForTagHints: a book with no ASIN or ISBN of its own sends its tags'
// as the book's identifiers (a hint metaserve looks up); one with either of
// its own sends only its own, never a tag's beside it.
func TestQueryForTagHints(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		book               catalog.Book
		wantASIN, wantISBN string
	}{
		{"hints alone", catalog.Book{TagASIN: "B0TAG00001", TagISBN: "9780000000005"}, "B0TAG00001", "9780000000005"},
		{"its own ASIN", catalog.Book{ASIN: "B0OWN00001", TagASIN: "B0TAG00001", TagISBN: "9780000000005"}, "B0OWN00001", ""},
		{"its own ISBN", catalog.Book{ISBN: "9780000000006", TagASIN: "B0TAG00001"}, "", "9780000000006"},
		{"nothing", catalog.Book{}, "", ""},
	} {
		q := QueryFor(&tc.book, "")
		if q.BookASIN != tc.wantASIN || q.BookISBN != tc.wantISBN {
			t.Errorf("%s: BookASIN %q BookISBN %q, want %q %q", tc.name, q.BookASIN, q.BookISBN, tc.wantASIN, tc.wantISBN)
		}
	}
}

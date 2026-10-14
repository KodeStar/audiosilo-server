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
		{ScopeFill, []string{"asin", "description", "isbn", "published", "series", "series_index"}, true},
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
	if got := p.Values["more_series"]; got != `[{"name":"`+watch+`","position":1},{"name":"Omnibus","position":0}]` {
		t.Fatalf("proposed more_series = %s", got)
	}

	// No series yet: the work's main one, and the rest beside it.
	set, _ := planSet(ScopeFill, state(map[string]string{"title": "Guards! Guards!"}, nil, false), p)
	if set["series"] != "Discworld" || set["series_index"] != "8" ||
		set["more_series"] != `[{"name":"`+watch+`","position":1},{"name":"Omnibus","position":0}]` {
		t.Fatalf("fill on no series = %v", set)
	}

	// Filed under City Watch by its folder: fill keeps that, numbers it as City
	// Watch numbers it (not Discworld's 8), and adds Discworld beside it.
	st := state(map[string]string{"title": "Guards! Guards!", "series": watch}, nil, false)
	set, _ = planSet(ScopeFill, st, p)
	if _, ok := set["series"]; ok || set["series_index"] != "1" ||
		set["more_series"] != `[{"name":"Discworld","position":8},{"name":"Omnibus","position":0}]` {
		t.Fatalf("fill on City Watch = %v", set)
	}

	// Overwrite keeps it too (the work is in City Watch, as the match dialog
	// offers it), numbered there.
	st = state(map[string]string{"title": "Guards! Guards!", "series": watch, "series_index": "8"}, nil, false)
	if set, _ := planSet(ScopeOverwrite, st, p); set["series"] != "" || set["series_index"] != "1" ||
		set["more_series"] != `[{"name":"Discworld","position":8},{"name":"Omnibus","position":0}]` {
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
	if set, _ := planSet(ScopeFill, st, p); set["more_series"] != `[{"name":"`+watch+`","position":1},{"name":"Omnibus","position":0}]` {
		t.Fatalf("fill beside an unlisted series = %v", set)
	}

	// The admin's own list stays, under any scope.
	st = state(map[string]string{"title": "Guards! Guards!", "more_series": `[{"name":"Mine","position":2}]`}, []string{"more_series"}, false)
	if set, _ := planSet(ScopeOverwrite, st, p); set["more_series"] != "" {
		t.Fatalf("overwrite replaced the admin's list: %v", set)
	}
	// A list the book already has isn't "set".
	st = state(map[string]string{"title": "Guards! Guards!", "series": "Discworld", "series_index": "8",
		"more_series": `[{"name":"` + watch + `","position":1},{"name":"Omnibus","position":0}]`}, nil, false)
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

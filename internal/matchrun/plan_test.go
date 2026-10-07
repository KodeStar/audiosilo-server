package matchrun

import (
	"maps"
	"slices"
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
		set, cover := Plan(tc.scope, st, p)
		if got := keys(set); !slices.Equal(got, tc.want) || cover != tc.cover {
			t.Errorf("%s: set %v cover %v, want %v %v", tc.scope, got, cover, tc.want, tc.cover)
		}
	}

	// A book with a cover keeps it; a value the book already has isn't "set".
	st = state(map[string]string{"title": "The Martian", "series": "Mars"}, nil, false)
	set, cover := Plan(ScopeFill, st, p)
	if cover || set["title"] != "" || set["series"] != "" || set["series_index"] != "1" {
		t.Fatalf("fill over a cover = %v %v", set, cover)
	}

	// A position never lands beside another series: the book's own "Red Planet"
	// (kept by fill) has no place for Mars's "1".
	st = state(map[string]string{"title": "The Martian", "series": "Red Planet"}, nil, false)
	set, _ = Plan(ScopeFill, st, p)
	if _, ok := set["series_index"]; ok {
		t.Fatalf("fill put Mars #1 beside Red Planet: %v", set)
	}
	// ...but overwrite, which replaces the series too, sets both.
	set, _ = Plan(ScopeOverwrite, st, p)
	if set["series"] != "Mars" || set["series_index"] != "1" {
		t.Fatalf("overwrite = %v", set)
	}
	// An admin-edited series stays, and so the position stays off it.
	st = state(map[string]string{"title": "The Martian", "series": "Red Planet"}, []string{"series"}, false)
	set, _ = Plan(ScopeOverwrite, st, p)
	if _, ok := set["series_index"]; ok || set["series"] != "" {
		t.Fatalf("overwrite beside an edited series = %v", set)
	}

	// No cover on offer, no cover taken.
	bare := p
	bare.CoverURL = ""
	if _, cover := Plan(ScopeFill, state(nil, nil, true), bare); cover {
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
	} {
		if got, _, _ := Classify(tc.cands, tc.p); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
	if _, score, runnerUp := Classify(cands(95, 85), ids); score != 95 || runnerUp != 85 {
		t.Fatalf("scores = %d %d", score, runnerUp)
	}
}

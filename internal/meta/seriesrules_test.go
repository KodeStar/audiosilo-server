package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seriesNames renders series names in order, so a case reads at a glance.
func seriesNames(series []MatchSeries) string {
	parts := make([]string, 0, len(series))
	for _, s := range series {
		parts = append(parts, s.Name)
	}
	return strings.Join(parts, "; ")
}

func narrators(names ...string) []upstreamPersonRef {
	out := make([]upstreamPersonRef, 0, len(names))
	for _, n := range names {
		out = append(out, upstreamPersonRef{ID: strings.ToLower(n), Name: n})
	}
	return out
}

func seriesRef(id, name string) upstreamSeriesRef {
	return upstreamSeriesRef{ID: id, Name: name, Position: "2"}
}

// potterWork is the motivating case: a work listed in its series and in two
// editions of it, each named for a kind of recording.
func potterWork() *upstreamWorkDetail {
	return &upstreamWorkDetail{
		ID: "chamber", Title: "Harry Potter and the Chamber of Secrets", Language: "en",
		Series: []upstreamSeriesRef{
			seriesRef("harry-potter", "Harry Potter"),
			seriesRef("hp-full-cast", "Harry Potter (Full-Cast Editions)"),
			seriesRef("hp-fry", "Harry Potter (Narrated by Stephen Fry)"),
		},
		Recordings: []upstreamRecording{
			{ID: "jim-dale", Narrators: narrators("Jim Dale")},
			{ID: "stephen-fry", Narrators: narrators("Stephen Fry")},
			{ID: "full-cast", Narrators: narrators("Hugh Laurie", "Riz Ahmed", "Cush Jumbo", "Simon Pegg", "Indira Varma")},
		},
	}
}

// TestWorkSeriesEditions: an edition (a series named like another of the work's,
// plus a qualifier naming a kind of recording) belongs to the recordings it
// names, and a book is in it only as one of them, where it is the main series;
// a book of another recording, or of none known, gets the work's series alone.
func TestWorkSeriesEditions(t *testing.T) {
	t.Parallel()
	ws := newWorkSeries(potterWork(), nil)
	for rec, want := range map[string]string{
		"jim-dale":    "Harry Potter",
		"":            "Harry Potter",
		"unknown":     "Harry Potter",
		"full-cast":   "Harry Potter (Full-Cast Editions); Harry Potter",
		"stephen-fry": "Harry Potter (Narrated by Stephen Fry); Harry Potter",
	} {
		if got := seriesNames(ws.of(rec)); got != want {
			t.Errorf("of(%q) = %s, want %s", rec, got, want)
		}
	}
}

// TestJudgeSeries: the allowed and the denied cases of each rule, as the series
// a book of an unabridged recording is in.
func TestJudgeSeries(t *testing.T) {
	t.Parallel()
	ref := seriesRef
	unabridged := upstreamRecording{ID: "full", Narrators: narrators("Stephen Briggs")}
	abridged := upstreamRecording{ID: "short", Abridged: true, Narrators: narrators("Tony Robinson")}
	cases := []struct {
		name  string
		refs  []upstreamSeriesRef
		facts map[string]*seriesFacts
		want  string // a book of the unabridged recording
		short string // a book of the abridged one ("" = as want)
	}{{
		name: "a sub-series named after its parent is a series of its own",
		refs: []upstreamSeriesRef{ref("discworld", "Discworld"), ref("tiffany", "Discworld: Tiffany Aching")},
		want: "Discworld; Discworld: Tiffany Aching",
	}, {
		name: "an abridged edition only for the abridged recording, as its main series",
		refs: []upstreamSeriesRef{ref("discworld", "Discworld"), ref("discworld-abridged", "Discworld (abridged)"),
			ref("tiffany", "Discworld: Tiffany Aching"), ref("tiffany-abridged", "Discworld: Tiffany Aching (abridged)")},
		want:  "Discworld; Discworld: Tiffany Aching",
		short: "Discworld (abridged); Discworld; Discworld: Tiffany Aching; Discworld: Tiffany Aching (abridged)",
	}, {
		name: "a qualified name with no base on the work is a series of its own",
		refs: []upstreamSeriesRef{ref("tiffany-abridged", "Discworld: Tiffany Aching (abridged)")},
		want: "Discworld: Tiffany Aching (abridged)",
	}, {
		name: "an ordering note, a language edition or an unabridged edition holds no recording",
		refs: []upstreamSeriesRef{ref("narnia", "Narnia"), ref("chrono", "Narnia (Chronological)"),
			ref("de", "Narnia [German Edition]"), ref("full", "Narnia (Unabridged)")},
		want: "Narnia",
	}, {
		name: "a variant reading order is never one of the book's series",
		refs: []upstreamSeriesRef{ref("narnia", "Narnia"), {ID: "chrono", Name: "Narnia, chronologically", Position: "2", OrderingOf: "narnia"}},
		want: "Narnia",
	}, {
		name:  "a translation of a series the work is in: dropped",
		refs:  []upstreamSeriesRef{ref("bobiverse", "Bobiverse"), ref("bobiverse-it", "Bobiverse")},
		facts: map[string]*seriesFacts{"bobiverse-it": {TranslationOf: []string{"bobiverse"}}},
		want:  "Bobiverse",
	}, {
		name:  "a translation whose original the work isn't in: kept",
		refs:  []upstreamSeriesRef{ref("bobiverse-it", "Bobiverse Italia")},
		facts: map[string]*seriesFacts{"bobiverse-it": {Language: "en", TranslationOf: []string{"bobiverse"}}},
		want:  "Bobiverse Italia",
	}, {
		name: "another language's series: dropped; a regional tag or an unknown language: kept",
		refs: []upstreamSeriesRef{ref("hp", "Harry Potter"), ref("hp-de", "Harry Potter Deutsch"),
			ref("hp-gb", "Harry Potter UK"), ref("hp-tie", "Harry Potter Tie")},
		facts: map[string]*seriesFacts{"hp": {Language: "en"}, "hp-de": {Language: "de"}, "hp-gb": {Language: "en-GB"}, "hp-tie": {}},
		want:  "Harry Potter; Harry Potter UK; Harry Potter Tie",
	}, {
		name: "a series listed twice keeps its first position",
		refs: []upstreamSeriesRef{ref("dune", "Dune"), {ID: "dune", Name: "Dune", Position: "7"}},
		want: "Dune",
	}}
	for _, c := range cases {
		d := &upstreamWorkDetail{ID: "w", Language: "en", Series: c.refs, Recordings: []upstreamRecording{unabridged, abridged}}
		ws := newWorkSeries(d, c.facts)
		if got := seriesNames(ws.of("full")); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
		if c.short == "" {
			c.short = c.want
		}
		if got := seriesNames(ws.of("short")); got != c.short {
			t.Errorf("%s (abridged): %s, want %s", c.name, got, c.short)
		}
	}
}

// TestEditionHolds: what each kind of qualifier holds.
func TestEditionHolds(t *testing.T) {
	t.Parallel()
	fry := upstreamRecording{Narrators: narrators("Stephen Fry")}
	beck := upstreamRecording{Narrators: narrators("Rufus Beck")}
	short := upstreamRecording{Abridged: true, Narrators: narrators("Tony Robinson")}
	cast := upstreamRecording{Narrators: narrators("Full Cast")}
	trio := upstreamRecording{Narrators: narrators("Indira Varma", "Bill Nighy", "Steven Cree")}
	for _, c := range []struct {
		qualifier string
		rec       upstreamRecording
		want      bool
	}{
		{"abridged", short, true},
		{"abridged", fry, false},
		{"gekürzt", short, true},
		{"Unabridged", fry, false},
		{"ungekürzt", short, false},
		{"Full-Cast Editions", cast, true},
		{"Full Cast", trio, false}, // three voices is a reading, not a cast
		{"Dramatized Adaptation", cast, true},
		{"Narrated by Stephen Fry", fry, true},
		{"Narrated by Stephen Fry", beck, false},
		{"Gesprochen von Rufus Beck", beck, true},
		{"German Edition", fry, false},
		{"Chronological", fry, false},
	} {
		if got := kindOf(c.qualifier).holds(c.qualifier, &c.rec); got != c.want {
			t.Errorf("%q holds %v = %v, want %v", c.qualifier, c.rec.Narrators, got, c.want)
		}
	}
	if q, ok := editionQualifier("Harry Potter - Gesprochen von Rufus Beck", []string{"Harry Potter"}); !ok || q != "Gesprochen von Rufus Beck" {
		t.Errorf("a narrator tail = %q %v", q, ok)
	}
	if _, ok := editionQualifier("Harry Potter - The Illustrated Editions", []string{"Harry Potter"}); ok {
		t.Error("a dash tail that names no narrator read as an edition")
	}
}

// TestRailMains: the rail families a book's cap is spent on. A Discworld novel in
// more series than the cap, most of them editions of the others, keeps the
// series a book of its recording is in (the Series page's "Discworld: Tiffany
// Aching" was cut by "(abridged)" editions sorting first); an edition comes back
// for its own recording, after the rest; an edition named for anything else keeps
// its place; a series the cache knows is foreign goes.
func TestRailMains(t *testing.T) {
	t.Parallel()
	d := &upstreamWorkDetail{ID: "hat", Language: "en", Series: []upstreamSeriesRef{
		seriesRef("discworld", "Discworld"), seriesRef("discworld-abridged", "Discworld (abridged)"),
		seriesRef("for-kids", "Discworld: For Kids"), seriesRef("for-kids-abridged", "Discworld: For Kids (abridged)"),
		seriesRef("tiffany", "Discworld: Tiffany Aching"), seriesRef("tiffany-abridged", "Discworld: Tiffany Aching (abridged)"),
		seriesRef("chrono", "Discworld (Chronological)"), seriesRef("scheibenwelt", "Scheibenwelt"),
	}, Recordings: []upstreamRecording{
		{ID: "briggs", Narrators: narrators("Stephen Briggs")},
		{ID: "robinson", Abridged: true, Narrators: narrators("Tony Robinson")},
	}}
	ids := func(refs []upstreamSeriesRef) string {
		out := make([]string, 0, len(refs))
		for _, r := range refs {
			out = append(out, r.ID)
		}
		return strings.Join(out, " ")
	}
	foreign := map[string]*seriesFacts{"scheibenwelt": {Language: "de"}}
	if got, want := ids(railMains(familyMains(d.Series), d, "briggs", foreign)), "discworld for-kids tiffany chrono"; got != want {
		t.Errorf("unabridged = %s, want %s", got, want)
	}
	if got, want := ids(railMains(familyMains(d.Series), d, "robinson", foreign)),
		"discworld for-kids tiffany chrono discworld-abridged for-kids-abridged tiffany-abridged"; got != want {
		t.Errorf("abridged = %s, want %s", got, want)
	}
	if got := ids(railMains(familyMains(d.Series), d, "briggs", nil)); !strings.HasSuffix(got, "chrono scheibenwelt") {
		t.Errorf("no facts known = %s, want the foreign series kept in place", got)
	}
}

// seriesBody is a series record with the given extra fields and no works.
func seriesBody(id, name, extra string) string {
	return `{"id":"` + id + `","name":"` + name + `",` + extra + `"authors":[],"works":[]}`
}

// TestCandidatesApplyTheSeriesRules: a candidate's series are the ones a book of
// its work can be in, read from the series records too (a translation and another
// language's series are left out), each record read once, then from the cache;
// each recording carries its own; a repick reads no record.
func TestCandidatesApplyTheSeriesRules(t *testing.T) {
	t.Parallel()
	m := &mockMeta{
		lookupJSON: `{"work":{"id":"w","title":"T","authors":[]},"recording_id":"porter"}`,
		workJSON: `{"id":"w","title":"We Are Legion","language":"en","authors":[],"series":[
			{"id":"bobiverse","name":"Bobiverse","position":"1"},
			{"id":"bobiverse-it","name":"Bobiverse","position":"1"},
			{"id":"bob-de","name":"Bob-Universum","position":"1"},
			{"id":"bobiverse-gone","name":"Bobiverse Omnibus","position":"1"}],
			"recordings":[{"id":"porter","narrators":[{"id":"ray-porter","name":"Ray Porter"}]}]}`,
		seriesJSON: map[string]string{
			"bobiverse":    seriesBody("bobiverse", "Bobiverse", `"language":"en",`),
			"bobiverse-it": seriesBody("bobiverse-it", "Bobiverse", `"language":"en","translation_of":[{"id":"bobiverse","name":"Bobiverse","language":"en"}],`),
			"bob-de":       seriesBody("bob-de", "Bob-Universum", `"language":"de",`),
			// bobiverse-gone has no record (404): nothing is known against it.
		},
	}
	mux := http.NewServeMux()
	mux.Handle("/", m.handler())
	mux.HandleFunc("GET /api/v1/works/match", func(w http.ResponseWriter, _ *http.Request) {
		// Two hits of the one work: its series records are still read once each.
		_, _ = w.Write([]byte(`{"results":[{"id":"w","score":95,"reasons":{}},{"id":"w","score":90,"reasons":{}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := NewService(srv.URL, nil)

	if _, err := s.Candidates(context.Background(), MatchQuery{ASIN: "B0LEGION", NoSeries: true}); err != nil || m.seriesHits.Load() != 0 {
		t.Fatalf("repick read %d series records (%v), want none", m.seriesHits.Load(), err)
	}
	for range 2 {
		cands, err := s.Candidates(context.Background(), MatchQuery{Title: "We Are Legion"})
		if err != nil || len(cands) != 2 {
			t.Fatalf("candidates = %v %v", cands, err)
		}
		if got, want := seriesNames(cands[0].Series), "Bobiverse; Bobiverse Omnibus"; got != want {
			t.Fatalf("series = %s, want %s", got, want)
		}
		if rec := cands[0].Recordings[0]; seriesNames(rec.Series) != "Bobiverse; Bobiverse Omnibus" || cands[0].Series[0].ID != "bobiverse" {
			t.Fatalf("recording series = %+v, series = %+v", rec.Series, cands[0].Series)
		}
	}
	if got := m.seriesHits.Load(); got != 4 {
		t.Fatalf("series GETs = %d, want 4 (one per series, then the cache)", got)
	}
}

// TestEnrichLeavesOutAForeignRail: a rail whose record says it is in another
// language than the work is left out of the envelope.
func TestEnrichLeavesOutAForeignRail(t *testing.T) {
	t.Parallel()
	m := &mockMeta{
		lookupJSON: `{"work":{"id":"w","title":"A Hat Full of Sky","authors":[]},"recording_id":"briggs"}`,
		workJSON: `{"id":"w","title":"A Hat Full of Sky","language":"en","authors":[],"series":[
			{"id":"scheibenwelt","name":"Scheibenwelt","position":"32"},{"id":"discworld","name":"Discworld","position":"32"}],
			"recordings":[{"id":"briggs","narrators":[]}]}`,
		seriesJSON: map[string]string{
			"scheibenwelt": seriesBody("scheibenwelt", "Scheibenwelt", `"language":"de",`),
			"discworld":    seriesBody("discworld", "Discworld", `"language":"en",`),
		},
	}
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	env, err := NewService(srv.URL, nil).Enrich(context.Background(), "B0HAT", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Series) != 1 || env.Series[0].Name != "Discworld" {
		t.Fatalf("rails = %+v, want Discworld alone", env.Series)
	}
}

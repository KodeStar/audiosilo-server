package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// seriesNames renders series as "name[recording,...]" in order, so a case reads
// at a glance.
func seriesNames(series []MatchSeries) string {
	parts := make([]string, 0, len(series))
	for _, s := range series {
		p := s.Name
		if len(s.RecordingIDs) > 0 {
			p += "[" + strings.Join(s.RecordingIDs, ",") + "]"
		}
		parts = append(parts, p)
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

// potterWork is the motivating case: a work listed in its series and in two
// editions of it, each named for a kind of recording.
func potterWork() *upstreamWorkDetail {
	return &upstreamWorkDetail{
		ID: "chamber", Title: "Harry Potter and the Chamber of Secrets", Language: "en",
		Series: []upstreamSeriesRef{
			{ID: "harry-potter", Name: "Harry Potter", Position: "2"},
			{ID: "harry-potter-full-cast-editions", Name: "Harry Potter (Full-Cast Editions)", Position: "2"},
			{ID: "harry-potter-narrated-by-stephen-fry", Name: "Harry Potter (Narrated by Stephen Fry)", Position: "2"},
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
// a book of another recording gets the work's own series alone.
func TestWorkSeriesEditions(t *testing.T) {
	t.Parallel()
	series := workSeries(potterWork(), nil)
	if got, want := seriesNames(series), "Harry Potter; Harry Potter (Full-Cast Editions)[full-cast]; Harry Potter (Narrated by Stephen Fry)[stephen-fry]"; got != want {
		t.Fatalf("series = %s\nwant %s", got, want)
	}
	for rec, want := range map[string]string{
		"jim-dale":    "Harry Potter",
		"":            "Harry Potter",
		"unknown":     "Harry Potter",
		"full-cast":   "Harry Potter (Full-Cast Editions)[full-cast]; Harry Potter",
		"stephen-fry": "Harry Potter (Narrated by Stephen Fry)[stephen-fry]; Harry Potter",
	} {
		if got := seriesNames(SeriesFor(series, rec)); got != want {
			t.Errorf("SeriesFor(%q) = %s, want %s", rec, got, want)
		}
	}
}

// TestJudgeSeries: the allowed and the denied cases of each rule.
func TestJudgeSeries(t *testing.T) {
	t.Parallel()
	ref := func(id, name string) upstreamSeriesRef { return upstreamSeriesRef{ID: id, Name: name, Position: "1"} }
	unabridged := upstreamRecording{ID: "full", Narrators: narrators("Stephen Briggs")}
	abridged := upstreamRecording{ID: "short", Abridged: true, Narrators: narrators("Tony Robinson")}
	cases := []struct {
		name  string
		refs  []upstreamSeriesRef
		recs  []upstreamRecording
		facts map[string]*seriesFacts
		want  string
	}{{
		name: "a sub-series named after its parent is a series of its own",
		refs: []upstreamSeriesRef{ref("discworld", "Discworld"), ref("tiffany", "Discworld: Tiffany Aching")},
		recs: []upstreamRecording{unabridged},
		want: "Discworld; Discworld: Tiffany Aching",
	}, {
		name: "an abridged edition no recording is: dropped",
		refs: []upstreamSeriesRef{ref("discworld", "Discworld"), ref("discworld-abridged", "Discworld (abridged)"), ref("tiffany", "Discworld: Tiffany Aching"), ref("tiffany-abridged", "Discworld: Tiffany Aching (abridged)")},
		recs: []upstreamRecording{unabridged},
		want: "Discworld; Discworld: Tiffany Aching",
	}, {
		name: "an abridged edition an abridged recording is: kept for it",
		refs: []upstreamSeriesRef{ref("discworld", "Discworld"), ref("discworld-abridged", "Discworld (abridged)")},
		recs: []upstreamRecording{unabridged, abridged},
		want: "Discworld; Discworld (abridged)[short]",
	}, {
		name: "a qualified name with no base on the work is a series of its own",
		refs: []upstreamSeriesRef{ref("tiffany-abridged", "Discworld: Tiffany Aching (abridged)")},
		recs: []upstreamRecording{unabridged},
		want: "Discworld: Tiffany Aching (abridged)",
	}, {
		name: "an ordering note, a language edition or an unabridged edition holds no recording",
		refs: []upstreamSeriesRef{ref("narnia", "Narnia"), ref("chrono", "Narnia (Chronological)"), ref("de", "Narnia [German Edition]"), ref("full", "Narnia (Unabridged)")},
		recs: []upstreamRecording{unabridged, abridged},
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
		refs:  []upstreamSeriesRef{ref("bobiverse-it", "Bobiverse")},
		facts: map[string]*seriesFacts{"bobiverse-it": {Language: "en", TranslationOf: []string{"bobiverse"}}},
		want:  "Bobiverse",
	}, {
		name:  "another language's series: dropped; a regional tag or an unknown language: kept",
		refs:  []upstreamSeriesRef{ref("hp", "Harry Potter"), ref("hp-de", "Harry Potter (Deutsch)"), ref("hp-gb", "Harry Potter UK"), ref("hp-tie", "Harry Potter Tie")},
		facts: map[string]*seriesFacts{"hp": {Language: "en"}, "hp-de": {Language: "de"}, "hp-gb": {Language: "en-GB"}, "hp-tie": {}},
		want:  "Harry Potter; Harry Potter UK; Harry Potter Tie",
	}, {
		name: "a series listed twice keeps its first position",
		refs: []upstreamSeriesRef{ref("dune", "Dune"), {ID: "dune", Name: "Dune", Position: "7"}},
		want: "Dune",
	}}
	for _, c := range cases {
		d := &upstreamWorkDetail{ID: "w", Language: "en", Series: c.refs, Recordings: c.recs}
		if got := seriesNames(workSeries(d, c.facts)); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
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
		if got := editionHolds(c.qualifier, &c.rec); got != c.want {
			t.Errorf("editionHolds(%q, %v) = %v, want %v", c.qualifier, c.rec.Narrators, got, c.want)
		}
	}
	if q, ok := editionQualifier("Harry Potter - Gesprochen von Rufus Beck", []string{"Harry Potter"}); !ok || q != "Gesprochen von Rufus Beck" {
		t.Errorf("a narrator tail = %q %v", q, ok)
	}
	if _, ok := editionQualifier("Harry Potter - The Illustrated Editions", []string{"Harry Potter"}); ok {
		t.Error("a dash tail that names no narrator read as an edition")
	}
}

// rulesMeta serves a work whose series records carry facts, through works/match
// and the lookup, counting series GETs.
func rulesMeta(t *testing.T, work string, series map[string]string) (*Service, *mockMeta) {
	t.Helper()
	m := &mockMeta{
		lookupJSON: `{"work":{"id":"w","title":"T","authors":[]},"recording_id":"briggs"}`,
		workJSON:   work, seriesJSON: series,
	}
	mux := http.NewServeMux()
	mux.Handle("/", m.handler())
	mux.HandleFunc("GET /api/v1/works/match", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":"w","score":95,"reasons":{}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewService(srv.URL, nil), m
}

// seriesBody is a series record with the given extra fields and no works.
func seriesBody(id, name, extra string) string {
	return `{"id":"` + id + `","name":"` + name + `",` + extra + `"authors":[],"works":[]}`
}

// TestCandidatesApplyTheSeriesRules: a candidate's series are the ones a book of
// its work can be in, read from the series records too (a translation and another
// language's series are left out), and a second look is answered from the cache.
func TestCandidatesApplyTheSeriesRules(t *testing.T) {
	t.Parallel()
	work := `{"id":"w","title":"We Are Legion","language":"en","authors":[],"series":[
		{"id":"bobiverse","name":"Bobiverse","position":"1"},
		{"id":"bobiverse-it","name":"Bobiverse","position":"1"},
		{"id":"bob-de","name":"Bob-Universum","position":"1"},
		{"id":"bobiverse-gone","name":"Bobiverse Omnibus","position":"1"}],
		"recordings":[{"id":"briggs","narrators":[{"id":"ray-porter","name":"Ray Porter"}]}]}`
	s, m := rulesMeta(t, work, map[string]string{
		"bobiverse":    seriesBody("bobiverse", "Bobiverse", `"language":"en",`),
		"bobiverse-it": seriesBody("bobiverse-it", "Bobiverse", `"language":"en","translation_of":[{"id":"bobiverse","name":"Bobiverse","language":"en"}],`),
		"bob-de":       seriesBody("bob-de", "Bob-Universum", `"language":"de",`),
		// bobiverse-gone has no record (404): nothing is known against it.
	})
	for range 2 {
		cands, err := s.Candidates(context.Background(), MatchQuery{Title: "We Are Legion"})
		if err != nil || len(cands) != 1 {
			t.Fatalf("candidates = %v %v", cands, err)
		}
		if got, want := seriesNames(cands[0].Series), "Bobiverse; Bobiverse Omnibus"; got != want {
			t.Fatalf("series = %s, want %s", got, want)
		}
		if cands[0].Series[0].ID != "bobiverse" {
			t.Fatalf("series = %+v, want each with its id", cands[0].Series)
		}
	}
	if got := m.seriesHits.Load(); got != 4 {
		t.Fatalf("series GETs = %d, want 4 (one per series, then the cache)", got)
	}
}

// TestEnrichRailsSpendTheCapOnTheBooksSeries: a work in more series than the
// rail cap, most of them editions of the others, keeps the rails a book of its
// recording is in (the Series page's "Discworld: Tiffany Aching" was cut by
// "(abridged)" editions sorting first); an edition comes back for its own
// recording, after the rest; a rail in another language is left out.
func TestEnrichRailsSpendTheCapOnTheBooksSeries(t *testing.T) {
	t.Parallel()
	refs := []string{"discworld", "Discworld", "discworld-abridged", "Discworld (abridged)",
		"discworld-for-kids", "Discworld: For Kids", "discworld-for-kids-abridged", "Discworld: For Kids (abridged)",
		"discworld-tiffany-aching", "Discworld: Tiffany Aching", "discworld-tiffany-aching-abridged", "Discworld: Tiffany Aching (abridged)"}
	work := func(extra ...string) string {
		var parts []string
		all := append(slices.Clone(refs), extra...)
		for i := 0; i < len(all); i += 2 {
			parts = append(parts, `{"id":"`+all[i]+`","name":"`+all[i+1]+`","position":"2"}`)
		}
		return `{"id":"w","title":"A Hat Full of Sky","language":"en","authors":[],"series":[` + strings.Join(parts, ",") + `],
			"recordings":[{"id":"briggs","narrators":[{"id":"sb","name":"Stephen Briggs"}]},
			{"id":"robinson","abridged":true,"narrators":[{"id":"tr","name":"Tony Robinson"}]}]}`
	}
	series := map[string]string{}
	for i := 0; i < len(refs); i += 2 {
		series[refs[i]] = seriesBody(refs[i], refs[i+1], `"language":"en",`)
	}
	rails := func(lookup, work string) string {
		t.Helper()
		m := &mockMeta{lookupJSON: lookup, workJSON: work, seriesJSON: series}
		srv := httptest.NewServer(m.handler())
		t.Cleanup(srv.Close)
		env, err := NewService(srv.URL, nil).Enrich(context.Background(), "B0HAT", "")
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(env.Series))
		for _, r := range env.Series {
			names = append(names, r.Name)
		}
		return strings.Join(names, "; ")
	}
	const briggs = `{"work":{"id":"w","title":"A Hat Full of Sky","authors":[]},"recording_id":"briggs"}`
	const robinson = `{"work":{"id":"w","title":"A Hat Full of Sky","authors":[]},"recording_id":"robinson"}`
	if got, want := rails(briggs, work()), "Discworld; Discworld: For Kids; Discworld: Tiffany Aching"; got != want {
		t.Errorf("unabridged rails = %s, want %s", got, want)
	}
	if got, want := rails(robinson, work()), "Discworld; Discworld: For Kids; Discworld: Tiffany Aching"; got != want {
		t.Errorf("abridged rails = %s, want %s (the editions after)", got, want)
	}
	series["scheibenwelt"] = seriesBody("scheibenwelt", "Scheibenwelt", `"language":"de",`)
	foreign := `{"id":"w","title":"A Hat Full of Sky","language":"en","authors":[],"series":[
		{"id":"scheibenwelt","name":"Scheibenwelt","position":"32"},{"id":"discworld","name":"Discworld","position":"32"}],
		"recordings":[{"id":"briggs","narrators":[]}]}`
	if got, want := rails(briggs, foreign), "Discworld"; got != want {
		t.Errorf("rails = %s, want %s (none in another language)", got, want)
	}
}

// TestMatchSeriesWire: the fields MatchSeries adds are omitted where empty, so a
// candidate's series read as they did for a series that is no edition.
func TestMatchSeriesWire(t *testing.T) {
	t.Parallel()
	b, _ := json.Marshal(MatchSeries{Name: "Dune", Position: "1"})
	if string(b) != `{"name":"Dune","position":"1"}` {
		t.Fatalf("plain series = %s", b)
	}
}

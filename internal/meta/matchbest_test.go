package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// bestMeta is a metaserve for MatchQuery.BestOnly: works/match answers body, and
// works and series answer from their maps (else 404, or failing's status), each
// request counted by route.
type bestMeta struct {
	body    string
	works   map[string]string
	series  map[string]string
	failing map[string]int

	mu     sync.Mutex
	got    url.Values
	counts map[string]int
}

func (m *bestMeta) count(route string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts == nil {
		m.counts = map[string]int{}
	}
	m.counts[route]++
}

func (m *bestMeta) calls() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts
}

func (m *bestMeta) service(t *testing.T) *Service {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/match", func(w http.ResponseWriter, r *http.Request) {
		m.count("match")
		m.mu.Lock()
		m.got = r.URL.Query()
		m.mu.Unlock()
		_, _ = w.Write([]byte(m.body))
	})
	answer := func(route string, docs map[string]string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			m.count(route + ":" + id)
			if code := m.failing[id]; code != 0 {
				w.WriteHeader(code)
				return
			}
			doc, ok := docs[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(doc))
		}
	}
	mux.HandleFunc("GET /api/v1/works/{id}", answer("work", m.works))
	mux.HandleFunc("GET /api/v1/series/{id}", answer("series", m.series))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewService(srv.URL, nil)
}

// A work in its own series and in a German translation of it, whose facts drop the
// translation, and the same two series' records.
const (
	bestWork = `{"id":"colour","title":"The Colour of Magic","language":"en","authors":[{"id":"tp","name":"Terry Pratchett"}],
		"series":[{"id":"discworld","name":"Discworld","position":"1"},{"id":"scheibenwelt","name":"Scheibenwelt","position":"1"}],
		"recordings":[{"id":"nigel","narrators":[{"id":"np","name":"Nigel Planer"}],"runtime_min":420,"asin":[{"region":"us","asin":"B0COLOUR01"}]}]}`
	otherWork  = `{"id":"light","title":"The Light Fantastic","language":"en","authors":[{"id":"tp","name":"Terry Pratchett"}],"series":[],"recordings":[]}`
	bestFacts  = `{"discworld":{"language":"en"},"scheibenwelt":{"language":"de","translation_of":["discworld"]}}`
	bestReason = `"reasons":{"title":1,"author":"full"}`
)

var bestSeries = map[string]string{
	"discworld":    `{"id":"discworld","name":"Discworld","language":"en"}`,
	"scheibenwelt": `{"id":"scheibenwelt","name":"Scheibenwelt","language":"de","translation_of":[{"id":"discworld","name":"Discworld"}]}`,
}

var bestQuery = MatchQuery{Title: "The Colour of Magic", Author: "Terry Pratchett", Duration: 420 * 60, Limit: 2, BestOnly: true, NoDetailsBelow: 60}

func matchBody(results ...string) string {
	return `{"results":[` + strings.Join(results, ",") + `]}`
}

func matchResult(id string, score int, extra string) string {
	r := `{"kind":"work","id":"` + id + `","score":` + strconv.Itoa(score) + `,` + bestReason
	if extra != "" {
		r += "," + extra
	}
	return r + "}"
}

func inlined(work, facts string) string {
	out := `"work":` + work
	if facts != "" {
		out += `,"series_facts":` + facts
	}
	return out
}

// What is inlined is all that is read, and all a request asks for: one wait.
func TestBestOnlyUsesWhatIsInlined(t *testing.T) {
	m := &bestMeta{body: matchBody(matchResult("colour", 97, inlined(bestWork, bestFacts)), matchResult("light", 71, ""))}
	s := m.service(t)
	cands, err := s.Candidates(context.Background(), bestQuery)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.calls(); !reflect.DeepEqual(got, map[string]int{"match": 1}) {
		t.Fatalf("requests = %v, want only the match", got)
	}
	if m.got.Get("expand") != "work" {
		t.Fatalf("expand = %q, want work", m.got.Get("expand"))
	}
	if len(cands) != 2 || cands[0].WorkID != "colour" || cands[0].DefaultRecordingID != "nigel" || cands[0].Score != 97 {
		t.Fatalf("candidates = %+v", cands)
	}
	if want := []MatchSeries{{ID: "discworld", Name: "Discworld", Position: "1"}}; !reflect.DeepEqual(cands[0].Series, want) {
		t.Fatalf("series = %+v, want the translation dropped by the inlined facts", cands[0].Series)
	}
	if r := cands[1]; r.WorkID != "light" || r.Score != 71 || r.Reasons == nil || r.Title != "" || r.IdentifierHit() {
		t.Fatalf("runner-up = %+v, want a stub with its score and reasons", r)
	}
	// The facts reach the cache a fetch would have filled.
	if f, hit, err := cacheGet[seriesFacts](s.cache, nsSeries.key("scheibenwelt")); !hit || err != nil || f.Language != "de" {
		t.Fatalf("cached facts = %+v, %v, %v", f, hit, err)
	}
}

// The best is Candidates' best, an identifier hit winning a tie, wherever metaserve
// ranked it: index 1 here, where the inlined work is.
func TestBestOnlyFindsTheBestOnTheSecondResult(t *testing.T) {
	hit := `"recording_id":"nigel","reasons":{"identifier":"asin"},` + inlined(bestWork, bestFacts)
	m := &bestMeta{body: matchBody(matchResult("light", 100, ""),
		`{"kind":"work","id":"colour","score":100,`+hit+`}`)}
	cands, err := m.service(t).Candidates(context.Background(), MatchQuery{Title: "x", BookASIN: "B0COLOUR01", Limit: 2, BestOnly: true, NoDetailsBelow: 60})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.calls(); !reflect.DeepEqual(got, map[string]int{"match": 1}) {
		t.Fatalf("requests = %v, want only the match", got)
	}
	if len(cands) != 2 || cands[0].WorkID != "colour" || !cands[0].IdentifierHit() || cands[0].RecordingID != "nigel" || cands[1].WorkID != "light" {
		t.Fatalf("candidates = %+v", cands)
	}
}

// A work inlined on a result that isn't the best by Candidates' rule is no answer for
// the best: its details are fetched.
func TestBestOnlyIgnoresAWorkInlinedElsewhere(t *testing.T) {
	m := &bestMeta{
		body:   matchBody(matchResult("light", 71, inlined(otherWork, "")), matchResult("colour", 97, "")),
		works:  map[string]string{"colour": bestWork},
		series: bestSeries,
	}
	cands, err := m.service(t).Candidates(context.Background(), bestQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 || cands[0].Title != "The Colour of Magic" || m.calls()["work:colour"] != 1 {
		t.Fatalf("candidates = %+v, requests %v", cands, m.calls())
	}
}

// An older metaserve inlines nothing: the best's details are fetched as before, the
// runner-up's are not, and the facts stay cached for the next book.
func TestBestOnlyFallsBackWithoutInlining(t *testing.T) {
	m := &bestMeta{
		body:   matchBody(matchResult("colour", 97, ""), matchResult("light", 71, "")),
		works:  map[string]string{"colour": bestWork, "light": otherWork},
		series: bestSeries,
	}
	s := m.service(t)
	for range 2 {
		cands, err := s.Candidates(context.Background(), bestQuery)
		if err != nil {
			t.Fatal(err)
		}
		if len(cands) != 2 || cands[0].Title != "The Colour of Magic" || cands[1].Title != "" || cands[1].Score != 71 {
			t.Fatalf("candidates = %+v", cands)
		}
		if want := []MatchSeries{{ID: "discworld", Name: "Discworld", Position: "1"}}; !reflect.DeepEqual(cands[0].Series, want) {
			t.Fatalf("series = %+v", cands[0].Series)
		}
	}
	want := map[string]int{"match": 2, "work:colour": 2, "series:discworld": 1, "series:scheibenwelt": 1}
	if got := m.calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

// A best the caller would drop isn't read, unless an identifier found it, and nothing
// inlined is wasted.
func TestBestOnlyWeakBest(t *testing.T) {
	for name, tc := range map[string]struct {
		result    string
		wantWork  int
		wantTitle string
	}{
		"weak, not read":      {matchResult("colour", 59, ""), 0, ""},
		"at the floor, read":  {matchResult("colour", 60, ""), 1, "The Colour of Magic"},
		"weak but identified": {`{"kind":"work","id":"colour","score":40,"reasons":{"identifier":"isbn"}}`, 1, "The Colour of Magic"},
		"weak but inlined":    {matchResult("colour", 40, inlined(bestWork, bestFacts)), 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			m := &bestMeta{body: matchBody(tc.result), works: map[string]string{"colour": bestWork}, series: bestSeries}
			cands, err := m.service(t).Candidates(context.Background(), bestQuery)
			if err != nil {
				t.Fatal(err)
			}
			if len(cands) != 1 || cands[0].WorkID != "colour" || cands[0].Reasons == nil || cands[0].Title != tc.wantTitle {
				t.Fatalf("candidates = %+v", cands)
			}
			if got := m.calls()["work:colour"]; got != tc.wantWork {
				t.Fatalf("work fetched %d times, want %d", got, tc.wantWork)
			}
			if tc.wantWork == 0 && tc.wantTitle == "" && m.calls()["series:discworld"] != 0 {
				t.Fatalf("series read for a dropped best: %v", m.calls())
			}
		})
	}
}

// A series the facts don't name is unknown and drops nothing; nil facts are normal.
func TestBestOnlyMissingFactsDropNothing(t *testing.T) {
	both := []MatchSeries{{ID: "discworld", Name: "Discworld", Position: "1"}, {ID: "scheibenwelt", Name: "Scheibenwelt", Position: "1"}}
	for name, extra := range map[string]string{
		"no member":    inlined(bestWork, ""),
		"entry absent": inlined(bestWork, `{"discworld":{"language":"en"}}`),
		"null entry":   inlined(bestWork, `{"scheibenwelt":null}`),
	} {
		t.Run(name, func(t *testing.T) {
			m := &bestMeta{body: matchBody(matchResult("colour", 97, extra))}
			cands, err := m.service(t).Candidates(context.Background(), bestQuery)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cands[0].Series, both) {
				t.Fatalf("series = %+v, want both", cands[0].Series)
			}
			if got := m.calls(); !reflect.DeepEqual(got, map[string]int{"match": 1}) {
				t.Fatalf("requests = %v: work present means the facts were read", got)
			}
		})
	}
}

// A gone best gives way to the next; a best that can't be read is an outage at
// once (server#306), never the next standing in: Classify would see that one alone
// and could apply the wrong work unasked. With none left, a gone work is no match.
func TestBestOnlyBestCantBeRead(t *testing.T) {
	body := matchBody(matchResult("colour", 97, ""), matchResult("light", 71, ""))
	for name, tc := range map[string]struct {
		failing map[string]int
		works   map[string]string
		want    string
		wantErr bool
	}{
		"gone, the next stands in": {nil, map[string]string{"light": otherWork}, "light", false},
		"failing is an outage":     {map[string]int{"colour": 500}, map[string]string{"light": otherWork}, "", true},
		"gone, then failing":       {map[string]int{"light": 500}, nil, "", true},
		"all failing is an outage": {map[string]int{"colour": 500, "light": 500}, nil, "", true},
		"all gone is no match":     {nil, nil, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			m := &bestMeta{body: body, works: tc.works, failing: tc.failing}
			cands, err := m.service(t).Candidates(context.Background(), bestQuery)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if tc.want == "" {
				if len(cands) != 0 {
					t.Fatalf("candidates = %+v, want none", cands)
				}
				return
			}
			if len(cands) != 1 || cands[0].WorkID != tc.want || cands[0].Title == "" {
				t.Fatalf("candidates = %+v, want %s with its details", cands, tc.want)
			}
		})
	}
}

// A weak next never hides an outage: a best that can't be read is one at once, so the
// next is not asked (a stub asks nothing, and would answer for a service that is down,
// the book a weak match and no outage), and a gone best leaves the weak next a stub.
func TestBestOnlyWeakNextDoesNotHideAnOutage(t *testing.T) {
	body := matchBody(matchResult("colour", 97, ""), matchResult("light", 40, ""))
	for name, tc := range map[string]struct {
		failing   map[string]int
		works     map[string]string
		wantTitle string
		wantErr   bool
	}{
		"both failing is an outage": {failing: map[string]int{"colour": 503, "light": 503}, wantErr: true},
		"the best failing":          {failing: map[string]int{"colour": 500}, works: map[string]string{"light": otherWork}, wantErr: true},
		"gone: no outage to hide":   {works: map[string]string{"light": otherWork}},
	} {
		t.Run(name, func(t *testing.T) {
			m := &bestMeta{body: body, works: tc.works, failing: tc.failing}
			cands, err := m.service(t).Candidates(context.Background(), bestQuery)
			if (err != nil) != tc.wantErr {
				t.Fatalf("candidates = %+v, err = %v, want error %v", cands, err, tc.wantErr)
			}
			if !tc.wantErr && (len(cands) != 1 || cands[0].WorkID != "light" || cands[0].Title != tc.wantTitle) {
				t.Fatalf("candidates = %+v, want light titled %q", cands, tc.wantTitle)
			}
			if got := m.calls()["work:light"]; got != 0 {
				t.Fatalf("the weak next was read %d times: a stub asks nothing", got)
			}
		})
	}
}

// An inlined work is judged as a fetched one is: no facts where a fetch reads none (no
// language and one series, here beside a variant order its record says it translates),
// and nothing kept of a series the work doesn't list.
func TestBestOnlyInlinedFactsRuleAsAFetchDoes(t *testing.T) {
	const work = `{"id":"saga","title":"Die Saga","authors":[],
		"series":[{"id":"die-saga","name":"Die Saga","position":"1"},{"id":"chrono","name":"Chrono","position":"1","ordering_of":"the-saga"}],"recordings":[]}`
	const facts = `{"die-saga":{"translation_of":["chrono"]},"elsewhere":{"language":"de"}}`
	series := map[string]string{"die-saga": `{"id":"die-saga","name":"Die Saga","translation_of":[{"id":"chrono","name":"Chrono"}]}`}
	want := []MatchSeries{{ID: "die-saga", Name: "Die Saga", Position: "1"}}
	for name, body := range map[string]string{
		"fetched": matchBody(matchResult("saga", 97, "")),
		"inlined": matchBody(matchResult("saga", 97, inlined(work, facts))),
	} {
		t.Run(name, func(t *testing.T) {
			m := &bestMeta{body: body, works: map[string]string{"saga": work}, series: series}
			s := m.service(t)
			cands, err := s.Candidates(context.Background(), bestQuery)
			if err != nil {
				t.Fatal(err)
			}
			if len(cands) != 1 || !reflect.DeepEqual(cands[0].Series, want) {
				t.Fatalf("candidates = %+v, want the series %+v", cands, want)
			}
			for _, id := range []string{"die-saga", "elsewhere"} {
				if _, hit, _ := cacheGet[seriesFacts](s.cache, nsSeries.key(id)); hit {
					t.Fatalf("the facts of %s were cached", id)
				}
			}
		})
	}
}

// The best candidate is the same whichever way it came, and of the runner-up the two
// things a run reads are too: the dialog's full answer, the fetches and the inlined
// work (the facts of a real answer included).
func TestBestOnlyAnswersAsTheFullPathDoes(t *testing.T) {
	works := map[string]string{"colour": bestWork, "light": otherWork}
	hit := `"cover_url":"https://c/c.jpg","reasons":{"title":1,"identifier":"isbn"},"recording_id":"nigel"`
	plain := matchBody(matchResult("light", 90, ""), `{"kind":"work","id":"colour","score":90,`+hit+`}`)
	rich := matchBody(matchResult("light", 90, ""), `{"kind":"work","id":"colour","score":90,`+hit+`,`+inlined(bestWork, bestFacts)+`}`)
	q := bestQuery
	q.BestOnly, q.NoDetailsBelow = false, 0
	full, err := (&bestMeta{body: plain, works: works, series: bestSeries}).service(t).Candidates(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"fetched": plain, "inlined": rich} {
		got, err := (&bestMeta{body: body, works: works, series: bestSeries}).service(t).Candidates(context.Background(), bestQuery)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(full) {
			t.Fatalf("%s: %d candidates, want %d", name, len(got), len(full))
		}
		got[0].WebURL = full[0].WebURL // each stub is on its own port
		if !reflect.DeepEqual(got[0], full[0]) {
			t.Fatalf("%s: best = %+v, want %+v", name, got[0], full[0])
		}
		if got[1].WorkID != full[1].WorkID || got[1].Score != full[1].Score || got[1].IdentifierHit() != full[1].IdentifierHit() {
			t.Fatalf("%s: runner-up = %+v, want the score and hit of %+v", name, got[1], full[1])
		}
	}
}

// The dialog lists every candidate: no expand, every work read as ever.
func TestDialogDoesNotAskForExpansion(t *testing.T) {
	m := &bestMeta{
		body:   matchBody(matchResult("colour", 97, ""), matchResult("light", 40, "")),
		works:  map[string]string{"colour": bestWork, "light": otherWork},
		series: bestSeries,
	}
	cands, err := m.service(t).Candidates(context.Background(), MatchQuery{Title: "The Colour of Magic", Duration: 420 * 60})
	if err != nil {
		t.Fatal(err)
	}
	if m.got.Has("expand") {
		t.Fatalf("the dialog sent expand=%q", m.got.Get("expand"))
	}
	if len(cands) != 2 || cands[0].Title == "" || cands[1].Title == "" || m.calls()["work:light"] != 1 {
		t.Fatalf("candidates = %+v, requests %v", cands, m.calls())
	}
}

// A metaserve without the route is the frozen search, whatever the flag says: it has
// no scores to skip a fetch by.
func TestBestOnlyLeavesTheSearchFallback(t *testing.T) {
	m := &structuredMeta{routeless: true}
	cands, err := m.service(t).Candidates(context.Background(), MatchQuery{Title: "Sharpe's Eagle", Limit: 2, BestOnly: true, NoDetailsBelow: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, searches, _ := m.calls(); searches != 1 || len(cands) != 1 || cands[0].Title == "" {
		t.Fatalf("searches %d, candidates %+v", searches, cands)
	}
}

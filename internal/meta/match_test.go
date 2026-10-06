package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestScoreCandidate(t *testing.T) {
	martian := &MatchCandidate{
		Title: "The Martian", Authors: []MetaPersonRef{{Name: "Andy Weir"}},
		Recordings: []MatchRecording{{RuntimeMin: 634}},
	}
	for name, tc := range map[string]struct {
		q    MatchQuery
		c    *MatchCandidate
		want int
	}{
		"exact":            {MatchQuery{Title: "The Martian", Author: "Andy Weir", Duration: 634 * 60}, martian, 100},
		"other edition":    {MatchQuery{Title: "The Martian", Author: "Andy Weir", Duration: 680 * 60}, martian, 93},
		"untagged author":  {MatchQuery{Title: "The Martian", Duration: 634 * 60}, martian, 100},
		"no runtime known": {MatchQuery{Title: "The Martian", Author: "Andy Weir"}, martian, 100},
		"wrong book":       {MatchQuery{Title: "Artemis", Author: "Andy Weir", Duration: 540 * 60}, martian, 30},
		"subtitle":         {MatchQuery{Title: "Dune Messiah"}, &MatchCandidate{Title: "Dune", Subtitle: "Messiah"}, 100},
		"nothing to judge": {MatchQuery{}, martian, 0},
		// The book's tags carry fluff the community record doesn't.
		"unabridged":   {MatchQuery{Title: "The Martian (Unabridged)", Author: "Andy Weir", Duration: 634 * 60}, martian, 100},
		"series, book": {MatchQuery{Title: "Mistborn: The Final Empire (Book 1)", Series: "Mistborn", Author: "Brandon Sanderson"}, &MatchCandidate{Title: "The Final Empire", Authors: []MetaPersonRef{{Name: "Brandon Sanderson"}}}, 100},
		"co-written":   {MatchQuery{Title: "The Martian", Author: "Andy Weir, Mary Robinette Kowal"}, martian, 100},
		"initials":     {MatchQuery{Title: "The Hobbit", Author: "JRR Tolkien"}, &MatchCandidate{Title: "The Hobbit", Authors: []MetaPersonRef{{Name: "J. R. R. Tolkien"}}}, 100},
		// Different Cyrillic titles share only "и", "том" and "1" (3 of 7 words):
		// never the whole-title match an ASCII-only comparison would call it.
		"other script": {MatchQuery{Title: "Война и мир. Том 1"}, &MatchCandidate{Title: "Мастер и Маргарита. Том 1"}, 43},
	} {
		if got := scoreCandidate(tc.q, tc.c); got != tc.want {
			t.Errorf("%s: score = %d, want %d", name, got, tc.want)
		}
	}
}

// matchMock serves a search with two hits, one of which 404s and one of which
// fails, plus a lookup for an identifier.
func matchMock(t *testing.T, workCode map[string]int) *Service {
	t.Helper()
	return matchMockLookup(t, workCode, 0)
}

// matchMockLookup is matchMock with the lookup endpoint failing with lookupCode.
func matchMockLookup(t *testing.T, workCode map[string]int, lookupCode int) *Service {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/search", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":"dune"},{"id":"gone"},{"id":"broken"}]}`))
	})
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		if lookupCode != 0 {
			w.WriteHeader(lookupCode)
			return
		}
		_, _ = w.Write([]byte(`{"work":{"id":"dune","title":"Dune","cover_url":"https://c/dune.jpg"},"recording_id":"r2"}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if code := workCode[id]; code != 0 {
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + id + `","title":"Dune","authors":[{"id":"fh","name":"Frank Herbert"}],
			"series":[{"id":"dune","name":"Dune","position":"1"},{"id":"dune-chrono","name":"Dune (chronological)","position":"7","ordering_of":"dune"}],
			"recordings":[{"id":"r1","narrators":[],"runtime_min":1262,"asin":[{"region":"us","asin":"B002V1OF70"},{"region":"uk","asin":"B002V1OF70"}],"isbn":["9781427201430"]},
			              {"id":"r2","narrators":[],"runtime_min":600,"cover_url":"https://c/r2.jpg"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewService(srv.URL, nil)
}

func TestCandidates(t *testing.T) {
	s := matchMock(t, map[string]int{"gone": http.StatusNotFound, "broken": http.StatusInternalServerError})
	cands, err := s.Candidates(context.Background(), MatchQuery{Text: "dune", ASIN: "B002V1OF70", Title: "Dune", Duration: 1262 * 60})
	if err != nil {
		t.Fatal(err)
	}
	// The identifier hit and the search hit are one work; the 404 and the failure
	// are dropped rather than failing the whole search.
	if len(cands) != 1 {
		t.Fatalf("candidates = %+v", cands)
	}
	c := cands[0]
	if c.Score != 100 || c.RecordingID != "r2" || c.CoverURL != "https://c/dune.jpg" || len(c.Series) != 1 ||
		len(c.Recordings) != 2 || strings.Join(c.Recordings[0].ASINs, ",") != "B002V1OF70" ||
		c.Recordings[0].ISBNs[0] != "9781427201430" || c.Recordings[1].ASINs == nil || !strings.HasSuffix(c.WebURL, "id=dune") {
		t.Fatalf("candidate = %+v", c)
	}

	// Every expansion failing is an outage, not "no match".
	s = matchMock(t, map[string]int{"dune": 500, "gone": 500, "broken": 500})
	if _, err := s.Candidates(context.Background(), MatchQuery{Text: "dune"}); err == nil {
		t.Fatal("an all-failed fan-out must be an error")
	}
	// Only 404s is a clean empty answer.
	s = matchMock(t, map[string]int{"dune": 404, "gone": 404, "broken": 404})
	if cands, err := s.Candidates(context.Background(), MatchQuery{Text: "dune"}); err != nil || len(cands) != 0 {
		t.Fatalf("all-404 = %v %v, want empty", cands, err)
	}
}

// TestCandidatesOneLegFails: the identifier lookup failing still returns what the
// text search found; only both failing is an outage.
func TestCandidatesOneLegFails(t *testing.T) {
	s := matchMockLookup(t, map[string]int{"gone": 404, "broken": 404}, http.StatusInternalServerError)
	cands, err := s.Candidates(context.Background(), MatchQuery{Text: "dune", ASIN: "B002V1OF70", Title: "Dune"})
	if err != nil || len(cands) != 1 || cands[0].WorkID != "dune" || cands[0].RecordingID != "" {
		t.Fatalf("lookup down, search fine = %+v %v; want the search's candidate", cands, err)
	}
	if _, err := s.Candidates(context.Background(), MatchQuery{ASIN: "B002V1OF70"}); err == nil {
		t.Fatal("with only the lookup asked and it down, the error must surface")
	}
}

// matchServer is a metaserve stub for one Candidates call: it records the search
// text and lookup identifiers it was sent, answers search and lookup with the given
// codes (200 serves one hit, "dune"), and serves works/{id} with workCode.
type matchServer struct {
	searchCode, lookupCode, workCode int
	gotQ, gotASIN, gotISBN           string
}

func (m *matchServer) service(t *testing.T) *Service {
	t.Helper()
	answer := func(w http.ResponseWriter, code int, body string) {
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write([]byte(body))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/search", func(w http.ResponseWriter, r *http.Request) {
		m.gotQ = r.URL.Query().Get("q")
		answer(w, m.searchCode, `{"results":[{"id":"dune"}]}`)
	})
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		m.gotASIN, m.gotISBN = r.URL.Query().Get("asin"), r.URL.Query().Get("isbn")
		answer(w, m.lookupCode, `{"work":{"id":"dune","title":"Dune"},"recording_id":"r1"}`)
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		answer(w, m.workCode, `{"id":"dune","title":"Dune","authors":[{"id":"fh","name":"Frank Herbert"}],"recordings":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewService(srv.URL, nil)
}

// TestCandidatesSearchesTheCleanTitle: with no query the book's own title is
// searched without its series name and edition fluff, since metaserve's search
// requires every word and the community record has none of "(Unabridged)".
func TestCandidatesSearchesTheCleanTitle(t *testing.T) {
	m := &matchServer{searchCode: 200, lookupCode: 200, workCode: 200}
	if _, err := m.service(t).Candidates(context.Background(), MatchQuery{
		Title: "Dune: Dune Chronicles, Book 1 (Unabridged)", Series: "Dune Chronicles", Author: "Frank Herbert",
	}); err != nil {
		t.Fatal(err)
	}
	if m.gotQ != "Dune Frank Herbert" {
		t.Fatalf("searched %q, want %q", m.gotQ, "Dune Frank Herbert")
	}
}

// TestCandidatesNormalizesIdentifiers: an ASIN or ISBN as an admin pastes it
// (lowercase, or hyphenated as printed) is looked up in the form metaserve holds.
func TestCandidatesNormalizesIdentifiers(t *testing.T) {
	m := &matchServer{searchCode: 200, lookupCode: 200, workCode: 200}
	s := m.service(t)
	if _, err := s.Candidates(context.Background(), MatchQuery{ASIN: " b002v1of70 "}); err != nil || m.gotASIN != "B002V1OF70" {
		t.Fatalf("asin looked up as %q (%v)", m.gotASIN, err)
	}
	if _, err := s.Candidates(context.Background(), MatchQuery{ISBN: "978-1-4272-0143-0"}); err != nil || m.gotISBN != "9781427201430" {
		t.Fatalf("isbn looked up as %q (%v)", m.gotISBN, err)
	}
}

// TestCandidatesReportsALegOutage: one leg down while the other's hits all turn out
// gone (404 on expansion) leaves nothing to show, so it is the outage that is
// reported, never "no match" - the leg that failed may well have had the book.
func TestCandidatesReportsALegOutage(t *testing.T) {
	q := MatchQuery{Text: "dune", ASIN: "B002V1OF70"}
	for name, m := range map[string]*matchServer{
		"search down": {searchCode: 500, lookupCode: 200, workCode: 404},
		"lookup down": {searchCode: 200, lookupCode: 500, workCode: 404},
	} {
		if cands, err := m.service(t).Candidates(context.Background(), q); err == nil {
			t.Errorf("%s: %d candidates and no error, want the outage", name, len(cands))
		}
	}
	// Both legs fine and every hit gone is a clean "no match".
	m := &matchServer{searchCode: 200, lookupCode: 200, workCode: 404}
	if cands, err := m.service(t).Candidates(context.Background(), q); err != nil || len(cands) != 0 {
		t.Fatalf("all hits gone = %v %v, want an empty answer", cands, err)
	}
}

// structuredMeta is a metaserve stub for the structured-match leg. matchCode
// picks how works/match answers: 200 serves matchBody, any other code is
// written as the status, and routeless leaves the route unregistered, so the
// request falls through to works/{id} exactly as on a metaserve that predates
// it. It records what each endpoint was sent.
type structuredMeta struct {
	matchCode int
	matchBody string
	routeless bool

	mu                      sync.Mutex
	gotMatch                url.Values
	matchCalls, searchCalls int
	lookupCalls             int
	gotQ                    string
}

// seen returns what the stub recorded, under its lock (the handlers ran on the
// server's goroutines).
func (m *structuredMeta) seen() (match url.Values, searchCalls int, q string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gotMatch, m.searchCalls, m.gotQ
}

func (m *structuredMeta) calls() (match, search, lookup int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.matchCalls, m.searchCalls, m.lookupCalls
}

func (m *structuredMeta) service(t *testing.T) *Service {
	t.Helper()
	mux := http.NewServeMux()
	if !m.routeless {
		mux.HandleFunc("GET /api/v1/works/match", func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			m.gotMatch = r.URL.Query()
			m.matchCalls++
			m.mu.Unlock()
			if m.matchCode != http.StatusOK {
				w.WriteHeader(m.matchCode)
				return
			}
			_, _ = w.Write([]byte(m.matchBody))
		})
	}
	mux.HandleFunc("GET /api/v1/works/search", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.searchCalls++
		m.gotQ = r.URL.Query().Get("q")
		m.mu.Unlock()
		_, _ = w.Write([]byte(`{"results":[{"id":"sharpes-eagle"}]}`))
	})
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		m.lookupCalls++
		m.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})
	// Only the one work exists, so "match" read as a work id is a 404 - the
	// answer an older metaserve gives the new route.
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "sharpes-eagle" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"sharpes-eagle","title":"Sharpe’s Eagle","authors":[{"id":"bernard-cornwell","name":"Bernard Cornwell"}],
			"series":[{"id":"richard-sharpe-novels","name":"Richard Sharpe Novels","position":"8"}],
			"recordings":[{"id":"rupert-farley-2014","narrators":[],"runtime_min":660,"asin":[{"region":"us","asin":"B002SQ7KVE"}]}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewService(srv.URL, nil)
}

// sharpeBook is the motivating case: tags that are garbage (the author's name
// as the title, the title as the author) in a folder layout that is good.
var sharpeBook = MatchQuery{
	Title: "Bernard Cornwell", Author: "Sharpe's Eagle (Sharpe 08)", Duration: 39600,
	Path: "Bernard Cornwell/Richard Sharpe/Sharpe - 08 - Sharpe's Eagle", IsFolder: true,
}

const sharpeMatch = `{"results":[{"kind":"work","id":"sharpes-eagle","title":"Sharpe’s Eagle","score":97,
	"recording_id":"rupert-farley-2014","reasons":{"title":1,"author":"full","series":"position","runtime":0.01}}]}`

// TestCandidatesUsesTheStructuredMatch: the book's tag AND path facts go to
// works/match, path guesses first and numbering left on for metaserve to read,
// and metaserve's score and reasons are what the console shows. The older
// search is not consulted, and a recording metaserve picked by runtime is not
// passed off as an identifier's.
func TestCandidatesUsesTheStructuredMatch(t *testing.T) {
	m := &structuredMeta{matchCode: http.StatusOK, matchBody: sharpeMatch}
	cands, err := m.service(t).Candidates(context.Background(), sharpeBook)
	if err != nil {
		t.Fatal(err)
	}
	gotMatch, searchCalls, _ := m.seen()
	want := url.Values{
		"title":   {"Sharpe - 08 - Sharpe's Eagle", "Bernard Cornwell"},
		"author":  {"Bernard Cornwell", "Sharpe's Eagle (Sharpe 08)"},
		"series":  {"Richard Sharpe"},
		"runtime": {"39600"},
		"limit":   {"6"},
	}
	if !reflect.DeepEqual(gotMatch, want) {
		t.Errorf("works/match sent %v\nwant %v", gotMatch, want)
	}
	if searchCalls != 0 {
		t.Errorf("the older search ran %d times beside a working match", searchCalls)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %+v", cands)
	}
	c := cands[0]
	if c.WorkID != "sharpes-eagle" || c.Score != 97 || c.Reasons == nil || c.Reasons.Series != "position" ||
		c.Reasons.Author != "full" || *c.Reasons.Title != 1 || c.RecordingID != "" || len(c.Recordings) != 1 {
		t.Fatalf("candidate = %+v (reasons %+v)", c, c.Reasons)
	}
}

// TestCandidatesSendsTheTypedTextWithTheFacts: what the admin typed is one more
// guess, not a replacement for the book's own facts.
func TestCandidatesSendsTheTypedTextWithTheFacts(t *testing.T) {
	m := &structuredMeta{matchCode: http.StatusOK, matchBody: sharpeMatch}
	q := sharpeBook
	q.Text = "  sharpes eagle cornwell "
	if _, err := m.service(t).Candidates(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	gotMatch, _, _ := m.seen()
	if gotMatch.Get("q") != "sharpes eagle cornwell" || gotMatch.Get("series") != "Richard Sharpe" || len(gotMatch["title"]) != 2 {
		t.Fatalf("works/match sent %v", gotMatch)
	}
	// Tag-only facts fill in what the path does not say.
	q = MatchQuery{Title: "Dune", Series: "Dune Chronicles", SeriesIndex: 1, Author: "Frank Herbert", Path: "Dune.m4b"}
	if _, err := m.service(t).Candidates(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	gotMatch, _, _ = m.seen()
	if gotMatch.Get("series") != "Dune Chronicles" || gotMatch.Get("position") != "1" || gotMatch.Get("runtime") != "" {
		t.Fatalf("works/match sent %v", gotMatch)
	}
}

// TestCandidatesSendsTheBookIdentifiers: the book's own ASIN rides on the
// match, which looks it up itself, so no separate lookup runs, and the
// recording it names is the candidate's.
func TestCandidatesSendsTheBookIdentifiers(t *testing.T) {
	m := &structuredMeta{matchCode: http.StatusOK, matchBody: `{"results":[{"id":"sharpes-eagle","score":100,
		"recording_id":"rupert-farley-2014","reasons":{"identifier":"asin"}}]}`}
	q := sharpeBook
	q.BookASIN = "b002sq7kve"
	cands, err := m.service(t).Candidates(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	gotMatch, _, _ := m.seen()
	if _, _, lookups := m.calls(); gotMatch.Get("asin") != "B002SQ7KVE" || lookups != 0 {
		t.Fatalf("works/match sent %v, %d lookups", gotMatch, lookups)
	}
	if len(cands) != 1 || cands[0].Score != 100 || cands[0].RecordingID != "rupert-farley-2014" || cands[0].Reasons.Identifier != "asin" {
		t.Fatalf("candidates = %+v", cands)
	}
}

// TestCandidatesFallsBackWithoutTheMatchRoute: every way an older metaserve can
// answer works/match as a route it lacks leaves the dialog searching exactly as
// it did before, and the answer is remembered so the next match goes straight
// to the search.
func TestCandidatesFallsBackWithoutTheMatchRoute(t *testing.T) {
	for name, m := range map[string]*structuredMeta{
		"route unknown (works/{id} 404)": {routeless: true},
		"404":                            {matchCode: http.StatusNotFound},
		"405":                            {matchCode: http.StatusMethodNotAllowed},
		"200 without results":            {matchCode: http.StatusOK, matchBody: `{"id":"match","title":"Match"}`},
	} {
		t.Run(name, func(t *testing.T) {
			s := m.service(t)
			cands, err := s.Candidates(context.Background(), sharpeBook)
			if err != nil {
				t.Fatal(err)
			}
			_, searchCalls, gotQ := m.seen()
			if searchCalls != 1 || gotQ != "Bernard Cornwell Sharpe's Eagle (Sharpe 08)" {
				t.Fatalf("search ran %d times with %q, want once with the tags (today's query)", searchCalls, gotQ)
			}
			if len(cands) != 1 || cands[0].WorkID != "sharpes-eagle" || cands[0].Reasons != nil {
				t.Fatalf("candidates = %+v, want sharpes-eagle scored by the frozen scorer", cands)
			}
			before, _, _ := m.calls()
			if _, err := s.Candidates(context.Background(), sharpeBook); err != nil {
				t.Fatal(err)
			}
			if after, search, _ := m.calls(); after != before || search != 2 {
				t.Fatalf("second match: works/match %d -> %d calls, search %d; want it skipped", before, after, search)
			}
		})
	}
}

// TestCandidatesMatchOutageIsAnError: a metaserve that has the route but
// fails it (over its budget, a 5xx) is an outage the dialog shows, not a
// reason to answer from the weaker search - and the route is tried again next
// time.
func TestCandidatesMatchOutageIsAnError(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusInternalServerError} {
		m := &structuredMeta{matchCode: code}
		s := m.service(t)
		for range 2 {
			if cands, err := s.Candidates(context.Background(), sharpeBook); err == nil {
				t.Fatalf("%d: candidates %+v, want an error", code, cands)
			}
		}
		if matches, searches, _ := m.calls(); matches != 2 || searches != 0 {
			t.Fatalf("%d: works/match %d calls, search %d; want 2 and 0", code, matches, searches)
		}
	}
}

// TestCandidatesTypedIdentifierSkipsTheMatch: an ASIN the admin typed asks for
// that record, so no fact match runs beside it.
func TestCandidatesTypedIdentifierSkipsTheMatch(t *testing.T) {
	m := &structuredMeta{matchCode: http.StatusOK, matchBody: sharpeMatch}
	if _, err := m.service(t).Candidates(context.Background(), MatchQuery{ASIN: "B002SQ7KVE", Title: "Dune"}); err != nil {
		t.Fatal(err)
	}
	gotMatch, searchCalls, _ := m.seen()
	if gotMatch != nil || searchCalls != 0 {
		t.Fatalf("match %v / search %d ran for a typed identifier", gotMatch, searchCalls)
	}
}

// TestCandidatesCancelled: a request whose context is gone is an error, and is
// not retried as a search - that would only spend the same dead context again.
func TestCandidatesCancelled(t *testing.T) {
	m := &structuredMeta{matchCode: http.StatusOK, matchBody: sharpeMatch}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if cands, err := m.service(t).Candidates(ctx, sharpeBook); err == nil {
		t.Fatalf("cancelled = %+v, want an error", cands)
	}
	if _, searchCalls, _ := m.seen(); searchCalls != 0 {
		t.Fatalf("a cancelled match fell back to search %d times", searchCalls)
	}
}

// TestMatchParamsSendsBothSeries: the series folder and the tagged series both
// go up, folder first, when they differ ("Fiction/Andy Weir/Artemis" puts the
// author's folder in the series slot), and once when they fold alike.
func TestMatchParamsSendsBothSeries(t *testing.T) {
	cases := []struct {
		name string
		q    MatchQuery
		want []string
	}{
		{"a different tag beside the folder", MatchQuery{Series: "Artemis", Path: "Fiction/Andy Weir/Artemis", IsFolder: true},
			[]string{"Andy Weir", "Artemis"}},
		{"the same series", MatchQuery{Series: "richard  SHARPE", Path: sharpeBook.Path, IsFolder: true},
			[]string{"Richard Sharpe"}},
		{"no series folder", MatchQuery{Series: "Dune Chronicles", Path: "Frank Herbert/Dune", IsFolder: true},
			[]string{"Dune Chronicles"}},
		{"no tag", MatchQuery{Path: sharpeBook.Path, IsFolder: true}, []string{"Richard Sharpe"}},
		{"the same series under a numbered folder", MatchQuery{Series: "Tawny Man", IsFolder: true,
			Path: "Robin Hobb/03 - Tawny Man/Golden Fool"}, []string{"03 - Tawny Man"}},
	}
	for _, c := range cases {
		if got := matchParams(c.q, "", "", "")["series"]; !slices.Equal(got, c.want) {
			t.Errorf("%s: series = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestMatchParamsPairsThePositionWithItsSeries: metaserve applies one position
// to every series guess, and a stated one replaces the leaf's numbering, so the
// tagged position goes up only when no DIFFERENT series folder does: beside one
// it would turn the folder's agreement ("03 - Tawny Man", leaf "TM02") into a
// conflict.
func TestMatchParamsPairsThePositionWithItsSeries(t *testing.T) {
	cases := []struct {
		name     string
		q        MatchQuery
		position string
	}{
		{"a different series folder", MatchQuery{Series: "Realm of the Elderlings", SeriesIndex: 15, IsFolder: true,
			Path: "Robin Hobb/Realms of the Elderlings/03 - Tawny Man/TM02 - Golden Fool"}, ""},
		{"an agreeing series folder", MatchQuery{Series: "richard sharpe", SeriesIndex: 8, IsFolder: true, Path: sharpeBook.Path}, "8"},
		{"no series folder", MatchQuery{Series: "Dune Chronicles", SeriesIndex: 1, IsFolder: true, Path: "Frank Herbert/Dune"}, "1"},
		{"no series tag", MatchQuery{SeriesIndex: 8, IsFolder: true, Path: sharpeBook.Path}, "8"},
		{"an agreeing numbered series folder", MatchQuery{Series: "Tawny Man", SeriesIndex: 2, IsFolder: true,
			Path: "Robin Hobb/03 - Tawny Man/Golden Fool"}, "2"},
		// A bare-number leaf is the volume of the series folder holding it.
		{"a path volume", MatchQuery{IsFolder: true, Path: "Brandon Sanderson/Stormlight Archive/03"}, "3"},
		{"a path volume beside an agreeing tag", MatchQuery{Series: "Stormlight Archive", SeriesIndex: 2, IsFolder: true,
			Path: "Brandon Sanderson/Stormlight Archive/03"}, "3"},
		{"a path volume beside a different tag", MatchQuery{Series: "The Cosmere", SeriesIndex: 9, IsFolder: true,
			Path: "Brandon Sanderson/Stormlight Archive/03"}, ""},
	}
	for _, c := range cases {
		if got := matchParams(c.q, "", "", "").Get("position"); got != c.position {
			t.Errorf("%s: position = %q, want %q", c.name, got, c.position)
		}
	}
}

// TestCandidatesWithNoFactsSendsNoMatch: a book with nothing to match on is an
// empty answer, not the 400 metaserve gives a request naming nothing.
func TestCandidatesWithNoFactsSendsNoMatch(t *testing.T) {
	m := &structuredMeta{matchCode: http.StatusBadRequest}
	cands, err := m.service(t).Candidates(context.Background(), MatchQuery{Path: "Track 01.mp3", Duration: 600})
	if err != nil || len(cands) != 0 {
		t.Fatalf("candidates = %+v, %v; want an empty answer", cands, err)
	}
	if matches, searches, lookups := m.calls(); matches+searches+lookups != 0 {
		t.Fatalf("sent %d matches, %d searches, %d lookups for a book with no facts", matches, searches, lookups)
	}
}

// TestMatchParamsDropsAnOverlongIdentifier: metaserve ignores an identifier
// past its limit, so a garbage tag is not sent as the request's only fact.
func TestMatchParamsDropsAnOverlongIdentifier(t *testing.T) {
	v := matchParams(MatchQuery{Path: "Track 01.mp3"}, "", strings.Repeat("B", 21), "")
	if hasMatchFacts(v) {
		t.Fatalf("works/match sent %v for an overlong ASIN alone", v)
	}
	if v := matchParams(MatchQuery{}, "", "B002SQ7KVE", ""); v.Get("asin") != "B002SQ7KVE" {
		t.Fatalf("works/match sent %v, want the ASIN", v)
	}
}

// TestCandidatesNullResultsIsAnEmptyAnswer: a works/match 200 whose results are
// null (a Go encoder's nil slice) is an empty answer from a metaserve that HAS
// the route, not a missing route: no fallback search, nothing remembered.
func TestCandidatesNullResultsIsAnEmptyAnswer(t *testing.T) {
	m := &structuredMeta{matchCode: http.StatusOK, matchBody: `{"results":null}`}
	s := m.service(t)
	for range 2 {
		cands, err := s.Candidates(context.Background(), sharpeBook)
		if err != nil || len(cands) != 0 {
			t.Fatalf("candidates = %+v, %v; want an empty answer", cands, err)
		}
	}
	if matches, searches, _ := m.calls(); matches != 2 || searches != 0 {
		t.Fatalf("works/match %d calls, search %d; want 2 and 0", matches, searches)
	}
}

// TestMatchParamsBoundsEachValue: a tag longer than metaserve reads is cut to
// what it reads, on a rune boundary, so the URL stays short.
func TestMatchParamsBoundsEachValue(t *testing.T) {
	long := strings.Repeat("é", 200) // 400 bytes
	got := matchParams(MatchQuery{Title: long, Author: long}, "", "", "")
	for _, k := range []string{"title", "author"} {
		v := got.Get(k)
		if len(v) > maxMatchValueBytes || !utf8.ValidString(v) || v == "" {
			t.Errorf("%s = %d bytes (valid %v), want at most %d valid bytes", k, len(v), utf8.ValidString(v), maxMatchValueBytes)
		}
	}
}

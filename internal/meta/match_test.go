package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

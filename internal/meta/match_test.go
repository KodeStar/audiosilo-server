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

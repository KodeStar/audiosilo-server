package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestOrderASINs(t *testing.T) {
	// As metaserve lists them: by region, so au and ca come before uk and us.
	refs := []ASINRef{
		{"au", "B0AU000001"}, {"ca", "B0CA000001"}, {"uk", "B0UK000001"}, {"us", "B0US000001"},
		{"us", "B0US000002"},
	}
	for name, tc := range map[string]struct {
		refs   []ASINRef
		region string
		want   string
	}{
		"preferred first":      {refs, "uk", "B0UK000001,B0US000001,B0US000002,B0AU000001,B0CA000001"},
		"no preference: us":    {refs, "", "B0US000001,B0US000002,B0AU000001,B0CA000001,B0UK000001"},
		"preferred missing":    {refs, "de", "B0US000001,B0US000002,B0AU000001,B0CA000001,B0UK000001"},
		"neither: as listed":   {[]ASINRef{{"au", "B0AU000001"}, {"de", "B0DE000001"}}, "uk", "B0AU000001,B0DE000001"},
		"one id, many regions": {[]ASINRef{{"au", "0008460639"}, {"uk", "0008460639"}, {"us", "0008460639"}}, "uk", "0008460639"},
		"none":                 {nil, "uk", ""},
	} {
		if got := strings.Join(orderASINs(tc.refs, tc.region), ","); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

func TestDefaultRecording(t *testing.T) {
	c := &MatchCandidate{Recordings: []MatchRecording{
		{ID: "us-only", RuntimeMin: 600, ASINRefs: []ASINRef{{"us", "B0US000001"}}},
		{ID: "uk-too", RuntimeMin: 600, ASINRefs: []ASINRef{{"us", "B0US000002"}, {"uk", "B0UK000002"}}},
		{ID: "abridged", RuntimeMin: 300},
		{ID: "unknown"},
	}}
	pick := func(seconds float64, region string) string {
		if r := DefaultRecording(c, seconds, region); r != nil {
			return r.ID
		}
		return ""
	}
	if got := pick(600*60, "uk"); got != "uk-too" {
		t.Errorf("a tie goes to the preferred region's recording, got %s", got)
	}
	if got := pick(600*60, ""); got != "us-only" {
		t.Errorf("no preference keeps the first of a tie, got %s", got)
	}
	if got := pick(310*60, "uk"); got != "abridged" {
		t.Errorf("the closest runtime wins over the region, got %s", got)
	}
	c.RecordingID = "unknown"
	if got := pick(600*60, "uk"); got != "unknown" {
		t.Errorf("an identifier's recording wins, got %s", got)
	}
	if DefaultRecording(&MatchCandidate{}, 60, "uk") != nil {
		t.Error("no recordings, no pick")
	}
}

// TestCandidatesRegionAndLimit: the candidates carry each ASIN with its
// marketplace and list the preferred one's first, and a bulk query's Limit both
// asks metaserve for fewer works and expands no more.
func TestCandidatesRegionAndLimit(t *testing.T) {
	var (
		mu      sync.Mutex
		limit   string
		fetched int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/match", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		limit = r.URL.Query().Get("limit")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"results":[{"id":"a","score":95,"reasons":{}},{"id":"b","score":60,"reasons":{}},{"id":"c","score":40,"reasons":{}}]}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetched++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"` + r.PathValue("id") + `","title":"T","recordings":[{"id":"r","runtime_min":600,
			"asin":[{"region":"au","asin":"B0AU000001"},{"region":"uk","asin":"B0UK000001"},{"region":"us","asin":"B0US000001"}]}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := NewService(srv.URL, nil)

	cands, err := s.Candidates(context.Background(), MatchQuery{Title: "T", Region: "uk", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotLimit, gotFetched := limit, fetched
	mu.Unlock()
	if gotLimit != "2" || gotFetched != 2 || len(cands) != 2 {
		t.Fatalf("limit=%s fetched=%d candidates=%d, want 2/2/2", gotLimit, gotFetched, len(cands))
	}
	rec := cands[0].Recordings[0]
	if strings.Join(rec.ASINs, ",") != "B0UK000001,B0US000001,B0AU000001" || len(rec.ASINRefs) != 3 ||
		rec.ASINRefs[0] != (ASINRef{"au", "B0AU000001"}) || rec.RegionASIN("uk") != "B0UK000001" || rec.RegionASIN("de") != "" {
		t.Fatalf("recording = %+v", rec)
	}

	// The dialog's own query (no limit) still expands up to maxMatchCandidates.
	if _, err := s.Candidates(context.Background(), MatchQuery{Title: "T"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if limit != "6" || fetched != 5 {
		t.Fatalf("dialog query: limit=%s fetched=%d, want 6 and the 3 hits expanded", limit, fetched)
	}
}

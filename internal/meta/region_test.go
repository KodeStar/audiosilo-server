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

// TestASINRegion: the first ASIN is named by the marketplace that put it first,
// not by the first region listing it (a worldwide id is listed au first).
func TestASINRegion(t *testing.T) {
	world := []ASINRef{{"au", "0008460639"}, {"uk", "0008460639"}, {"us", "0008460639"}}
	for name, tc := range map[string]struct {
		refs         []ASINRef
		region, want string
	}{
		"preferred":         {world, "uk", "uk"},
		"no preference: us": {world, "", "us"},
		"preferred missing": {world, "de", "us"},
		"neither":           {[]ASINRef{{"au", "B0AU000001"}, {"ca", "B0AU000001"}}, "uk", "au"},
	} {
		asins := orderASINs(tc.refs, tc.region)
		if got := asinRegion(tc.refs, asins[0], tc.region); got != tc.want {
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
		if r := DefaultRecording(c, seconds, region, nil); r != nil {
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
	if DefaultRecording(&MatchCandidate{}, 60, "uk", []string{"Ray Porter"}) != nil {
		t.Error("no recordings, no pick")
	}
}

// TestDefaultRecordingPrefersTheNarrator: The Martian read by Wil Wheaton (the
// newer recording) and by R. C. Bray. The book's narrator picks the recording
// before the runtime does, after an identifier's; a narrator no recording
// names, or none, leaves the runtime to choose.
func TestDefaultRecordingPrefersTheNarrator(t *testing.T) {
	people := func(names ...string) []MetaPersonRef {
		out := []MetaPersonRef{}
		for _, n := range names {
			out = append(out, MetaPersonRef{Name: n})
		}
		return out
	}
	c := &MatchCandidate{Recordings: []MatchRecording{
		{ID: "bray", RuntimeMin: 653, Narrators: people("R. C. Bray")},
		{ID: "wheaton", RuntimeMin: 660, Narrators: people("Wil Wheaton")},
		{ID: "cast", RuntimeMin: 700, Narrators: people("Ann Example", "R C Bray")},
	}}
	pick := func(narrators ...string) string {
		if r := DefaultRecording(c, 653*60, "", narrators); r != nil {
			return r.ID
		}
		return ""
	}
	for want, narrators := range map[string][]string{
		"wheaton": {"Wil Wheaton"},
		"bray":    {"RC Bray"}, // folds alike; of the two he reads, the closer runtime
		"cast":    {"Ann Example"},
	} {
		if got := pick(narrators...); got != want {
			t.Errorf("narrated by %v: %s, want %s", narrators, got, want)
		}
	}
	if got := pick("Somebody Else"); got != "bray" {
		t.Errorf("a narrator no recording names: %s, want the runtime's bray", got)
	}
	if got := pick(); got != "bray" {
		t.Errorf("no narrator: %s, want the runtime's bray", got)
	}
	// His recording of another length is another edition: the runtime decides.
	short := &MatchCandidate{Recordings: []MatchRecording{
		{ID: "abridged-wheaton", RuntimeMin: 300, Narrators: people("Wil Wheaton")},
		{ID: "bray", RuntimeMin: 650, Narrators: people("R. C. Bray")},
	}}
	if r := DefaultRecording(short, 653*60, "", []string{"Wil Wheaton"}); r == nil || r.ID != "bray" {
		t.Errorf("a narrator's recording of another length: %v, want bray", r)
	}
	c.RecordingID = "cast"
	if got := pick("Wil Wheaton"); got != "cast" {
		t.Errorf("an identifier's recording beats the narrator: %s", got)
	}
}

func TestRegionLanguage(t *testing.T) {
	for region, want := range map[string]string{"us": "en", "uk": "en", "in": "en", "de": "de", "jp": "ja", "br": "pt", "": "", "xx": ""} {
		if got := RegionLanguage(region); got != want {
			t.Errorf("RegionLanguage(%q) = %q, want %q", region, got, want)
		}
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

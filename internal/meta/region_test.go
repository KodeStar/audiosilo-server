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
	t.Parallel()
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
	// A middle initial on either side is the same narrator.
	initials := &MatchCandidate{Recordings: []MatchRecording{
		{ID: "dietz", RuntimeMin: 653, Narrators: people("Norman Dietz")},
		{ID: "wilson", RuntimeMin: 660, Narrators: people("George K. Wilson")},
		{ID: "harding", RuntimeMin: 670, Narrators: people("Michael Harding")},
	}}
	for want, narrator := range map[string]string{"wilson": "George Wilson", "harding": "Michael A. Harding"} {
		if r := DefaultRecording(initials, 653*60, "", []string{narrator}); r == nil || r.ID != want {
			t.Errorf("narrated by %s: %v, want %s", narrator, r, want)
		}
	}
	c.RecordingID = "cast"
	if got := pick("Wil Wheaton"); got != "cast" {
		t.Errorf("an identifier's recording beats the narrator: %s", got)
	}
}

// TestCandidatesIdentifierHitLeads: at one score the work an identifier named
// comes first (metaserve breaks a tie on the title), and the ASIN that found it
// leads its marketplace's, so a book the store lists under two keeps its tags'
// when that store is the preferred one; another store's still comes first there.
func TestCandidatesIdentifierHitLeads(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/match", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":"other","score":100,"reasons":{"title":1}},
			{"id":"found","score":100,"recording_id":"r","reasons":{"identifier":"asin"}}]}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + r.PathValue("id") + `","title":"T","recordings":[{"id":"r","runtime_min":600,
			"asin":[{"region":"uk","asin":"B0UK000001"},{"region":"us","asin":"B0US000001"},{"region":"us","asin":"B0US000002"}]}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := NewService(srv.URL, nil)

	cands, err := s.Candidates(context.Background(), MatchQuery{Title: "T", BookASIN: "b0us000002", Region: "us"})
	if err != nil || len(cands) != 2 || cands[0].WorkID != "found" {
		t.Fatalf("candidates = %+v, %v; want the identifier's work first", cands, err)
	}
	rec := cands[0].Recordings[0]
	if strings.Join(rec.ASINs, ",") != "B0US000002,B0US000001,B0UK000001" || rec.ASINRegion != "us" || rec.RegionASIN("us") != "B0US000002" {
		t.Errorf("found recording = %+v, want the ASIN that found it first", rec)
	}
	if other := cands[1].Recordings[0]; other.ASINs[0] != "B0US000001" {
		t.Errorf("the other work's recording = %+v, want metaserve's order", other)
	}
	cands, err = s.Candidates(context.Background(), MatchQuery{Title: "T", BookASIN: "B0US000002", Region: "uk"})
	if err != nil || strings.Join(cands[0].Recordings[0].ASINs, ",") != "B0UK000001,B0US000002,B0US000001" {
		t.Errorf("preferring uk: %+v, %v; want its ASIN first, then the one that found it", cands, err)
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

func TestNormalizeLanguage(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"en": "en", " English ": "english", "eng": "eng", "en-GB": "en", "en_US": "en", "Deutsch": "deutsch",
		"pt-BR": "pt", "Klingon": "klingon", "français": "français",
		// The first of several, without a note.
		"eng;fre": "eng", "en/de": "en", "English (US)": "english", "en, de": "en",
		// Says no language.
		"und": "", "zxx": "", "mul": "", "mis": "", "XXX": "", "unknown": "", "None": "", "N/A": "", "NA": "",
		"": "", "e": "", "123": "", "?": "",
	} {
		if got := NormalizeLanguage(in); got != want {
			t.Errorf("NormalizeLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

// A plain book matches a work holding a dramatization, but doesn't take its recording.
func TestDefaultRecordingKeepsAPlainBookOffADramatization(t *testing.T) {
	plain := MatchRecording{ID: "plain", RuntimeMin: 700, Publisher: "Tantor Audio", Narrators: []MetaPersonRef{{Name: "Bea Reader"}}}
	ga := MatchRecording{ID: "ga", RuntimeMin: 400, Publisher: "Graphic Audio LLC", Narrators: []MetaPersonRef{{Name: "Alex Hill-Knight"}}}
	cast := MatchRecording{ID: "cast", RuntimeMin: 400, Publisher: "Corey Press", Narrators: []MetaPersonRef{{Name: "Full Cast"}}}
	bare := MatchRecording{ID: "bare", RuntimeMin: 400, Narrators: []MetaPersonRef{{Name: "Alex Hill-Knight"}}}
	long := MatchRecording{ID: "long", RuntimeMin: 900, Publisher: "GraphicAudio", Narrators: []MetaPersonRef{{Name: "Alex Hill-Knight"}}}
	both := MatchRecording{ID: "both", RuntimeMin: 420, Publisher: "Tantor Audio", Narrators: []MetaPersonRef{{Name: "Alex Hill-Knight"}}}
	many := MatchRecording{ID: "many", RuntimeMin: 400, Publisher: "Spoken Realms", Narrators: []MetaPersonRef{{Name: "A B"}, {Name: "C D"}, {Name: "E F"}, {Name: "G H"}}}
	cases := []struct {
		name      string
		c         MatchCandidate
		narrators []string
		want      string
		minutes   float64 // the book's length; 0 = unknown
	}{
		{"denied: nearer by length", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{plain, ga}}, nil, "plain", 400},
		{"denied: a full-cast credit", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{plain, cast}}, nil, "plain", 400},
		{"denied: only the dramatization, of another length", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{ga}}, nil, "", 410},
		{"allowed: only the dramatization, the book's length", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{ga}}, nil, "ga", 400},
		{"allowed: only the dramatization, within 1%", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{ga}}, nil, "ga", 403.9},
		{"allowed: only the dramatization, within two minutes", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{{ID: "short", RuntimeMin: 13, Publisher: "GraphicAudio"}}}, nil, "short", 14.9},
		{"denied: only the dramatization, past two minutes", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{{ID: "short", RuntimeMin: 13, Publisher: "GraphicAudio"}}}, nil, "", 15.1},
		{"denied: only dramatizations, the 1984 collection 1.9% off", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{{ID: "bbc", RuntimeMin: 670, Publisher: "GraphicAudio"}}}, nil, "", 683},
		{"allowed: only dramatizations, the one the length names", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{long, ga, cast}}, nil, "ga", 400},
		{"allowed: metaserve named another, the length names this one", MatchCandidate{PlainBook: true, RecordingID: "long", Recordings: []MatchRecording{long, ga}}, nil, "ga", 400},
		{"denied: beside a plain reading, the length names the dramatization", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{plain, ga}}, nil, "plain", 400},
		{"denied: unknown length", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{ga}}, nil, "", 0},
		{"denied: metaserve named it by length", MatchCandidate{PlainBook: true, RecordingID: "ga", Recordings: []MatchRecording{plain, ga}}, nil, "plain", 400},
		{"denied: no publisher on a labelled work", MatchCandidate{PlainBook: true, Title: "Dune [Dramatized Adaptation]", Recordings: []MatchRecording{bare}}, nil, "", 450},
		{"allowed: no publisher on a plain title", MatchCandidate{PlainBook: true, Title: "Dune", Recordings: []MatchRecording{bare}}, nil, "bare", 400},
		{"allowed: several readers are no dramatization", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{plain, many}}, nil, "many", 400},
		{"allowed: the book states it", MatchCandidate{Recordings: []MatchRecording{plain, ga}}, nil, "ga", 400},
		{"allowed: an identifier named it", MatchCandidate{PlainBook: true, RecordingID: "ga", Reasons: &MatchReasons{Identifier: "asin"}, Recordings: []MatchRecording{plain, ga}}, nil, "ga", 400},
		{"allowed: its narrator is the book's", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{plain, ga}}, []string{"Alex Hill-Knight"}, "ga", 400},
		{"denied: its narrator, but another length", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{plain, long}}, []string{"Alex Hill-Knight"}, "plain", 400},
		{"denied: a narrator on both prefers the plain reading", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{both, ga}}, []string{"Alex Hill-Knight"}, "both", 400},
		{"denied: metaserve named it, a narrator on both", MatchCandidate{PlainBook: true, RecordingID: "ga", Recordings: []MatchRecording{both, ga}}, []string{"Alex Hill-Knight"}, "both", 400},
		{"allowed: a narrator on both, the book states it", MatchCandidate{Recordings: []MatchRecording{both, ga}}, []string{"Alex Hill-Knight"}, "ga", 400},
		{"denied: a publisher under a title keeping (Dramatized)", MatchCandidate{PlainBook: true, Title: "The Idiot (Dramatized)", Recordings: []MatchRecording{plain}}, nil, "", 400},
		{"allowed: a publisher under the label, beside a dramatization", MatchCandidate{PlainBook: true, Title: "The Idiot (Dramatized Adaptation)", Recordings: []MatchRecording{plain, ga}}, nil, "plain", 400},
		{"denied: a publisher under the label, no recording saying so", MatchCandidate{PlainBook: true, Title: "The Idiot (Dramatized Adaptation)", Recordings: []MatchRecording{plain}}, nil, "", 400},
		{"denied: a publisher under a part's label", MatchCandidate{PlainBook: true, Title: "Warbreaker (1 of 3) [Dramatized Adaptation]", Recordings: []MatchRecording{plain}}, nil, "", 400},
		{"denied: a subtitle keeping the statement", MatchCandidate{PlainBook: true, Title: "Dune", Subtitle: "A Full-Cast Dramatization", Recordings: []MatchRecording{plain}}, nil, "", 400},
	}
	for _, tc := range cases {
		got := ""
		if r := DefaultRecording(&tc.c, tc.minutes*60, "", tc.narrators); r != nil {
			got = r.ID
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestFitRecording: a recording of another edition by length gives way to one of the
// work's that the length fits, the narrator's first, then the closest; a plain book
// never to a dramatization the length doesn't name; with none fitting, the length
// unknown, or the book's narrator reading it, the recording stays.
func TestFitRecording(t *testing.T) {
	t.Parallel()
	kramer := MatchRecording{ID: "kramer", RuntimeMin: 758, Narrators: []MetaPersonRef{{Name: "Michael Kramer"}}}
	ga := MatchRecording{ID: "ga", RuntimeMin: 564, Publisher: "GraphicAudio"}
	near := MatchRecording{ID: "near", RuntimeMin: 600, Narrators: []MetaPersonRef{{Name: "Ann Reader"}}}
	nearer := MatchRecording{ID: "nearer", RuntimeMin: 570}
	unknown := MatchRecording{ID: "unknown"}
	cases := []struct {
		name      string
		c         MatchCandidate
		rec       string
		narrators []string
		seconds   float64
		want      string
	}{
		{"swapped: the length fits the dramatization", MatchCandidate{Recordings: []MatchRecording{kramer, ga}}, "kramer", nil, 563 * 60, "ga"},
		{"swapped: the closest that fits", MatchCandidate{Recordings: []MatchRecording{kramer, near, nearer}}, "kramer", nil, 563 * 60, "nearer"},
		{"swapped: the narrator's that fits", MatchCandidate{Recordings: []MatchRecording{kramer, near, nearer}}, "kramer", []string{"Ann Reader"}, 563 * 60, "near"},
		{"kept: the book's narrator reads it", MatchCandidate{Recordings: []MatchRecording{kramer, ga}}, "kramer", []string{"Michael Kramer"}, 563 * 60, "kramer"},
		{"kept: it fits", MatchCandidate{Recordings: []MatchRecording{near, nearer}}, "near", nil, 563 * 60, "near"},
		{"kept: none fits", MatchCandidate{Recordings: []MatchRecording{kramer, unknown}}, "kramer", nil, 563 * 60, "kramer"},
		{"kept: the book's length unknown", MatchCandidate{Recordings: []MatchRecording{kramer, ga}}, "kramer", nil, 0, "kramer"},
		{"kept: a plain book, a dramatization 2% off", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{kramer, ga}}, "kramer", nil, 575 * 60, "kramer"},
		{"kept: a plain book beside a plain reading, even the length the dramatization's", MatchCandidate{PlainBook: true, Recordings: []MatchRecording{kramer, ga}}, "kramer", nil, 564 * 60, "kramer"},
	}
	for _, tc := range cases {
		var rec *MatchRecording
		for i := range tc.c.Recordings {
			if tc.c.Recordings[i].ID == tc.rec {
				rec = &tc.c.Recordings[i]
			}
		}
		got := ""
		if r := FitRecording(&tc.c, rec, tc.seconds, "", tc.narrators); r != nil {
			got = r.ID
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if FitRecording(&MatchCandidate{Recordings: []MatchRecording{ga}}, nil, 564*60, "", nil) != nil {
		t.Error("no recording picked stays none")
	}
}

func TestMatchQueryStatesDramatization(t *testing.T) {
	cases := []struct {
		q    MatchQuery
		want bool
	}{
		{MatchQuery{Title: "Dune [Dramatized Adaptation]"}, true},
		{MatchQuery{Title: "Dune", Series: "Dune (Full Cast)"}, true},
		{MatchQuery{Title: "Dune", Path: "Frank Herbert/Dune (Dramatised)"}, true},
		{MatchQuery{Title: "Dune", Narrators: []string{"Full Cast"}}, true},
		{MatchQuery{Text: "dune full-cast"}, true},
		{MatchQuery{Title: "Dune (GraphicAudio)"}, true},
		{MatchQuery{Title: "Dune", Path: "Graphic Audio/Dune"}, true},
		{MatchQuery{Title: "Graphic Novels of Audio"}, false},
		{MatchQuery{Title: "Dune", Narrators: []string{"Scott Brick"}, Path: "Frank Herbert/Dune"}, false},
		{MatchQuery{Title: "Radio Free Albemuth"}, false},
		{MatchQuery{Title: "The Spaceship", Genres: []string{"Radio Drama"}}, true},
		{MatchQuery{Title: "Paradise Lost", Genres: []string{"Comedy", "radio plays"}}, true},
		{MatchQuery{Title: "Die drei ???", Genres: []string{"Hörspiel"}}, true},
		{MatchQuery{Title: "Dune", Genres: []string{"Graphic Audio"}}, true},
		{MatchQuery{Title: "Riyria", Author: "Michael J. Sullivan - Graphic Audio"}, true},
		{MatchQuery{Title: "Dune", Genres: []string{"Audiobook", "Science Fiction & Fantasy", "Radio"}}, false},
		{MatchQuery{Title: "Dune", Genres: []string{"Drama"}}, false},
		{MatchQuery{ASIN: "B0DUNE0001"}, false},
	}
	for _, tc := range cases {
		if got := tc.q.statesDramatization(); got != tc.want {
			t.Errorf("%+v: states = %v, want %v", tc.q, got, tc.want)
		}
	}
}

// Through Candidates: the flag the console reads for a book that holds no recording.
func TestCandidatesFlagAWorkHoldingOnlyADramatization(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/search", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":"dune"}]}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"dune","title":"Dune","authors":[{"id":"fh","name":"Frank Herbert"}],
			"recordings":[{"id":"ga","narrators":[{"id":"a","name":"Alex Reader"}],"runtime_min":400,"publisher":"GraphicAudio"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := NewService(srv.URL, nil)
	for _, tc := range []struct {
		title     string
		minutes   float64
		only      bool
		recording string
	}{
		{"Dune", 430, true, ""},
		{"Dune", 400, false, "ga"}, // the length names the dramatization
		{"Dune [Dramatized Adaptation]", 430, false, "ga"},
	} {
		cands, err := s.Candidates(context.Background(), MatchQuery{Text: "dune", Title: tc.title, Duration: tc.minutes * 60})
		if err != nil || len(cands) != 1 {
			t.Fatalf("%s: candidates = %+v, %v", tc.title, cands, err)
		}
		if c := cands[0]; c.DramatizationOnly != tc.only || c.DefaultRecordingID != tc.recording {
			t.Errorf("%s: only = %v, default = %q; want %v, %q", tc.title, c.DramatizationOnly, c.DefaultRecordingID, tc.only, tc.recording)
		}
	}
}

// The same table as metaserve's TestTitleDramatizationCovers.
func TestTitleSpeaksFor(t *testing.T) {
	cases := []struct {
		title, subtitle string
		noPublisher     bool
		publisher       bool
	}{
		{title: "The Idiot"},
		{title: "Tracker (Dramatized Adaptation)", noPublisher: true},
		{title: "The Idiot (Dramatized)", noPublisher: true, publisher: true},
		{title: "The Wonderful Wizard of Oz: A Radio Dramatization", noPublisher: true, publisher: true},
		{title: "Dirk Gently: Two BBC Radio Full-Cast Dramas", noPublisher: true, publisher: true},
		{title: "Warbreaker (1 of 3) [Dramatized Adaptation]", noPublisher: true, publisher: true},
		{title: "Dune", subtitle: "A Full-Cast Dramatization", noPublisher: true, publisher: true},
		{title: "Dark Road Part Three (Dramatized Adaptation)", noPublisher: true, publisher: true},
	}
	for _, c := range cases {
		if got := titleSpeaksFor(c.title, c.subtitle, ""); got != c.noPublisher {
			t.Errorf("%q / %q: no publisher = %v, want %v", c.title, c.subtitle, got, c.noPublisher)
		}
		if got := titleSpeaksFor(c.title, c.subtitle, "Tantor Audio"); got != c.publisher {
			t.Errorf("%q / %q: a publisher = %v, want %v", c.title, c.subtitle, got, c.publisher)
		}
	}
}

package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// rail builds a main-view rail at position at, with entries "id@position".
func rail(at string, entries ...string) MetaSeries {
	r := MetaSeries{Position: at}
	for _, e := range entries {
		id, pos, _ := strings.Cut(e, "@")
		r.Works = append(r.Works, MetaSeriesWork{ID: id, Position: pos})
	}
	return r
}

func TestPreviousWorkIDs(t *testing.T) {
	cur := &MetaWork{ID: "cur"}
	for name, tc := range map[string]struct {
		series []MetaSeries
		want   []string
	}{
		"nearest first": {
			series: []MetaSeries{rail("4", "a@1", "b@2", "c@3", "cur@4", "d@5")},
			want:   []string{"c", "b", "a"},
		},
		"decimal positions sort numerically": {
			series: []MetaSeries{rail("3", "a@1", "n@2.5", "b@2", "x@10", "cur@3")},
			want:   []string{"n", "b", "a"},
		},
		"capped at five": {
			series: []MetaSeries{rail("9", "a@1", "b@2", "c@3", "d@4", "e@5", "f@6", "g@7", "h@8", "cur@9")},
			want:   []string{"h", "g", "f", "e", "d"},
		},
		"nearest across rails, rail order breaks ties, an id counted once at its nearest": {
			series: []MetaSeries{
				rail("3", "a@1", "b@2", "cur@3"),
				rail("2", "z@1", "cur@2"),
				rail("10", "a@9", "q@7", "cur@10"),
			},
			// distances: b=1, z=1, a=1 (rail 3), q=3; a's farther copy (rail 1, 2) drops.
			want: []string{"b", "z", "a", "q"},
		},
		"unparsable positions contribute nothing": {
			series: []MetaSeries{
				rail("1-3", "a@1"),        // the rail's own position
				rail("", "b@1"),           // blank
				rail("NaN", "c@1"),        // ParseFloat accepts it; we do not
				rail("4", "d@x", "e@3"),   // an entry's
				rail("Inf", "f@1", "g@2"), // nor the infinities
			},
			want: []string{"e"},
		},
		"the current work and later ones are never previous": {
			series: []MetaSeries{rail("2", "cur@1", "cur@2", "later@3")},
			want:   nil,
		},
		"no rails": {want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := PreviousWorkIDs(&Enrichment{Work: cur, Series: tc.series})
			if !slices.Equal(got, tc.want) {
				t.Fatalf("PreviousWorkIDs = %v, want %v", got, tc.want)
			}
		})
	}
	if got := PreviousWorkIDs(nil); got != nil {
		t.Fatalf("nil envelope = %v", got)
	}
}

// TestPreviousFetchesInOrderAndSkipsFailures: Previous resolves the ids through
// Work, keeps PreviousWorkIDs' order, and leaves a failed one out.
func TestPreviousFetchesInOrderAndSkipsFailures(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch id := r.PathValue("id"); id {
		case "gone":
			w.WriteHeader(http.StatusNotFound)
		case "down":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(`{"id":"` + id + `","title":"` + id + `","authors":[],"language":"en"}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	svc := NewService(srv.URL, nil)

	env := &Enrichment{Work: &MetaWork{ID: "cur"}, Series: []MetaSeries{rail("6", "a@1", "gone@2", "b@3", "down@4", "c@5", "cur@6")}}
	var ids []string
	for _, w := range svc.Previous(context.Background(), env) {
		ids = append(ids, w.ID)
	}
	if want := []string{"c", "b", "a"}; !slices.Equal(ids, want) {
		t.Fatalf("Previous = %v, want %v", ids, want)
	}
	if got := svc.Previous(context.Background(), &Enrichment{Work: &MetaWork{ID: "cur"}}); got != nil {
		t.Fatalf("no rails = %v, want nil", got)
	}
}

func TestChapterAt(t *testing.T) {
	starts := []float64{0, 100, 200}
	for _, tc := range []struct {
		starts   []float64
		position float64
		want     int
	}{
		{nil, 500, 0},
		{starts, 0, 0},
		{starts, -5, 0},
		{starts, 1, 1},
		{starts, 99, 1},
		{starts, 100, 2},
		{starts, 250, 3},
		{starts, 9999, 3},
		// A first chapter that does not start at 0: before it is chapter 0.
		{[]float64{30, 100}, 10, 0},
	} {
		if got := ChapterAt(tc.starts, tc.position); got != tc.want {
			t.Errorf("ChapterAt(%v, %v) = %d, want %d", tc.starts, tc.position, got, tc.want)
		}
	}
}

// spoilerWork is a current work with characters revealed at chapters 0, 1, 2
// and 5, recaps through chapters 0, 2 and 5, and a summary.
func spoilerWork() *MetaWork {
	return &MetaWork{
		ID: "cur",
		Characters: []MetaCharacter{
			{ID: "c0", Reveal: MetaPosition{Chapter: 0}},
			{ID: "c1", Reveal: MetaPosition{Chapter: 1}},
			{ID: "c2", Reveal: MetaPosition{Chapter: 2}},
			{ID: "c5", Reveal: MetaPosition{Chapter: 5}},
		},
		Recaps: []MetaRecap{
			{Through: MetaPosition{Chapter: 0}, Text: "r0"},
			{Through: MetaPosition{Chapter: 2}, Text: "r2"},
			{Through: MetaPosition{Chapter: 5}, Text: "r5"},
		},
		RecapSummary: &MetaRecapSummary{InShort: "short", Ending: "end"},
		Attribution:  &MetaAttribution{Credit: attributionContributors},
	}
}

func characterIDs(w *MetaWork) []string {
	var out []string
	for _, c := range w.Characters {
		out = append(out, c.ID)
	}
	return out
}

func recapTexts(w *MetaWork) []string {
	var out []string
	for _, r := range w.Recaps {
		out = append(out, r.Text)
	}
	return out
}

// TestHideSpoilersTruthTable pins the server's gating to the player's
// meta-gating.ts rules, chapter by chapter.
func TestHideSpoilersTruthTable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		chapter    int
		finished   bool
		characters []string
		recaps     []string
		summary    bool
	}{
		// Not started (or no chapters): the opening cast, the chapter-0 recaps.
		{"not started", 0, false, []string{"c0", "c1"}, []string{"r0"}, false},
		{"chapter 1", 1, false, []string{"c0", "c1"}, []string{"r0"}, false},
		// Inside chapter 2: its cast is met, but its recap is not over yet.
		{"chapter 2", 2, false, []string{"c0", "c1", "c2"}, []string{"r0"}, false},
		{"chapter 3", 3, false, []string{"c0", "c1", "c2"}, []string{"r0", "r2"}, false},
		{"chapter 5", 5, false, []string{"c0", "c1", "c2", "c5"}, []string{"r0", "r2"}, false},
		{"chapter 6", 6, false, []string{"c0", "c1", "c2", "c5"}, []string{"r0", "r2", "r5"}, false},
		// Finished reveals everything, at any chapter.
		{"finished", 1, true, []string{"c0", "c1", "c2", "c5"}, []string{"r0", "r2", "r5"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := HideSpoilers(&Enrichment{Work: spoilerWork()}, tc.chapter, tc.finished).Work
			if ids := characterIDs(got); !slices.Equal(ids, tc.characters) {
				t.Errorf("characters = %v, want %v", ids, tc.characters)
			}
			if texts := recapTexts(got); !slices.Equal(texts, tc.recaps) {
				t.Errorf("recaps = %v, want %v", texts, tc.recaps)
			}
			if (got.RecapSummary != nil) != tc.summary {
				t.Errorf("recap_summary = %+v, want present=%v", got.RecapSummary, tc.summary)
			}
			if got.Attribution == nil {
				t.Error("attribution dropped while community content remains")
			}
		})
	}
}

// TestHideSpoilersAttributionFollowsContent: a work whose every community
// entry is held back no longer carries the credit; a community description
// (spoiler-free) keeps it.
func TestHideSpoilersAttributionFollowsContent(t *testing.T) {
	w := &MetaWork{
		ID:           "cur",
		Characters:   []MetaCharacter{{ID: "late", Reveal: MetaPosition{Chapter: 9}}},
		RecapSummary: &MetaRecapSummary{Ending: "end"},
		Attribution:  &MetaAttribution{Credit: attributionContributors},
	}
	if got := HideSpoilers(&Enrichment{Work: w}, 1, false).Work; got.Attribution != nil || got.Characters != nil {
		t.Fatalf("fully gated work = %+v, want no characters and no attribution", got)
	}
	w.CommunityDescription = &MetaCommunityDescription{Text: "spoiler-free"}
	if got := HideSpoilers(&Enrichment{Work: w}, 1, false).Work; got.Attribution == nil || got.CommunityDescription == nil {
		t.Fatalf("description-only work = %+v, want description and attribution kept", got)
	}
}

// TestHideSpoilersPrevious: previous works keep their cast, recaps and
// in_short; only the ending goes, and a summary that was only an ending goes
// whole.
func TestHideSpoilersPrevious(t *testing.T) {
	full := spoilerWork()
	full.ID = "p1"
	endingOnly := &MetaWork{ID: "p2", RecapSummary: &MetaRecapSummary{Ending: "end"}, Attribution: &MetaAttribution{}}
	plain := &MetaWork{ID: "p3"}
	got := HideSpoilers(&Enrichment{Work: spoilerWork(), Previous: []*MetaWork{full, endingOnly, plain}}, 1, true).Previous
	if len(got) != 3 {
		t.Fatalf("previous = %+v", got)
	}
	if len(got[0].Characters) != 4 || len(got[0].Recaps) != 3 || got[0].RecapSummary == nil ||
		got[0].RecapSummary.InShort != "short" || got[0].RecapSummary.Ending != "" || got[0].Attribution == nil {
		t.Fatalf("previous[0] = %+v / %+v", got[0], got[0].RecapSummary)
	}
	if got[1].RecapSummary != nil || got[1].Attribution != nil {
		t.Fatalf("previous[1] = %+v, want no summary and no attribution", got[1])
	}
	if got[2] != plain {
		t.Fatal("an unchanged previous work should be shared, not copied")
	}
}

// TestHideSpoilersNeverMutates: the envelope Enrich returns is shared with the
// cache, so gating must leave its input exactly as it was.
func TestHideSpoilersNeverMutates(t *testing.T) {
	env := &Enrichment{Matched: true, Work: spoilerWork(), Previous: []*MetaWork{spoilerWork()}}
	before, _ := json.Marshal(env)
	work, prev := env.Work, env.Previous
	out := HideSpoilers(env, 1, false)
	after, _ := json.Marshal(env)
	if string(before) != string(after) || env.Work != work || !reflect.DeepEqual(env.Previous, prev) {
		t.Fatalf("input changed:\nbefore %s\nafter  %s", before, after)
	}
	if out == env || out.Work == env.Work || out.Previous[0] == env.Previous[0] {
		t.Fatal("gated parts must be copies")
	}
	if HideSpoilers(nil, 0, false) != nil {
		t.Fatal("nil envelope")
	}
}

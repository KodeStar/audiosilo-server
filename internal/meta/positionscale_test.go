package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// evenChapters is n chapters of 100 s each, on a whole-book timeline.
func evenChapters(n int) []metadata.Chapter {
	out := make([]metadata.Chapter, n)
	for i := range out {
		at := float64(i * 100)
		out[i] = metadata.Chapter{Index: i, Start: at, End: at + 100, BookOffset: at}
	}
	return out
}

// chaptersAt is a chapter at each start offset, the last running to total.
func chaptersAt(total float64, starts ...float64) []metadata.Chapter {
	out := make([]metadata.Chapter, len(starts))
	for i, at := range starts {
		end := total
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		out[i] = metadata.Chapter{Index: i, Start: at, End: end, BookOffset: at}
	}
	return out
}

// scaledWork is a work on a scale of length, with a character revealed and a recap
// through each of the positions.
func scaledWork(length int, positions ...int) *MetaWork {
	w := &MetaWork{ID: "w", PositionScale: &MetaPositionScale{Length: length, Unit: "act"},
		Attribution: &MetaAttribution{Credit: attributionContributors}}
	for _, p := range positions {
		w.Characters = append(w.Characters, MetaCharacter{ID: "c", Reveal: MetaPosition{Chapter: p}})
		w.Recaps = append(w.Recaps, MetaRecap{Through: MetaPosition{Chapter: p}, Text: "r"})
	}
	return w
}

func reveals(w *MetaWork) []int {
	var out []int
	for _, c := range w.Characters {
		out = append(out, c.Reveal.Chapter)
	}
	return out
}

func throughs(w *MetaWork) []int {
	var out []int
	for _, r := range w.Recaps {
		out = append(out, r.Through.Chapter)
	}
	return out
}

func TestOnRecordingTranslates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		length    int
		positions []int
		chapters  []metadata.Chapter
		duration  float64
		want      []int
	}{
		// Finer: 43 tracks of 100 s on 5 acts. Act 1 ends at 860 s and the first track
		// starting at or past it is the tenth (900 s).
		{"finer", 5, []int{1, 2, 3, 4, 5}, evenChapters(43), 4300, []int{10, 19, 27, 36, 43}},
		{"p=0 stays before this book", 5, []int{0}, evenChapters(43), 4300, []int{0}},
		{"p at the length is the last chapter", 5, []int{5}, evenChapters(43), 4300, []int{43}},
		{"p past the length is the last chapter", 5, []int{6, 99}, evenChapters(43), 4300, []int{43, 43}},
		// A start exactly on the fraction is at it.
		{"a chapter starting on the fraction", 2, []int{1}, evenChapters(4), 400, []int{3}},
		// Uneven: the first track holds three quarters of the book, so by time the first
		// three acts all end inside it or on its end and land on the second. A count would
		// say 3, 4, 6.
		{"uneven tracks go by time", 4, []int{1, 2, 3, 4}, chaptersAt(4000, 0, 3000, 3200, 3400, 3600, 3800), 4000, []int{2, 2, 2, 6}},
		// The other way round, a long last track: Act 1 ends on the fifth track's start and
		// the rest inside the last (a count would again say 3, 4, 6).
		{"a long last track", 4, []int{1, 2, 3}, chaptersAt(8000, 0, 500, 1000, 1500, 2000, 2500), 8000, []int{5, 6, 6}},
		// Coarser: 3 tracks on 5 acts. No track starts past 2/3, so the rest are the last.
		{"coarser", 5, []int{1, 2, 3, 4, 5}, evenChapters(3), 300, []int{2, 3, 3, 3, 3}},
		// Duration unknown: the last chapter's end stands in.
		{"no duration", 5, []int{1}, evenChapters(43), 0, []int{10}},
		// Times that can't be trusted: every chapter at 0 (an on-demand index), so by
		// count, equal shares (43/5 = 8.6, up to 9, the tenth starting after it).
		{"all offsets zero go by count", 5, []int{1, 2, 3, 4, 5}, make([]metadata.Chapter, 43), 4300, []int{10, 19, 27, 36, 43}},
		{"count, exact fraction", 2, []int{1}, make([]metadata.Chapter, 4), 0, []int{3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := OnRecording(scaledWork(tc.length, tc.positions...), tc.chapters, tc.duration)
			if r := reveals(got); !slices.Equal(r, tc.want) {
				t.Errorf("reveals = %v, want %v", r, tc.want)
			}
			if r := throughs(got); !slices.Equal(r, tc.want) {
				t.Errorf("throughs = %v, want %v", r, tc.want)
			}
			if got.PositionScale != nil {
				t.Errorf("position_scale = %+v beside translated numbers", got.PositionScale)
			}
		})
	}
}

// Each of these leaves the numbers alone: the answer is the shared work, not a copy.
func TestOnRecordingPassesThrough(t *testing.T) {
	scaled := scaledWork(5, 1, 5)
	noScale := scaledWork(5, 1, 5)
	noScale.PositionScale = nil
	for _, tc := range []struct {
		name     string
		work     *MetaWork
		chapters []metadata.Chapter
	}{
		{"equal count", scaled, evenChapters(5)},
		{"no chapters", scaled, nil},
		// One chapter says nothing about a place: on it every position would land on
		// the only chapter there is, and the whole cast would show from the start.
		{"a lone chapter", scaled, evenChapters(1)},
		{"no scale", noScale, evenChapters(43)},
		{"no work", nil, evenChapters(43)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OnRecording(tc.work, tc.chapters, 4300); got != tc.work {
				t.Fatalf("got a copy: %+v", got)
			}
		})
	}
}

func TestOnRecordingKeepsOtherFields(t *testing.T) {
	w := scaledWork(5, 0, 1)
	w.Characters[1].Name, w.Characters[1].Aliases, w.Characters[1].Role = "Hamlet", []string{"The Dane"}, "protagonist"
	w.Recaps[0].Scope = "series"
	w.RecapSummary = &MetaRecapSummary{InShort: "short"}
	got := OnRecording(w, evenChapters(43), 4300)
	if c := got.Characters[1]; c.Name != "Hamlet" || c.Role != "protagonist" || len(c.Aliases) != 1 {
		t.Fatalf("character = %+v", c)
	}
	if got.Recaps[0].Scope != "series" || got.Recaps[0].Through.Chapter != 0 || got.RecapSummary != w.RecapSummary || got.Attribution != w.Attribution {
		t.Fatalf("work = %+v", got)
	}
}

// Partial says the sidecar stops short of the length on purpose: the length is still
// the book's, so a position lands where it would on a whole one.
func TestOnRecordingPartial(t *testing.T) {
	w := scaledWork(5, 1, 2)
	w.PositionScale.Partial = true
	if got := reveals(OnRecording(w, evenChapters(43), 4300)); !slices.Equal(got, []int{10, 19}) {
		t.Fatalf("reveals = %v", got)
	}
}

// The promise itself, over every position of several shapes of book: a translated
// position's chapter starts at or past the position's fraction (a reveal is never
// early) unless it is the last chapter, the chapter before it starts before the
// fraction (it is the FIRST such), and positions never go down.
func TestOnRecordingNeverEarly(t *testing.T) {
	books := [][]metadata.Chapter{
		evenChapters(43),
		evenChapters(6),
		chaptersAt(4000, 0, 3000, 3200, 3400, 3600, 3800),
		chaptersAt(9000, 0, 10, 20, 8000, 8500),
		chaptersAt(1000, 30, 40, 900),
	}
	for _, chapters := range books {
		total := chapters[len(chapters)-1].End
		for _, length := range []int{2, 5, 7, 44, 100} {
			if length == len(chapters) {
				continue
			}
			var positions []int
			for p := 1; p <= length; p++ {
				positions = append(positions, p)
			}
			got := reveals(OnRecording(scaledWork(length, positions...), chapters, total))
			if !slices.IsSorted(got) {
				t.Errorf("%d chapters on %d: not ascending: %v", len(chapters), length, got)
			}
			for i, r := range got {
				fraction := float64(i+1) * total / float64(length)
				if r < len(chapters) && chapters[r-1].BookOffset < fraction {
					t.Errorf("%d chapters on %d: position %d lands on chapter %d, starting at %v before %v",
						len(chapters), length, i+1, r, chapters[r-1].BookOffset, fraction)
				}
				if r > 1 && chapters[r-2].BookOffset >= fraction {
					t.Errorf("%d chapters on %d: position %d lands on chapter %d, past chapter %d at %v",
						len(chapters), length, i+1, r, r-1, chapters[r-2].BookOffset)
				}
			}
		}
	}
}

// hamlet is a 43-track Hamlet whose sidecar is on 5 acts, a recap through each act
// and a character revealed in each.
func hamlet() (*MetaWork, []metadata.Chapter) {
	w := scaledWork(5)
	for act, name := range []string{"Act 1", "Act 2", "Act 3", "Act 4", "Act 5"} {
		w.Characters = append(w.Characters, MetaCharacter{ID: name, Reveal: MetaPosition{Chapter: act + 1}})
		w.Recaps = append(w.Recaps, MetaRecap{Through: MetaPosition{Chapter: act + 1}, Text: name})
	}
	return w, evenChapters(43)
}

// The defect: at track 6 the numbers compared raw had Act 5's recap (through 5) behind
// the listener. Translated, nothing past where the listener is shows.
func TestHamletActFiveHiddenAtTrackSix(t *testing.T) {
	w, chapters := hamlet()

	// Denied, the way it was: the raw numbers leak four acts' recaps.
	raw := HideSpoilers(&Enrichment{Work: w}, 6, false).Work
	if got := recapTexts(raw); !slices.Contains(got, "Act 5") {
		t.Fatalf("the raw numbers no longer show the defect: %v", got)
	}

	translated := OnRecording(w, chapters, 4300)
	at := func(track int) *MetaWork { return HideSpoilers(&Enrichment{Work: translated}, track, false).Work }
	if got := recapTexts(at(6)); len(got) != 0 {
		t.Fatalf("recaps at track 6 = %v, want none", got)
	}
	if got := characterIDs(at(6)); len(got) != 0 {
		t.Fatalf("characters at track 6 = %v, want none", got)
	}
	// Allowed: Act 1 ended at 860 s, inside track 9. Its cast shows from track 10, the
	// first starting past it, and its recap once that track is behind the listener.
	if got := characterIDs(at(10)); !slices.Equal(got, []string{"Act 1"}) {
		t.Fatalf("characters at track 10 = %v", got)
	}
	if got := recapTexts(at(11)); !slices.Equal(got, []string{"Act 1"}) {
		t.Fatalf("recaps at track 11 = %v", got)
	}
	// In the last track: Act 5's cast has come, its recap only with a finished book.
	last := at(43)
	if got := characterIDs(last); len(got) != 5 {
		t.Fatalf("characters at track 43 = %v", got)
	}
	if got := recapTexts(last); slices.Contains(got, "Act 5") || len(got) != 4 {
		t.Fatalf("recaps at track 43 = %v", got)
	}
	whole := HideSpoilers(&Enrichment{Work: translated}, 6, true).Work
	if got := recapTexts(whole); len(got) != 5 {
		t.Fatalf("finished recaps = %v", got)
	}
}

// spoilers=hide and a client gating the numbers it receives must agree at every place
// in the book: what a shipped app shows is what the server would have left.
func TestHideSpoilersAgreesWithAClientGate(t *testing.T) {
	w, chapters := hamlet()
	translated := OnRecording(w, chapters, 4300)
	for track := range len(chapters) + 1 {
		var wantCast, wantRecaps []string
		for _, c := range translated.Characters {
			if c.Reveal.Chapter <= max(track, 1) { // meta-gating.ts characterIsVisible
				wantCast = append(wantCast, c.ID)
			}
		}
		for _, r := range translated.Recaps {
			if r.Through.Chapter == 0 || r.Through.Chapter < track { // recapIsVisible
				wantRecaps = append(wantRecaps, r.Text)
			}
		}
		got := HideSpoilers(&Enrichment{Work: translated}, track, false).Work
		if ids := characterIDs(got); !slices.Equal(ids, wantCast) {
			t.Errorf("track %d: characters = %v, client = %v", track, ids, wantCast)
		}
		if texts := recapTexts(got); !slices.Equal(texts, wantRecaps) {
			t.Errorf("track %d: recaps = %v, client = %v", track, texts, wantRecaps)
		}
	}
}

// The work Enrich returns is shared with the cache and every other caller.
func TestOnRecordingNeverMutates(t *testing.T) {
	w := scaledWork(5, 1, 3, 5)
	before, _ := json.Marshal(w)
	scale := w.PositionScale
	out := OnRecording(w, evenChapters(43), 4300)
	after, _ := json.Marshal(w)
	if string(before) != string(after) || w.PositionScale != scale {
		t.Fatalf("input changed:\nbefore %s\nafter  %s", before, after)
	}
	if out == w || &out.Characters[0] == &w.Characters[0] || &out.Recaps[0] == &w.Recaps[0] {
		t.Fatal("the translated parts must be copies")
	}
	// Two books sharing the work each get their own answer.
	if a, b := reveals(OnRecording(w, evenChapters(43), 4300)), reveals(OnRecording(w, evenChapters(10), 1000)); slices.Equal(a, b) {
		t.Fatalf("two recordings answered alike: %v", a)
	}
}

const scaledWorkDoc = `{"id":"hamlet","title":"Hamlet","authors":[],"language":"en",` +
	`"characters":[{"id":"hamlet","name":"Hamlet","reveal":{"chapter":1}}],` +
	`"position_scale":{"length":5,"unit":"act","partial":true}}`

// The scale is decoded off a work, on Work's answer (and the envelope's, by the same
// toWork), and survives the persistent cache.
func TestPositionScaleDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(scaledWorkDoc))
	}))
	store := newMemStore()
	s := NewService(srv.URL, nil)
	s.SetStore(store)
	ctx := context.Background()

	w, err := s.Work(ctx, "hamlet")
	if err != nil {
		t.Fatal(err)
	}
	if want := (MetaPositionScale{Length: 5, Unit: "act", Partial: true}); w.PositionScale == nil || *w.PositionScale != want {
		t.Fatalf("position_scale = %+v", w.PositionScale)
	}
	body, _ := json.Marshal(w)
	if !jsonHas(t, body, "position_scale") {
		t.Fatalf("not on the wire: %s", body)
	}

	// A restart reads it back from the store, with upstream gone.
	srv.Close()
	again := NewService(srv.URL, nil)
	again.SetStore(store)
	w, err = again.Work(ctx, "hamlet")
	if err != nil || w.PositionScale == nil || w.PositionScale.Length != 5 {
		t.Fatalf("from the store = %+v, %v", w, err)
	}
}

// A work that states none, or a length nothing can be placed on, carries none.
func TestPositionScaleOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/works/plain" {
			_, _ = w.Write([]byte(`{"id":"plain","title":"Plain","authors":[],"language":"en"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"nothing","title":"Nothing","authors":[],"language":"en","position_scale":{"length":0,"unit":"act"}}`))
	}))
	defer srv.Close()
	s := NewService(srv.URL, nil)
	for _, id := range []string{"plain", "nothing"} {
		w, err := s.Work(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(w)
		if w.PositionScale != nil || jsonHas(t, body, "position_scale") {
			t.Fatalf("%s: %s", id, body)
		}
	}
}

func jsonHas(t *testing.T, body []byte, key string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	_, ok := m[key]
	return ok
}

// Mirror mode needs nothing of its own: the copy's answer decodes as the remote
// service's does, so the scale arrives the moment the pinned pkg/query's handler sends
// it. This stands in for that handler; the pinned one sends none, which is a work with
// no scale, passed through as before.
func TestMirrorCarriesPositionScale(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the remote service was asked")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer remote.Close()
	s := NewService(remote.URL, nil)
	s.SetMirror(staticMirror{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/works/hamlet" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(scaledWorkDoc))
	})}, nil)
	w, err := s.Work(context.Background(), "hamlet")
	if err != nil {
		t.Fatal(err)
	}
	if w.PositionScale == nil || w.PositionScale.Length != 5 || w.PositionScale.Unit != "act" {
		t.Fatalf("position_scale = %+v", w.PositionScale)
	}
}

// staticMirror is a ready copy answering with h.
type staticMirror struct{ h http.Handler }

func (m staticMirror) Handler() http.Handler { return m.h }
func (staticMirror) Ready() bool             { return true }

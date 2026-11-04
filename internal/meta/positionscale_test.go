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
		name         string
		length       int
		positions    []int
		chapters     []metadata.Chapter
		duration     float64
		wantReveals  []int
		wantThroughs []int
	}{
		// 43 tracks of 100 s on 5 acts, each act 860 s. A reveal is the first track
		// starting at or past its act's start, a through the track holding its act's end.
		{"finer", 5, []int{1, 2, 3, 4, 5}, evenChapters(43), 4300, []int{1, 10, 19, 27, 36}, []int{9, 18, 26, 35, 43}},
		{"p=0 stays before this book", 5, []int{0}, evenChapters(43), 4300, []int{0}, []int{0}},
		{"p past the length is the last chapter", 5, []int{6, 99}, evenChapters(43), 4300, []int{43, 43}, []int{43, 43}},
		// A boundary exactly on the fraction: the second act starts on the third chapter,
		// and the first act's recap is chapter 2, opening as the third starts.
		{"a chapter boundary on the fraction", 2, []int{1, 2}, evenChapters(4), 400, []int{1, 3}, []int{2, 4}},
		// A long opening track, three quarters of the book. By time every act but the last
		// ends inside it; the count (reveals 1, 3, 4, 6, throughs 2, 3, 5, 6) is later, and
		// later wins.
		{"a long first track", 4, []int{1, 2, 3, 4}, chaptersAt(4000, 0, 3000, 3200, 3400, 3600, 3800), 4000, []int{1, 3, 4, 6}, []int{2, 3, 5, 6}},
		// A long last track, over two thirds of the book. By count these would read 1, 3, 4, 6
		// and 2, 3, 5, 6, but by time Act 2 starts on the fifth track's start and Act 1 ends
		// on the fourth's end, and all the rest lie inside the last. Time is later here.
		{"a long last track", 4, []int{1, 2, 3, 4}, chaptersAt(8000, 0, 500, 1000, 1500, 2000, 2500), 8000, []int{1, 5, 6, 6}, []int{4, 6, 6, 6}},
		// A duration read short of the last chapter's end: the later of the two is the
		// length, so the fractions aren't brought forward.
		{"a short duration", 4, []int{1, 2, 3, 4}, chaptersAt(8000, 0, 500, 1000, 1500, 2000, 2500), 3000, []int{1, 5, 6, 6}, []int{4, 6, 6, 6}},
		// Duration unknown: the last chapter's end stands in.
		{"no duration", 5, []int{1, 5}, evenChapters(43), 0, []int{1, 36}, []int{9, 43}},
		// Times that can't be trusted: every chapter at 0, so the count alone.
		{"all offsets zero go by count", 5, []int{1, 2, 3, 4, 5}, make([]metadata.Chapter, 43), 4300, []int{1, 10, 19, 27, 36}, []int{9, 18, 26, 35, 43}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := OnRecording(scaledWork(tc.length, tc.positions...), tc.chapters, tc.duration)
			if r := reveals(got); !slices.Equal(r, tc.wantReveals) {
				t.Errorf("reveals = %v, want %v", r, tc.wantReveals)
			}
			if r := throughs(got); !slices.Equal(r, tc.wantThroughs) {
				t.Errorf("throughs = %v, want %v", r, tc.wantThroughs)
			}
			if got.PositionScale != nil {
				t.Errorf("position_scale = %+v beside translated numbers", got.PositionScale)
			}
		})
	}
}

// 41 tracks on 40 chapters, the usual credits track or a split chapter. Whatever the
// tracks' lengths, the count keeps a position from reading early: with an opening track
// a fifth of the book, time alone would put chapter 9's end (and so its recap's opening)
// on the second track.
func TestOnRecordingNearEqualCounts(t *testing.T) {
	positions := []int{1, 10, 20, 39}
	wantReveals, wantThroughs := []int{1, 11, 21, 40}, []int{2, 11, 21, 40}

	even := OnRecording(scaledWork(40, positions...), evenChapters(41), 4100)
	if got := reveals(even); !slices.Equal(got, wantReveals) {
		t.Errorf("even reveals = %v, want %v", got, wantReveals)
	}
	if got := throughs(even); !slices.Equal(got, wantThroughs) {
		t.Errorf("even throughs = %v, want %v", got, wantThroughs)
	}

	// The opening track holds a fifth of the book, the other 40 share the rest evenly.
	starts := []float64{0}
	for i := range 40 {
		starts = append(starts, 1000+float64(i)*100)
	}
	uneven := OnRecording(scaledWork(40, positions...), chaptersAt(5000, starts...), 5000)
	for i, p := range positions {
		if r, c := uneven.Characters[i].Reveal.Chapter, even.Characters[i].Reveal.Chapter; r < c {
			t.Errorf("position %d: reveal %d reads before the count's %d", p, r, c)
		}
		if r, c := uneven.Recaps[i].Through.Chapter, even.Recaps[i].Through.Chapter; r < c {
			t.Errorf("position %d: through %d reads before the count's %d", p, r, c)
		}
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
		// Coarser than the scale: positions past the last chapter wait for the finish,
		// which is late and so safe; translating could only bring them forward.
		{"coarser", scaled, evenChapters(3)},
		{"one short", scaled, evenChapters(4)},
		{"a lone chapter", scaled, evenChapters(1)},
		{"no chapters", scaled, nil},
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
	got := OnRecording(w, evenChapters(43), 4300)
	if r, th := reveals(got), throughs(got); !slices.Equal(r, []int{1, 10}) || !slices.Equal(th, []int{9, 18}) {
		t.Fatalf("reveals = %v, throughs = %v", r, th)
	}
}

// The promise itself, over every position of several shapes of book. A reveal's chapter
// starts at or past its unit's start, and the chapter a recap opens in (the one after
// its through) starts at or past its unit's end, unless the recap only opens with a
// finished book. Neither is ever before the count, and neither goes down with p.
func TestOnRecordingNeverEarly(t *testing.T) {
	books := [][]metadata.Chapter{
		evenChapters(43),
		evenChapters(41),
		evenChapters(6),
		chaptersAt(4000, 0, 3000, 3200, 3400, 3600, 3800),
		chaptersAt(9000, 0, 10, 20, 8000, 8500),
		chaptersAt(1000, 30, 40, 900),
	}
	for _, chapters := range books {
		n, total := len(chapters), chapters[len(chapters)-1].End
		for _, length := range []int{2, 4, 5, 40} {
			if length >= n {
				continue
			}
			var positions []int
			for p := 1; p <= length; p++ {
				positions = append(positions, p)
			}
			got := OnRecording(scaledWork(length, positions...), chapters, total)
			revealed, through := reveals(got), throughs(got)
			if !slices.IsSorted(revealed) || !slices.IsSorted(through) {
				t.Errorf("%d chapters on %d: not ascending: %v %v", n, length, revealed, through)
			}
			for i, r := range revealed {
				unitStart := float64(i) * total / float64(length)
				if r < n && chapters[r-1].BookOffset < unitStart {
					t.Errorf("%d chapters on %d: reveal %d lands on chapter %d, starting at %v before %v",
						n, length, i+1, r, chapters[r-1].BookOffset, unitStart)
				}
				if r < ceilDiv(i*n, length)+1 {
					t.Errorf("%d chapters on %d: reveal %d lands on %d, before the count", n, length, i+1, r)
				}
			}
			for i, r := range through {
				unitEnd := float64(i+1) * total / float64(length)
				if r < n && chapters[r].BookOffset < unitEnd {
					t.Errorf("%d chapters on %d: recap %d opens in chapter %d, starting at %v before %v",
						n, length, i+1, r+1, chapters[r].BookOffset, unitEnd)
				}
				if r < ceilDiv((i+1)*n, length) {
					t.Errorf("%d chapters on %d: through %d lands on %d, before the count", n, length, i+1, r)
				}
			}
		}
	}
}

// hamlet is a 43-track Hamlet whose sidecar is on 5 acts, a recap through each act
// and a character revealed in each. Each act runs 860 s of 4300, each track 100 s.
func hamlet() (*MetaWork, []metadata.Chapter) {
	w := scaledWork(5)
	for act, name := range []string{"Act 1", "Act 2", "Act 3", "Act 4", "Act 5"} {
		w.Characters = append(w.Characters, MetaCharacter{ID: name, Reveal: MetaPosition{Chapter: act + 1}})
		w.Recaps = append(w.Recaps, MetaRecap{Through: MetaPosition{Chapter: act + 1}, Text: name})
	}
	return w, evenChapters(43)
}

// hamletAt is the translated Hamlet gated for a listener in that track.
func hamletAt(track int, finished bool) *MetaWork {
	w, chapters := hamlet()
	return HideSpoilers(&Enrichment{Work: OnRecording(w, chapters, 4300)}, track, finished).Work
}

// The defect: at track 6 the numbers compared raw had Act 5's recap (through 5) behind
// the listener.
func TestHamletActFiveRecapHiddenAtTrackSix(t *testing.T) {
	w, _ := hamlet()
	// Denied, the way it was: the raw numbers leak four acts' recaps.
	if got := recapTexts(HideSpoilers(&Enrichment{Work: w}, 6, false).Work); !slices.Contains(got, "Act 5") {
		t.Fatalf("the raw numbers no longer show the defect: %v", got)
	}
	if got := recapTexts(hamletAt(6, false)); len(got) != 0 {
		t.Fatalf("recaps at track 6 = %v, want none", got)
	}
	if got := characterIDs(hamletAt(6, false)); !slices.Equal(got, []string{"Act 1"}) {
		t.Fatalf("characters at track 6 = %v, want the opening cast", got)
	}
}

// The opening cast shows from the start, "From the start" included (the player's
// revealFromStart reads reveal <= 1), before anything has been played.
func TestHamletActOneCastShowsAtTrackOne(t *testing.T) {
	for _, track := range []int{0, 1} {
		if got := characterIDs(hamletAt(track, false)); !slices.Equal(got, []string{"Act 1"}) {
			t.Fatalf("characters at track %d = %v", track, got)
		}
	}
	w, chapters := hamlet()
	if got := OnRecording(w, chapters, 4300).Characters[0].Reveal.Chapter; got != 1 {
		t.Fatalf("Act 1's reveal = %d, want 1", got)
	}
}

// Act 5 starts at 3440 s, inside track 35; track 36 is the first starting past it.
func TestHamletActFiveCharacterOnlyFromTheTrackStartingActFive(t *testing.T) {
	if got := characterIDs(hamletAt(35, false)); slices.Contains(got, "Act 5") {
		t.Fatalf("characters at track 35 = %v", got)
	}
	if got := characterIDs(hamletAt(36, false)); !slices.Contains(got, "Act 5") {
		t.Fatalf("characters at track 36 = %v", got)
	}
}

// Nothing but a finished book opens the last act's recap, the last track included.
func TestHamletActFiveRecapOnlyWhenFinished(t *testing.T) {
	if got := recapTexts(hamletAt(43, false)); slices.Contains(got, "Act 5") || len(got) != 4 {
		t.Fatalf("recaps at track 43 = %v", got)
	}
	if got := recapTexts(hamletAt(43, true)); !slices.Contains(got, "Act 5") {
		t.Fatalf("finished recaps = %v", got)
	}
}

// Act 1 ends at 860 s, inside track 9 (800 to 900). Track 9 is still Act 1, so the recap
// opens on entering track 10, not a chapter later and not a chapter sooner.
func TestHamletActOneRecapOnceActOneIsOver(t *testing.T) {
	if got := recapTexts(hamletAt(9, false)); slices.Contains(got, "Act 1") {
		t.Fatalf("recaps at track 9, with Act 1 still playing = %v", got)
	}
	if got := recapTexts(hamletAt(10, false)); !slices.Equal(got, []string{"Act 1"}) {
		t.Fatalf("recaps at track 10 = %v", got)
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

// Unknown offsets must not read as "at the end", which showed everything: a saved place
// is read by count, rounded down, or as not started when there is no duration to read
// it against.
func TestListeningChapter(t *testing.T) {
	zero := make([]metadata.Chapter, 10)
	for _, tc := range []struct {
		name     string
		chapters []metadata.Chapter
		duration float64
		position float64
		want     int
	}{
		{"usable starts", evenChapters(10), 1000, 550, 6},
		{"zero offsets by count, rounded down", zero, 1000, 550, 5},
		{"zero offsets inside the first share", zero, 1000, 50, 0},
		{"zero offsets, late in the book, never past it", zero, 1000, 5000, 10},
		{"zero offsets and no duration: not started", zero, 0, 550, 0},
		{"zero offsets and no place", zero, 1000, 0, 0},
		{"no chapters", nil, 1000, 550, 0},
		{"a lone chapter holds every place", evenChapters(1), 100, 50, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ListeningChapter(tc.chapters, tc.duration, tc.position); got != tc.want {
				t.Fatalf("ListeningChapter = %d, want %d", got, tc.want)
			}
		})
	}
}

// ChapterAt alone has no duration to fall back on: starts all alike are not started.
// Out of order is not that: the walk stops at the first start past the place.
func TestChapterAtStartsAllAlike(t *testing.T) {
	for _, tc := range []struct {
		starts []float64
		want   int
	}{
		{[]float64{0, 0, 0}, 0},
		{[]float64{100, 100}, 0},
		{[]float64{0, 200, 100}, 1},
		// A zero-length marker among good starts is still a timeline.
		{[]float64{0, 100, 100, 200}, 3},
		{[]float64{0}, 1},
		// Past the last start, files of unknown length after the last known one are not
		// the end either: the place is in the run's first.
		{[]float64{0, 100, 100, 100}, 2},
		{[]float64{0, 50, 100, 100}, 3},
		{[]float64{0, 50, 100}, 3},
	} {
		if got := ChapterAt(tc.starts, 150); got != tc.want {
			t.Errorf("ChapterAt(%v, 150) = %d, want %d", tc.starts, got, tc.want)
		}
	}
}

// A gate that leaves none of the positions leaves no scale either, which is the
// community's like the credit that goes with it; one that leaves any keeps it.
func TestGateWorkDropsTheScaleWithTheLastPosition(t *testing.T) {
	gated := GateWork(scaledWork(5, 5), 1, false)
	if gated.PositionScale != nil || gated.Attribution != nil {
		t.Fatalf("nothing left, scale = %+v, attribution = %+v", gated.PositionScale, gated.Attribution)
	}
	gated = GateWork(scaledWork(5, 1, 5), 1, false)
	if gated.PositionScale == nil || gated.Attribution == nil {
		t.Fatalf("a position left, scale = %+v, attribution = %+v", gated.PositionScale, gated.Attribution)
	}
	if w := scaledWork(5, 5); GateWork(w, 1, true) != w {
		t.Fatal("a finished book is kept whole")
	}
}

// Times out of order can't be searched, so a translation goes by count.
func TestOnRecordingOutOfOrderOffsetsGoByCount(t *testing.T) {
	chapters := evenChapters(43)
	chapters[20].BookOffset = 0
	got := OnRecording(scaledWork(5, 1, 2, 5), chapters, 4300)
	if r, th := reveals(got), throughs(got); !slices.Equal(r, []int{1, 10, 36}) || !slices.Equal(th, []int{9, 18, 43}) {
		t.Fatalf("reveals = %v, throughs = %v", r, th)
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
	if a, b := throughs(OnRecording(w, evenChapters(43), 4300)), throughs(OnRecording(w, evenChapters(10), 1000)); slices.Equal(a, b) {
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

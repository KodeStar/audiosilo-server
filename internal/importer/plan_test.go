package importer

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/importer/abstest"
)

// TestBuildPayloadFromRecordedABS normalizes alex's recorded history.
func TestBuildPayloadFromRecordedABS(t *testing.T) {
	srv := abstest.New(t)
	c := clientFor(t, srv.URL, abstest.AdminToken)
	ctx := context.Background()
	u, own, err := c.userDetail(ctx, abstest.AlexID)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := c.sessions(ctx, abstest.AlexID, own)
	if err != nil {
		t.Fatal(err)
	}
	items, err := c.items(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := buildPayload(u, sessions, newItemIndex(items))
	if p.V != payloadVersion || len(p.Items) != 10 || len(p.Sessions) != 37 || len(p.Progress) != 9 || len(p.Bookmarks) != 3 {
		t.Fatalf("payload: %d items, %d sessions, %d progress, %d bookmarks", len(p.Items), len(p.Sessions),
			len(p.Progress), len(p.Bookmarks))
	}
	byID := map[string]Item{}
	for _, it := range p.Items {
		byID[it.ID[:8]] = it
	}
	if it := byID["867ec553"]; !it.Gone || it.Title != "Removed Later" || it.RelPath != "" {
		t.Errorf("deleted item = %+v", it)
	}
	if it := byID["9ab5a9b4"]; !it.IsFile || it.Siblings != 1 || it.RelPath != "Loose Single File.m4b" {
		t.Errorf("single file item = %+v", it)
	}
	if it := byID["851b8e6d"]; it.Series != "The Expanse" || it.Sequence != "1" || it.ASIN != "B00P9XDQFY" ||
		it.Author != "James S. A. Corey" || it.RelPath != "James S. A. Corey/The Expanse/01 - Leviathan Wakes" {
		t.Errorf("Leviathan Wakes = %+v", it)
	}
	var nullListened, web int
	for _, s := range p.Sessions {
		if s.Listened == 0 {
			nullListened++
		}
		if s.Platform == "web" && s.Device == "Mac OS 14.6 Firefox" {
			web++
		}
	}
	if nullListened != 1 || web == 0 {
		t.Errorf("null listening sessions %d, web sessions %d", nullListened, web)
	}

	enc, err := encodePayload(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodePayload(enc)
	if err != nil || !reflect.DeepEqual(back, p) {
		t.Fatalf("payload round trip: %v", err)
	}
	// A stored payload of another version (a shape this build doesn't know) is
	// refused rather than misread.
	if enc, err = encodePayload(&Payload{V: payloadVersion + 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := decodePayload(enc); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("a payload of another version: %v", err)
	}
}

// TestBuildPayloadGoneItem: an item ABS no longer has is one gone item however
// many sessions name it, and any progress or bookmark left on it is dropped (so
// matchAll can merge gone items: they never carry progress).
func TestBuildPayloadGoneItem(t *testing.T) {
	sess := func() absSession {
		s := absSession{LibraryItemID: "gone", DisplayTitle: "Removed", DisplayAuthor: "Someone", Duration: 600}
		s.StartedAt, s.UpdatedAt, s.TimeListening = 1, 2, 60
		return s
	}
	u := &absUserDetail{MediaProgress: []absProgress{{LibraryItemID: "gone", CurrentTime: 300}},
		Bookmarks: []absBookmark{{LibraryItemID: "gone", Time: 10, Title: "x"}}}
	p := buildPayload(u, []absSession{sess(), sess()}, newItemIndex(nil))
	if len(p.Items) != 1 || !p.Items[0].Gone || p.Items[0].Title != "Removed" || len(p.Sessions) != 2 ||
		len(p.Progress) != 0 || len(p.Bookmarks) != 0 {
		t.Errorf("payload = %+v", p)
	}
}

func TestDescribeMetadata(t *testing.T) {
	for _, tc := range []struct {
		m                          absMetadata
		title, author, series, seq string
	}{
		{absMetadata{Title: "Gods of Risk", AuthorName: "James S. A. Corey", SeriesName: "The Expanse #2.5"},
			"Gods of Risk", "James S. A. Corey", "The Expanse", "2.5"},
		{absMetadata{Title: "T", AuthorName: "A, B", SeriesName: "S #1, Other #4"}, "T", "A, B", "S", "1"},
		{absMetadata{Title: "T", SeriesName: "No Number"}, "T", "", "No Number", ""},
		{absMetadata{Title: "T", Authors: []struct {
			Name string `json:"name"`
		}{{"X"}, {"Y"}}, Series: []struct {
			Name     string `json:"name"`
			Sequence string `json:"sequence"`
		}{{"Arr", "7"}}}, "T", "X, Y", "Arr", "7"},
	} {
		title, author, series, seq := tc.m.describe()
		if title != tc.title || author != tc.author || series != tc.series || seq != tc.seq {
			t.Errorf("describe(%+v) = %q %q %q %q", tc.m, title, author, series, seq)
		}
	}
}

var (
	refA = catalog.Ref{LibraryID: 1, Path: "A"}
	refB = catalog.Ref{LibraryID: 1, Path: "B"}
	t0   = time.Date(2026, 3, 10, 20, 0, 0, 0, time.UTC)
)

func ms(t time.Time) int64 { return t.UnixMilli() }

func planState(cutoff time.Time) catalog.ImportState {
	return catalog.ImportState{Import: catalog.Import{Cutoff: catalog.FormatCutoff(cutoff)},
		Progress: map[catalog.Ref]catalog.Progress{}, Bookmarks: map[catalog.Ref][]catalog.Bookmark{}}
}

// TestPlanSessions: sessions on matched books before the cutoff, oldest first,
// with a long-open session's end clamped; the unmatched book is reported.
func TestPlanSessions(t *testing.T) {
	p := &Payload{
		Items: []Item{{ID: "a", Title: "A"}, {ID: "x", Title: "Lost", Author: "Who"}},
		Sessions: []Session{
			{ItemID: "a", Started: ms(t0.Add(48 * time.Hour)), Updated: ms(t0.Add(49 * time.Hour)), Listened: 1800},
			// Open overnight: 10 minutes of listening over 9 hours.
			{ItemID: "a", Started: ms(t0), Updated: ms(t0.Add(9 * time.Hour)), Listened: 600, StartPos: 0, EndPos: 600},
			{ItemID: "a", Started: ms(t0.Add(24 * time.Hour)), Updated: ms(t0.Add(24*time.Hour + 20*time.Minute)), Listened: 1000},
			{ItemID: "a", Started: ms(t0.Add(72 * time.Hour)), Listened: 300}, // after the cutoff
			{ItemID: "a", Started: ms(t0.Add(time.Hour)), Listened: 0},        // nothing listened
			{ItemID: "x", Started: ms(t0), Updated: ms(t0.Add(time.Hour)), Listened: 3000},
		},
	}
	matches := map[string]Match{"a": {Ref: refA, Tier: TierPath}, "x": {Reason: catalog.ReasonNoMatch}}
	w := plan(p, matches, planState(t0.Add(60*time.Hour)), time.UTC, t0)
	if len(w.Sessions) != 3 {
		t.Fatalf("sessions = %+v", w.Sessions)
	}
	first := w.Sessions[0]
	if !first.StartedAt.Equal(t0) || !first.LastAt.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("clamped session = %v..%v", first.StartedAt, first.LastAt)
	}
	if !w.Sessions[1].LastAt.Equal(t0.Add(24*time.Hour + 20*time.Minute)) {
		t.Errorf("a normal session's end moved: %v", w.Sessions[1].LastAt)
	}
	s := w.Plan.Summary
	if s.Sessions != 3 || s.SkippedAfterCutoff != 1 || s.Listened != 3400 || s.Matched.Path != 1 || s.Unmatched != 1 ||
		s.Items != 2 || *s.FirstListen != "2026-03-10T20:00:00Z" || *s.LastListen != "2026-03-12T20:00:00Z" {
		t.Errorf("summary = %+v", s)
	}
	if u := w.Plan.Unmatched; len(u) != 1 || u[0].Title != "Lost" || u[0].Listened != 3000 || u[0].Sessions != 1 {
		t.Errorf("unmatched = %+v", u)
	}

	// No cutoff: every session.
	if w := plan(p, matches, planState(time.Time{}), time.UTC, t0); w.Plan.Summary.Sessions != 4 {
		t.Errorf("no cutoff: %d sessions", w.Plan.Summary.Sessions)
	}
}

func TestMergeProgress(t *testing.T) {
	stamp := catalog.FormatStamp
	old := t0.Add(-30 * 24 * time.Hour)
	abs := Progress{Position: 3000, Duration: 8000, Started: ms(old), Updated: ms(t0)}
	done := Progress{Position: 8000, Duration: 8000, Done: true, Started: ms(old), Updated: ms(t0), FinishedAt: ms(t0)}
	row := func(pos float64, finished bool, updated time.Time, started string) *catalog.Progress {
		p := &catalog.Progress{Ref: refA, Position: pos, Duration: 8000, Finished: finished, PlaybackSpeed: 1.5,
			Version: 4, DeviceID: "phone", UpdatedAt: stamp(updated), StartedAt: started}
		if finished {
			p.FinishedAt = "2026-01-01T00:00:00Z"
		}
		return p
	}
	m := Match{Ref: refA, Tier: TierPath, Duration: 8100}
	for _, tc := range []struct {
		name   string
		have   *catalog.Progress
		abs    Progress
		want   mergeResult
		check  func(catalog.Progress) bool
		detail string
	}{
		{"new row from ABS", nil, abs, mergeResult{true, true, false}, func(p catalog.Progress) bool {
			return p.Position == 3000 && p.Duration == 8100 && p.Version == 1 && p.UpdatedAt == stamp(t0) &&
				p.StartedAt == old.Format(time.RFC3339) && p.FinishedAt == ""
		}, "position, book length, ABS's times"},
		{"new finished row", nil, done, mergeResult{true, true, true}, func(p catalog.Progress) bool {
			return p.Finished && p.FinishedAt == t0.Format(time.RFC3339)
		}, "finished with ABS's date"},
		{"ABS never played it", nil, Progress{Updated: ms(t0)}, mergeResult{}, nil, ""},
		{"ABS newer and further: advances", row(1000, false, t0.Add(-time.Hour), ""), abs,
			mergeResult{true, true, false}, func(p catalog.Progress) bool {
				return p.Position == 3000 && p.Version == 5 && p.UpdatedAt == stamp(t0) && p.PlaybackSpeed == 1.5 &&
					p.DeviceID == "phone" && p.StartedAt == old.Format(time.RFC3339)
			}, "moved on, speed and device kept"},
		{"ABS newer but behind: never rewinds", row(5000, false, t0.Add(-time.Hour), "2020-01-01T00:00:00Z"), abs,
			mergeResult{}, nil, ""},
		{"listening here is newer: only the start fills", row(100, false, t0.Add(time.Hour), "2026-03-01T00:00:00Z"), abs,
			mergeResult{true, false, false}, func(p catalog.Progress) bool {
				return p.Position == 100 && p.StartedAt == old.Format(time.RFC3339) && p.UpdatedAt == stamp(t0.Add(time.Hour)) &&
					p.Version == 5
			}, "earlier start, updated_at kept"},
		{"ABS finished it after: finished here with ABS's date", row(1000, false, t0.Add(-time.Hour), ""), done,
			mergeResult{true, true, true}, func(p catalog.Progress) bool {
				return p.Finished && p.Position == 8000 && p.FinishedAt == t0.Format(time.RFC3339)
			}, "finished"},
		{"finished here stays finished", row(8000, true, t0.Add(-time.Hour), "2020-01-01T00:00:00Z"),
			Progress{Position: 10, Updated: ms(t0), Started: ms(old)}, mergeResult{}, nil, ""},
		{"ABS's clock ahead: stamped now, not in the future", nil,
			Progress{Position: 3000, Started: ms(old), Updated: ms(t0.Add(48 * time.Hour))},
			mergeResult{true, true, false}, func(p catalog.Progress) bool {
				return p.UpdatedAt == stamp(t0)
			}, "updated_at clamped to now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, r := mergeProgress(tc.have, tc.abs, m, t0)
			if r != tc.want {
				t.Fatalf("result = %+v, want %+v (row %+v)", r, tc.want, got)
			}
			if tc.check != nil && !tc.check(got) {
				t.Errorf("row = %+v; want %s", got, tc.detail)
			}
		})
	}
}

// TestPlanEstimate: progress further than the sessions explain adds one
// estimated day (5 minutes or more, dated by ABS's start in the server's zone,
// before the cutoff only), at the speed the sessions show.
func TestPlanEstimate(t *testing.T) {
	loc := time.FixedZone("east", 5*3600)
	started := time.Date(2026, 3, 9, 22, 0, 0, 0, time.UTC) // 10 March in loc
	p := &Payload{
		Items: []Item{{ID: "a"}, {ID: "b"}},
		Sessions: []Session{
			// 1000 s of book in 500 s listened: speed 2.
			{ItemID: "a", Started: ms(t0), Updated: ms(t0.Add(500 * time.Second)), Listened: 500, StartPos: 0, EndPos: 1000},
			{ItemID: "b", Started: ms(t0), Updated: ms(t0.Add(time.Hour)), Listened: 3500, StartPos: 0, EndPos: 3500},
		},
		Progress: []Progress{
			{ItemID: "a", Position: 5000, Started: ms(started), Updated: ms(t0)},
			{ItemID: "b", Position: 3600, Started: ms(started), Updated: ms(t0)}, // 100 s short of an estimate
		},
	}
	matches := map[string]Match{"a": {Ref: refA, Tier: TierPath}, "b": {Ref: refB, Tier: TierPath}}
	w := plan(p, matches, planState(time.Time{}), loc, t0)
	if len(w.Days) != 1 || w.Days[0].Ref != refA || w.Days[0].Day != "2026-03-10" || w.Days[0].Listened != 2000 {
		t.Fatalf("days = %+v", w.Days)
	}
	if w.Plan.Summary.Estimated != 2000 {
		t.Errorf("estimated = %v", w.Plan.Summary.Estimated)
	}
	// A book started after the cutoff gets no estimate.
	if w := plan(p, matches, planState(started), loc, t0); len(w.Days) != 0 {
		t.Errorf("estimate after the cutoff: %+v", w.Days)
	}
}

// TestPlanEstimateCountsGoneSessions: a gone item's sessions merge onto the book
// its present item matched, so the estimate subtracts them too (they are
// imported as sessions; counting them again as an estimate would double them).
func TestPlanEstimateCountsGoneSessions(t *testing.T) {
	started := t0.Add(-48 * time.Hour)
	p := &Payload{
		Items: []Item{{ID: "present"}, {ID: "gone", Gone: true}},
		Sessions: []Session{
			{ItemID: "gone", Started: ms(started), Updated: ms(started.Add(time.Hour)), Listened: 3600,
				StartPos: 0, EndPos: 3600},
			{ItemID: "present", Started: ms(t0.Add(-time.Hour)), Updated: ms(t0), Listened: 1800,
				StartPos: 3600, EndPos: 5400},
		},
		Progress: []Progress{{ItemID: "present", Position: 6000, Started: ms(started), Updated: ms(t0)}},
	}
	matches := map[string]Match{"present": {Ref: refA, Tier: TierPath}, "gone": {Ref: refA, Tier: TierPath}}
	w := plan(p, matches, planState(time.Time{}), time.UTC, t0)
	if len(w.Days) != 1 || w.Days[0].Listened != 6000-3600-1800 {
		t.Fatalf("days = %+v, want one estimate of %d s", w.Days, 6000-3600-1800)
	}
}

// TestPlanEstimateFinishedUnheard: a book marked finished in ABS without one
// session (ABS sets its position to the end) gets no estimate; a finished book
// with sessions, those after the cutoff included, keeps the estimate of the rest,
// and so does an unfinished one with none (0021's gap).
func TestPlanEstimateFinishedUnheard(t *testing.T) {
	started := t0.Add(-48 * time.Hour)
	refC := catalog.Ref{LibraryID: 1, Path: "C"}
	p := &Payload{
		Items: []Item{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		Sessions: []Session{
			// b's only session is after the cutoff: recorded, not imported.
			{ItemID: "b", Started: ms(t0.Add(time.Hour)), Updated: ms(t0.Add(2 * time.Hour)), Listened: 3600,
				StartPos: 0, EndPos: 3600},
			// a has one that recorded nothing: still no listening.
			{ItemID: "a", Started: ms(t0.Add(-time.Hour)), Listened: 0},
		},
		Progress: []Progress{
			{ItemID: "a", Position: 36000, Duration: 36000, Done: true, Started: ms(started), Updated: ms(started),
				FinishedAt: ms(started)},
			{ItemID: "b", Position: 36000, Duration: 36000, Done: true, Started: ms(started), Updated: ms(t0)},
			{ItemID: "c", Position: 1800, Duration: 36000, Started: ms(started), Updated: ms(started)},
		},
	}
	matches := map[string]Match{"a": {Ref: refA, Tier: TierPath}, "b": {Ref: refB, Tier: TierPath},
		"c": {Ref: refC, Tier: TierPath}}
	w := plan(p, matches, planState(t0), time.UTC, t0)
	got := map[catalog.Ref]float64{}
	for _, d := range w.Days {
		got[d.Ref] = d.Listened
	}
	if len(got) != 2 || got[refB] != 36000-3600 || got[refC] != 1800 {
		t.Errorf("estimates = %+v", w.Days)
	}
	if s := w.Plan.Summary; s.Finished != 2 || s.Estimated != 36000-3600+1800 {
		t.Errorf("summary = %+v", s)
	}
}

// TestPlanBookmarks: a bookmark the person already has (within 2 s, same text)
// isn't added again, and two ABS ones that are the same land once.
func TestPlanBookmarks(t *testing.T) {
	p := &Payload{
		Items: []Item{{ID: "a"}},
		Bookmarks: []Bookmark{
			{ItemID: "a", Position: 101, Title: "Kept", Created: ms(t0)},
			{ItemID: "a", Position: 500, Title: "New", Created: ms(t0)},
			{ItemID: "a", Position: 501.5, Title: "New", Created: ms(t0)},
			{ItemID: "a", Position: 900, Title: "Kept", Created: ms(t0)},
			{ItemID: "z", Position: 1, Title: "Unmatched"},
		},
	}
	st := planState(time.Time{})
	st.Bookmarks[refA] = []catalog.Bookmark{{Ref: refA, Position: 100, Note: "Kept"}}
	w := plan(p, map[string]Match{"a": {Ref: refA, Tier: TierPath}}, st, time.UTC, t0)
	if len(w.Bookmarks) != 2 || w.Bookmarks[0].Position != 500 || w.Bookmarks[1].Position != 900 ||
		w.Bookmarks[0].CreatedAt != catalog.FormatStamp(t0) || w.Plan.Summary.Bookmarks != 2 {
		t.Errorf("bookmarks = %+v", w.Bookmarks)
	}
}

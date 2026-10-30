package api

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/notify"
	"github.com/kodestar/audiosilo-server/internal/upcoming"
)

// book_released as the job and the API make it together: the runner here is
// the server's own announcer (announceReleases) over a fake community service.

// announceMeta is the community service: one series, "The Expanse", whose
// works e1..eN have the dates given; the library's book has ASIN A1 (e1).
type announceMeta struct{ rec *meta.SeriesRecord }

func (m announceMeta) CachedWorkID(context.Context, string, string) (string, bool) { return "", false }

func (m announceMeta) WorkIDs(_ context.Context, books []meta.BookIDs) []meta.WorkID {
	out := make([]meta.WorkID, len(books))
	for i, b := range books {
		if b.ASIN == "A1" {
			out[i].ID = "e1"
		}
	}
	return out
}

func (m announceMeta) WorkSeries(context.Context, string) ([]meta.SeriesRef, error) {
	return []meta.SeriesRef{{ID: m.rec.ID, Name: m.rec.Name}}, nil
}

func (m announceMeta) Series(context.Context, string) (*meta.SeriesRecord, error) {
	cp := *m.rec
	cp.Members = slices.Clone(m.rec.Members)
	return &cp, nil
}

func (m announceMeta) WatchFeed(context.Context, []string, int) ([]meta.FeedItem, error) {
	return nil, nil
}

// releasedEvents is the book_released events so far, oldest first.
func releasedEvents(t *testing.T, e *testEnv) []catalog.ServerEvent {
	t.Helper()
	events, _, err := e.cat.ListServerEvents(context.Background(), 0, 50, notify.KindBookReleased)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(events)
	return events
}

func titlesOf(ev catalog.ServerEvent) []string {
	raw, _ := ev.Data["titles"].([]any)
	out := make([]string, len(raw))
	for i, v := range raw {
		out[i], _ = v.(string)
	}
	return out
}

// The first run is the baseline (a release already out is never announced);
// the run that sees seven preorders come out makes ONE event naming the first
// five and counting all seven; the next run makes none.
func TestBookReleasedAnnouncedOncePerRun(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: "Expanse/1", Title: "Leviathan Wakes",
		Author: "A", Series: "The Expanse", SeriesIndex: 1, ASIN: "A1", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	// e2 came out this month (the baseline's); e3..e9 are preorders out in November.
	rec := &meta.SeriesRecord{ID: "expanse", Name: "The Expanse", Members: []meta.SeriesMember{
		{WorkID: "e1", Position: "1", Title: "Leviathan Wakes", ReleaseDate: "2011"},
		{WorkID: "e2", Position: "2", Title: "Caliban's War", ReleaseDate: "2026-10-01"},
	}}
	for i := 3; i <= 9; i++ {
		rec.Members = append(rec.Members, meta.SeriesMember{WorkID: fmt.Sprintf("e%d", i), Position: fmt.Sprint(i),
			Title: fmt.Sprintf("Book %d", i), ReleaseDate: fmt.Sprintf("2026-11-%02d", i)})
	}
	clock := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	r := upcoming.New(e.cat, announceMeta{rec}, upcoming.Options{
		Now:      func() time.Time { return clock },
		Window:   func() int { return 90 },
		Announce: e.api.announceReleases,
	})

	if err := r.RunOnce(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := releasedEvents(t, e); len(got) != 0 {
		t.Fatalf("the baseline run announced %+v", got)
	}

	clock = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	if err := r.RunOnce(ctx, true); err != nil {
		t.Fatal(err)
	}
	got := releasedEvents(t, e)
	if len(got) != 1 {
		t.Fatalf("one run's releases make one event, got %d", len(got))
	}
	if n, _ := got[0].Data["count"].(float64); n != 7 {
		t.Fatalf("count = %v, want 7", got[0].Data["count"])
	}
	want := []string{
		"Book 3 (The Expanse #3)", "Book 4 (The Expanse #4)", "Book 5 (The Expanse #5)",
		"Book 6 (The Expanse #6)", "Book 7 (The Expanse #7)",
	}
	if titles := titlesOf(got[0]); !slices.Equal(titles, want) {
		t.Fatalf("titles = %q, want the first %d: %q", titles, maxReleaseTitles, want)
	}

	clock = clock.Add(24 * time.Hour)
	if err := r.RunOnce(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := releasedEvents(t, e); len(got) != 1 {
		t.Fatalf("a release is announced once: %d events", len(got))
	}
}

// The titles name a release's series and position when known, and fall back
// to the work id; no release, no event.
func TestAnnounceReleasesTitles(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	e.api.announceReleases(ctx, nil)
	if got := releasedEvents(t, e); len(got) != 0 {
		t.Fatalf("no release made an event: %+v", got)
	}
	e.api.announceReleases(ctx, []upcoming.Release{
		{WorkID: "a", Title: "Alpha", Series: "Saga", Position: "2"},
		{WorkID: "b", Title: "Beta", Series: "Saga"},
		{WorkID: "c"},
	})
	got := releasedEvents(t, e)
	if len(got) != 1 {
		t.Fatalf("events = %+v", got)
	}
	if titles := titlesOf(got[0]); !slices.Equal(titles, []string{"Alpha (Saga #2)", "Beta (Saga)", "c"}) {
		t.Fatalf("titles = %q", titles)
	}
	// The same releases again (a run repeated): the dedupe key holds it to one.
	e.api.announceReleases(ctx, []upcoming.Release{{WorkID: "c"}, {WorkID: "b"}, {WorkID: "a"}})
	if got := releasedEvents(t, e); len(got) != 1 {
		t.Fatalf("the same releases twice made %d events", len(got))
	}
}

package upcoming

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// fakeMeta is the community service for the runner's tests: works by ASIN,
// each work's series, series records, and a watch feed made from them.
type fakeMeta struct {
	mu        sync.Mutex
	works     map[string]string // asin -> work id
	workRefs  map[string][]meta.SeriesRef
	series    map[string]*meta.SeriesRecord
	seriesErr map[string]error
	feedErr   error
	feedItems func(ids []string) []meta.FeedItem

	seriesReads []string
	feeds       [][]string
}

func newFakeMeta() *fakeMeta {
	return &fakeMeta{works: map[string]string{}, workRefs: map[string][]meta.SeriesRef{},
		series: map[string]*meta.SeriesRecord{}, seriesErr: map[string]error{}}
}

func (f *fakeMeta) CachedWorkID(context.Context, string, string) (string, bool) { return "", false }

func (f *fakeMeta) WorkIDs(_ context.Context, books []meta.BookIDs) []meta.WorkID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]meta.WorkID, len(books))
	for i, b := range books {
		out[i] = meta.WorkID{ID: f.works[b.ASIN]}
	}
	return out
}

func (f *fakeMeta) WorkSeries(_ context.Context, id string) ([]meta.SeriesRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	refs, ok := f.workRefs[id]
	if !ok {
		return nil, meta.ErrNotFound
	}
	return refs, nil
}

func (f *fakeMeta) Series(_ context.Context, id string) (*meta.SeriesRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seriesReads = append(f.seriesReads, id)
	if err := f.seriesErr[id]; err != nil {
		return nil, err
	}
	rec, ok := f.series[id]
	if !ok {
		return nil, meta.ErrNotFound
	}
	cp := *rec
	cp.Members = slices.Clone(rec.Members)
	return &cp, nil
}

func (f *fakeMeta) WatchFeed(_ context.Context, ids []string, _ int) ([]meta.FeedItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feeds = append(f.feeds, slices.Clone(ids))
	if f.feedErr != nil {
		return nil, f.feedErr
	}
	if f.feedItems != nil {
		return f.feedItems(ids), nil
	}
	return nil, nil
}

func (f *fakeMeta) reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.seriesReads)
	f.seriesReads = nil
	return out
}

func (f *fakeMeta) setDate(series, work, date string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.series[series].Members {
		if m.WorkID == work {
			f.series[series].Members[i].ReleaseDate = date
		}
	}
}

// fixture is a catalog with one library, a fake service and a runner on a
// clock the test moves.
type fixture struct {
	cat       *catalog.Catalog
	lib       *catalog.Library
	md        *fakeMeta
	r         *Runner
	clock     time.Time
	announced [][]Release
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{md: newFakeMeta(), clock: now}
	f.cat = catalog.New(storetest.Open(t), func() time.Time { return f.clock })
	lib, err := f.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	f.lib = lib
	f.r = New(f.cat, f.md, Options{
		Now:      func() time.Time { return f.clock },
		Window:   func() int { return 90 },
		Announce: func(_ context.Context, rel []Release) { f.announced = append(f.announced, rel) },
	})
	return f
}

// book indexes a book of series at pos with asin, which the service knows as
// work.
func (f *fixture) book(t *testing.T, lib *catalog.Library, series string, pos float64, asin, work string) {
	t.Helper()
	path := fmt.Sprintf("%s/%v", series, pos)
	if _, err := f.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: lib.ID, RelPath: path, Title: path,
		Author: "A", Series: series, SeriesIndex: pos, ASIN: asin, AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	f.md.works[asin] = work
}

// expanse is a community series "The Expanse" (id "expanse") of works e1..eN
// with the given release dates, every work listing it as its series.
func (f *fixture) expanse(dates ...string) {
	rec := &meta.SeriesRecord{ID: "expanse", Name: "The Expanse"}
	for i, d := range dates {
		id := fmt.Sprintf("e%d", i+1)
		rec.Members = append(rec.Members, meta.SeriesMember{WorkID: id, Position: fmt.Sprint(i + 1),
			Title: fmt.Sprintf("Expanse %d", i+1), ReleaseDate: d})
		f.md.workRefs[id] = []meta.SeriesRef{{ID: "expanse", Name: "The Expanse"}}
	}
	f.md.series["expanse"] = rec
}

func (f *fixture) items(t *testing.T) []string {
	t.Helper()
	entries, err := f.cat.UpcomingFor(context.Background(), []catalog.Scope{{LibraryID: f.lib.ID, AllowAll: true}})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.WorkID+"="+e.Kind)
	}
	slices.Sort(out)
	return out
}

func (f *fixture) run(t *testing.T) error {
	t.Helper()
	return f.r.RunOnce(context.Background(), true)
}

func TestRunnerFirstRunIsSilentBaseline(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2012", "2026-10-01", "2026-12-01")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got, want := f.items(t), []string{"e2=missing", "e3=released", "e4=preorder"}; !slices.Equal(got, want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	if len(f.announced) != 0 {
		t.Fatalf("the first run must announce nothing, got %v", f.announced)
	}
	// Run again: still nothing, the baseline release was marked.
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if len(f.announced) != 0 {
		t.Fatalf("a baseline release must never be announced, got %v", f.announced)
	}
	st, _ := f.r.Status(context.Background())
	if st.LastSuccessAt == "" || st.Series != 1 || st.Items != 3 || st.LastError != "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestRunnerPreorderReleaseAnnouncedOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2026-11-12")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=preorder"}) {
		t.Fatalf("items = %v", got)
	}
	// The release day comes: one announcement.
	f.clock = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=released"}) {
		t.Fatalf("items = %v", got)
	}
	if len(f.announced) != 1 || len(f.announced[0]) != 1 || f.announced[0][0].WorkID != "e2" ||
		f.announced[0][0].Title != "Expanse 2" || f.announced[0][0].Series != "The Expanse" || f.announced[0][0].Position != "2" {
		t.Fatalf("announced = %+v", f.announced)
	}
	// And never again.
	f.clock = f.clock.Add(25 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if len(f.announced) != 1 {
		t.Fatalf("a release must be announced exactly once, got %+v", f.announced)
	}
	// A release in the window a later run finds is announced too.
	f.md.mu.Lock()
	f.md.series["expanse"].Members = append(f.md.series["expanse"].Members,
		meta.SeriesMember{WorkID: "e3", Position: "3", Title: "Expanse 3", ReleaseDate: "2026-11-10"})
	f.md.mu.Unlock()
	f.md.feedItems = func([]string) []meta.FeedItem {
		return []meta.FeedItem{{WorkID: "e3", State: "released", Date: "2026-11-10", Title: "The Expanse #3: Expanse 3"}}
	}
	f.clock = f.clock.Add(25 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if len(f.announced) != 2 || f.announced[1][0].WorkID != "e3" {
		t.Fatalf("announced = %+v", f.announced)
	}
}

func TestRunnerHeldElsewhereNotAnnounced(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2026-11-12")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	other, err := f.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Other", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	// Book 2 arrives in another library, filed under no series.
	if _, err := f.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: other.ID, RelPath: "loose/two",
		Title: "two", Author: "A", Series: "Loose", ASIN: "A2", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	f.md.works["A2"] = "e2"
	f.md.workRefs["e2"] = nil
	f.clock = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if len(f.announced) != 0 {
		t.Fatalf("a release the server holds a copy of must not be announced, got %+v", f.announced)
	}
}

func TestRunnerFailureKeepsResults(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2012", "2013")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	before := f.items(t)
	st0, _ := f.r.Status(context.Background())

	// A week on the series is due again, and the service fails.
	f.clock = f.clock.Add(8 * 24 * time.Hour)
	f.md.seriesErr["expanse"] = errors.New("upstream 500")
	f.md.feedErr = errors.New("upstream 500")
	if err := f.run(t); err == nil {
		t.Fatal("the run must fail")
	}
	if got := f.items(t); !slices.Equal(got, before) {
		t.Fatalf("a failed run must keep the earlier items: %v, want %v", got, before)
	}
	st, _ := f.r.Status(context.Background())
	if st.LastSuccessAt != st0.LastSuccessAt || st.LastError == "" || st.LastRunAt == st0.LastRunAt {
		t.Fatalf("status after a failure = %+v (before %+v)", st, st0)
	}
	rs, _ := f.cat.UpcomingRunState(context.Background())
	if rs.Failures != 1 {
		t.Fatalf("failures = %d", rs.Failures)
	}
	// The retry is in an hour.
	if next := f.r.after(context.Background(), errors.New("x")); next.Sub(f.clock) != retryDelay {
		t.Fatalf("retry after %v", next.Sub(f.clock))
	}
}

func TestRunnerGoneSeries(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2012")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	// The series is retired upstream: 404. Its items stay for the grace period.
	delete(f.md.series, "expanse")
	f.clock = f.clock.Add(8 * 24 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	stored, _ := f.cat.MetaSeriesAll(context.Background())
	if !stored["expanse"].Gone {
		t.Fatalf("a 404 marks the series gone: %+v", stored["expanse"])
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=missing"}) {
		t.Fatalf("items within the grace period = %v", got)
	}
	// Past the grace period it is dropped, with its items.
	f.clock = f.clock.Add(8 * 24 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	stored, _ = f.cat.MetaSeriesAll(context.Background())
	if _, ok := stored["expanse"]; ok {
		t.Fatal("a series gone past its grace must be dropped")
	}
	if got := f.items(t); len(got) != 0 {
		t.Fatalf("items of a dropped series = %v", got)
	}
}

func TestRunnerFeedDecidesReads(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2012")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.md.reads(); !slices.Equal(got, []string{"expanse"}) {
		t.Fatalf("first run reads = %v", got)
	}
	// A day on, no news: not read again.
	f.clock = f.clock.Add(24 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.md.reads(); len(got) != 0 {
		t.Fatalf("a series without news must not be read, got %v", got)
	}
	// News (a date changed): read.
	f.md.setDate("expanse", "e2", "2026-11-20")
	f.md.feedItems = func([]string) []meta.FeedItem {
		return []meta.FeedItem{{WorkID: "e2", State: "preorder", Date: "2026-11-20", Title: "The Expanse #2: Expanse 2"}}
	}
	f.clock = f.clock.Add(24 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.md.reads(); !slices.Equal(got, []string{"expanse"}) {
		t.Fatalf("a series with news must be read, got %v", got)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=preorder"}) {
		t.Fatalf("items = %v", got)
	}
}

func TestRunnerFeedBatches(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	stored := map[string]catalog.MetaSeriesRecord{}
	var ids []string
	for i := range 120 {
		id := fmt.Sprintf("s%03d", i)
		ids = append(ids, id)
		stored[id] = catalog.MetaSeriesRecord{ID: id, Name: "Series " + id}
	}
	// A batch holding s007 fills the feed (200 items): it is split until s007
	// stands alone, which then counts as news; nothing else has news.
	f.md.feedItems = func(batch []string) []meta.FeedItem {
		if !slices.Contains(batch, "s007") {
			return nil
		}
		items := make([]meta.FeedItem, meta.MaxFeedItems)
		for i := range items {
			items[i] = meta.FeedItem{WorkID: fmt.Sprintf("w%d", i), Title: "Series s007 #1: x"}
		}
		return items
	}
	news := f.r.news(context.Background(), ids, stored)
	if len(news) != 1 || !news["s007"] {
		t.Fatalf("news = %v", news)
	}
	for _, b := range f.md.feeds {
		if len(b) > meta.MaxFeedSeries {
			t.Fatalf("a feed request named %d series", len(b))
		}
	}
	if first := f.md.feeds[0]; len(first) != meta.MaxFeedSeries {
		t.Fatalf("first batch = %d series", len(first))
	}
	// A feed that is down makes every series news (read them all).
	f.md.feedItems, f.md.feedErr = nil, errors.New("down")
	if news := f.r.news(context.Background(), ids, stored); len(news) != len(ids) {
		t.Fatalf("with the feed down, news = %d series", len(news))
	}
}

func TestMarkNews(t *testing.T) {
	t.Parallel()
	stored := map[string]catalog.MetaSeriesRecord{
		"a": {ID: "a", Name: "Alpha", Members: []catalog.MetaSeriesMember{{WorkID: "a1", ReleaseDate: "2026-11"}}},
		"b": {ID: "b", Name: "Beta", Members: []catalog.MetaSeriesMember{{WorkID: "b1", ReleaseDate: "2026-11-02"}}},
	}
	for _, tc := range []struct {
		item meta.FeedItem
		want []string
	}{
		{meta.FeedItem{WorkID: "a1", Date: "2026-11-01"}, nil},                      // same date, read at its precision
		{meta.FeedItem{WorkID: "b1", Date: "2026-11-03"}, []string{"b"}},            // a moved date
		{meta.FeedItem{WorkID: "new", Title: "Beta #2: New"}, []string{"b"}},        // a new work, named after Beta
		{meta.FeedItem{WorkID: "new", Title: "Gamma #2: New"}, []string{"a", "b"}},  // named after neither: both
		{meta.FeedItem{WorkID: "new", Title: "Alphabet: Soup"}, []string{"a", "b"}}, // a name's prefix is not the name
		{meta.FeedItem{WorkID: "new", Title: "Alpha: A Novella"}, []string{"a"}},    // "<series>: <title>" for an unnumbered one
	} {
		out := map[string]bool{}
		markNews([]string{"a", "b"}, []meta.FeedItem{tc.item}, stored, out)
		var got []string
		for id := range out {
			got = append(got, id)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%+v: news = %v, want %v", tc.item, got, tc.want)
		}
	}
}

func TestRunnerSeriesNamedLoosely(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2012")
	// The local series lacks the article; the work is also in another series.
	f.md.workRefs["e1"] = []meta.SeriesRef{{ID: "other", Name: "Space Operas"}, {ID: "expanse", Name: "The Expanse"}}
	f.book(t, f.lib, "Expanse", 1, "A1", "e1")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=missing"}) {
		t.Fatalf("items = %v", got)
	}
	rows, _ := f.cat.UpcomingSeriesAll(context.Background())
	if len(rows) != 1 || rows[0].SeriesID != "expanse" || rows[0].Name != "Expanse" {
		t.Fatalf("series = %+v", rows)
	}
}

func TestRunnerLightRunReadsOnlyNewSeries(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2012")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.r.RunOnce(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := f.md.reads(); !slices.Equal(got, []string{"expanse"}) {
		t.Fatalf("reads = %v", got)
	}
	if len(f.md.feeds) != 0 {
		t.Fatal("a light run must not read the feed")
	}
	st, _ := f.cat.UpcomingRunState(context.Background())
	if st.LastRunAt != "" {
		t.Fatalf("a light run leaves the status alone: %+v", st)
	}
	f.clock = f.clock.Add(8 * 24 * time.Hour)
	if err := f.r.RunOnce(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := f.md.reads(); len(got) != 0 {
		t.Fatalf("a light run reads only series never read, got %v", got)
	}
}

func TestRunnerScheduleFirstRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if got := f.r.firstRun(context.Background()); !got.Equal(now.Add(firstDelay)) {
		t.Fatalf("first run with none before = %v", got)
	}
	_ = f.cat.SaveUpcomingRunState(context.Background(), catalog.UpcomingRun{LastSuccessAt: stamp(now.Add(-2 * time.Hour))})
	if got := f.r.firstRun(context.Background()); !got.Equal(now.Add(22 * time.Hour)) {
		t.Fatalf("first run after a recent success = %v", got)
	}
	if !strings.HasSuffix(stamp(now), "Z") {
		t.Fatal("stamps are UTC")
	}
}

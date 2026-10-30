package upcoming

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	cached    map[string]string // asin -> the cache's answer ("" a cached "no match")
	workRefs  map[string][]meta.SeriesRef
	workErr   map[string]error
	series    map[string]*meta.SeriesRecord
	seriesErr map[string]error
	feedErr   error
	feedItems func(ids []string) []meta.FeedItem
	// seriesHook runs before each series read (set before a run starts).
	seriesHook func(ctx context.Context, id string) error

	seriesReads []string
	feeds       [][]string
	workCalls   []time.Time // when each WorkIDs call came
}

func newFakeMeta() *fakeMeta {
	return &fakeMeta{works: map[string]string{}, cached: map[string]string{}, workRefs: map[string][]meta.SeriesRef{},
		workErr: map[string]error{}, series: map[string]*meta.SeriesRecord{}, seriesErr: map[string]error{}}
}

func (f *fakeMeta) CachedWorkID(_ context.Context, asin, _ string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.cached[asin]
	return id, ok
}

func (f *fakeMeta) WorkIDs(_ context.Context, books []meta.BookIDs) []meta.WorkID {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workCalls = append(f.workCalls, time.Now())
	out := make([]meta.WorkID, len(books))
	for i, b := range books {
		out[i] = meta.WorkID{ID: f.works[b.ASIN]}
	}
	return out
}

func (f *fakeMeta) WorkSeries(_ context.Context, id string) ([]meta.SeriesRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.workErr[id]; err != nil {
		return nil, err
	}
	refs, ok := f.workRefs[id]
	if !ok {
		return nil, meta.ErrNotFound
	}
	return refs, nil
}

func (f *fakeMeta) Series(ctx context.Context, id string) (*meta.SeriesRecord, error) {
	if f.seriesHook != nil {
		if err := f.seriesHook(ctx, id); err != nil {
			return nil, err
		}
	}
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

// lookups is when each WorkIDs call came.
func (f *fakeMeta) lookups() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.workCalls)
}

// feedCount is how many watch feed requests came (one per full run with a series).
func (f *fakeMeta) feedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.feeds)
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
// clock the test moves, switched on until off is set.
type fixture struct {
	cat       *catalog.Catalog
	lib       *catalog.Library
	md        *fakeMeta
	r         *Runner
	clock     time.Time
	off       atomic.Bool
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
		On:       func() bool { return !f.off.Load() },
		Now:      func() time.Time { return f.clock },
		Window:   func() int { return 90 },
		Announce: func(_ context.Context, rel []Release) { f.announced = append(f.announced, rel) },
	})
	return f
}

// library adds a books library.
func (f *fixture) library(t *testing.T, name string) *catalog.Library {
	t.Helper()
	lib, err := f.cat.CreateLibrary(context.Background(), catalog.Library{Name: name, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return lib
}

// loose indexes a book filed under no series at path with asin, which the
// service knows as work.
func (f *fixture) loose(t *testing.T, lib *catalog.Library, path, asin, work string) {
	t.Helper()
	if _, err := f.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: lib.ID, RelPath: path, Title: path,
		Author: "A", ASIN: asin, AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	f.md.works[asin] = work
}

// holds reports whether the server holds a copy of work anywhere.
func (f *fixture) holds(t *testing.T, work string) bool {
	t.Helper()
	held, err := f.cat.UpcomingOwnedWorks(context.Background(), nil, true, []string{work})
	if err != nil {
		t.Fatal(err)
	}
	return held[work]
}

// announcedIDs is every announcement's work ids, one string per call.
func (f *fixture) announcedIDs() []string {
	var out []string
	for _, rel := range f.announced {
		var ids []string
		for _, r := range rel {
			ids = append(ids, r.WorkID)
		}
		out = append(out, strings.Join(ids, ","))
	}
	return out
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
	f.loose(t, other, "loose/two", "A2", "e2")
	f.clock = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if !f.holds(t, "e2") {
		t.Fatal("a copy in no series must still hold its work")
	}
	if len(f.announced) != 0 {
		t.Fatalf("a release the server holds a copy of must not be announced, got %+v", f.announced)
	}
	// Its identifier cleared, it holds nothing any more.
	if _, err := f.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: other.ID, RelPath: "loose/two",
		Title: "loose/two", Author: "A", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(25 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if f.holds(t, "e2") {
		t.Fatal("a book whose identifier went must hold no work")
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
		"b": {ID: "b", Name: "Beta", Members: []catalog.MetaSeriesMember{{WorkID: "b1", ReleaseDate: "2026-11-02"},
			{WorkID: "c1"}}},
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
		{meta.FeedItem{WorkID: "c1", Undated: true}, nil},                           // undated, and stored undated: no news
		{meta.FeedItem{WorkID: "b1", Undated: true}, []string{"b"}},                 // its date went
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

// A local spelling with an article or a trailing "Series" the community's
// lacks still finds the series.
func TestRunnerSeriesNamedLooselyLocally(t *testing.T) {
	t.Parallel()
	for _, local := range []string{"The Expanse", "Expanse Series"} {
		f := newFixture(t)
		f.expanse("2011", "2012")
		f.md.series["expanse"].Name = "Expanse"
		f.md.workRefs["e1"] = []meta.SeriesRef{{ID: "expanse", Name: "Expanse"}}
		f.book(t, f.lib, local, 1, "A1", "e1")
		if err := f.run(t); err != nil {
			t.Fatal(err)
		}
		if got := f.items(t); !slices.Equal(got, []string{"e2=missing"}) {
			t.Fatalf("%s: items = %v", local, got)
		}
	}
}

// A re-resolution that fails to read a work and matches nothing keeps the
// series it had (and its items) for the next run, never "none found".
func TestRunnerSeriesReadFailureKeepsSeries(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2012", "2013")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	f.book(t, f.lib, "The Expanse", 3, "A3", "e3")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(31 * 24 * time.Hour)
	f.md.workErr["e1"] = errors.New("upstream 500")
	f.md.workRefs["e3"] = []meta.SeriesRef{{ID: "other", Name: "Space Operas"}}
	if err := f.run(t); err == nil {
		t.Fatal("a failed read must fail the run")
	}
	rows, _ := f.cat.UpcomingSeriesAll(context.Background())
	if len(rows) != 1 || rows[0].SeriesID != "expanse" {
		t.Fatalf("series = %+v", rows)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=missing"}) {
		t.Fatalf("items = %v", got)
	}
}

// A cached "no match" (the persistent cache keeps one however old) is never
// taken for an answer: the book is looked up.
func TestRunnerCachedNoMatchIsLookedUp(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2012")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	f.md.cached["A1"] = ""
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=missing"}) {
		t.Fatalf("items = %v", got)
	}
}

// A book's work is looked up again after a month, not from the cache: a merge
// upstream moves the book to the surviving work, and the series it was missing
// from holds it again.
func TestRunnerWorksLookedUpAgain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2012", "2013")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	f.book(t, f.lib, "The Expanse", 2, "A2", "e2-old") // merged into e2 upstream, later
	f.book(t, f.lib, "The Expanse", 3, "A3", "e3")
	f.md.cached["A2"] = "e2-old"
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=missing"}) {
		t.Fatalf("items = %v", got)
	}
	f.md.works["A2"] = "e2" // the merge
	f.clock = f.clock.Add(24 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=missing"}) {
		t.Fatalf("a day on, the answer stands: items = %v", got)
	}
	f.clock = f.clock.Add(30 * 24 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); len(got) != 0 {
		t.Fatalf("a month on, the book holds the merged work: items = %v", got)
	}
}

// A series resolved to none is asked again as soon as a book of it gets a new
// work (its ASIN corrected), not a month later.
func TestRunnerSeriesResolvedAgainOnANewWork(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2012")
	f.md.workRefs["x1"] = []meta.SeriesRef{{ID: "other", Name: "Space Operas"}}
	f.book(t, f.lib, "The Expanse", 1, "A1", "x1") // a wrong ASIN
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if rows, _ := f.cat.UpcomingSeriesAll(context.Background()); len(rows) != 1 || rows[0].SeriesID != "" {
		t.Fatalf("series = %+v", rows)
	}
	if _, err := f.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: f.lib.ID, RelPath: "The Expanse/1",
		Title: "The Expanse/1", Author: "A", Series: "The Expanse", SeriesIndex: 1, ASIN: "A1B", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	f.md.works["A1B"] = "e1"
	f.clock = f.clock.Add(24 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=missing"}) {
		t.Fatalf("items = %v", got)
	}
}

// A community series read for the first time is a baseline on its own: a
// release the series watched before finds that same run is still announced.
func TestRunnerNewSeriesDoesNotSilenceARelease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2026-11-12")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	// A month on, the series is resolved again and finds a second community
	// series named like it, with a release from before the server watched it.
	f.md.series["expanse-2"] = &meta.SeriesRecord{ID: "expanse-2", Name: "The Expanse", Members: []meta.SeriesMember{
		{WorkID: "e1", Position: "1", Title: "Expanse 1", ReleaseDate: "2011"},
		{WorkID: "x2", Position: "2", Title: "Old News", ReleaseDate: "2026-11-01"},
	}}
	f.md.workRefs["e1"] = []meta.SeriesRef{{ID: "expanse", Name: "The Expanse"}, {ID: "expanse-2", Name: "The Expanse"}}
	f.clock = time.Date(2026, 11, 30, 9, 0, 0, 0, time.UTC)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.announcedIDs(); !slices.Equal(got, []string{"e2"}) {
		t.Fatalf("announced = %v, want e2 alone", got)
	}
}

// A community series whose read failed is not classified with its siblings: its
// releases from before the server watched it stay a baseline once it is read.
func TestRunnerUnreadSeriesStaysUnclassified(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2012")
	f.md.series["expanse-2"] = &meta.SeriesRecord{ID: "expanse-2", Name: "The Expanse", Members: []meta.SeriesMember{
		{WorkID: "x2", Position: "2", Title: "Old News", ReleaseDate: "2026-10-20"},
	}}
	f.md.workRefs["e1"] = []meta.SeriesRef{{ID: "expanse", Name: "The Expanse"}, {ID: "expanse-2", Name: "The Expanse"}}
	f.md.seriesErr["expanse-2"] = errors.New("upstream 500")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.run(t); err == nil {
		t.Fatal("a failed read must fail the run")
	}
	delete(f.md.seriesErr, "expanse-2")
	f.clock = f.clock.Add(time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=missing", "x2=released"}) {
		t.Fatalf("items = %v", got)
	}
	if len(f.announced) != 0 {
		t.Fatalf("a series' first read is a baseline, got %v", f.announcedIDs())
	}
}

// A baseline in one library's new series never silences a release another
// library's series found the same run.
func TestRunnerBaselineElsewhereDoesNotSilence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2026-11-12")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	kids := f.library(t, "Kids")
	f.book(t, kids, "The Expanse", 1, "K1", "e1")
	f.clock = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.announcedIDs(); !slices.Equal(got, []string{"e2"}) {
		t.Fatalf("announced = %v, want e2", got)
	}
}

// An announced release whose item leaves (a copy arrives) and comes back (the
// copy goes) is not announced again.
func TestRunnerReleaseAnnouncedOnceAcrossACopy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.expanse("2011", "2026-11-12")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	f.clock = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	f.book(t, f.lib, "The Expanse", 2, "A2", "e2")
	f.clock = f.clock.Add(25 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); len(got) != 0 {
		t.Fatalf("items with the copy = %v", got)
	}
	if err := f.cat.DeleteBookAt(context.Background(), f.lib.ID, "The Expanse/2"); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(25 * time.Hour)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.items(t); !slices.Equal(got, []string{"e2=released"}) {
		t.Fatalf("items after the copy went = %v", got)
	}
	if got := f.announcedIDs(); !slices.Equal(got, []string{"e2"}) {
		t.Fatalf("announced = %v, want e2 once", got)
	}
}

// Releases are announced in series order, by number: 2 before 10.
func TestRunnerAnnouncesInSeriesOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dates := []string{"2011"}
	for i := 2; i <= 10; i++ {
		d := "2012"
		if i == 2 || i == 10 {
			d = "2026-11-12"
		}
		dates = append(dates, d)
	}
	f.expanse(dates...)
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	f.clock = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.announcedIDs(); !slices.Equal(got, []string{"e2,e10"}) {
		t.Fatalf("announced = %v", got)
	}
}

// Switched off during a run, the job stops at once: no further read, no
// announcement, no status, and no retry (a cancellation, not a failure).
func TestRunnerSwitchedOffMidRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.r.offCheck = 5 * time.Millisecond
	f.expanse("2011", "2026-11-12")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	st0, _ := f.cat.UpcomingRunState(context.Background())
	// Two weeks on the series is due; it is switched off while being read.
	f.md.seriesHook = func(ctx context.Context, _ string) error {
		f.off.Store(true)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return errors.New("the run was not stopped")
		}
	}
	f.clock = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	err := f.run(t)
	if !errors.Is(err, errSwitchedOff) || !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v, want switched off", err)
	}
	if len(f.announced) != 0 {
		t.Fatalf("announced = %v", f.announcedIDs())
	}
	if st, _ := f.cat.UpcomingRunState(context.Background()); st != st0 {
		t.Fatalf("a stopped run records nothing: %+v, was %+v", st, st0)
	}
	if next := f.r.after(context.Background(), err); next.Sub(f.clock) < interval-jitter {
		t.Fatalf("a stopped run is no failure to retry: next in %v", next.Sub(f.clock))
	}
	// Off, a run makes no call at all.
	if err := f.run(t); !errors.Is(err, errSwitchedOff) {
		t.Fatalf("run while off = %v", err)
	}
	// Back on, the release is found and announced.
	f.md.seriesHook = nil
	f.off.Store(false)
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.announcedIDs(); !slices.Equal(got, []string{"e2"}) {
		t.Fatalf("announced = %v", got)
	}
}

// runSchedule starts the runner's schedule on a fixture whose clock is the
// real time (the schedule waits in real time); it stops with the test.
func runSchedule(t *testing.T, f *fixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.r.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// eventually waits up to five seconds for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A burst of kicks (a scan adding books) makes one light run, once the kicks
// have been quiet for the settle time; it leaves the run status alone.
func TestRunnerKicksSettle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clock = time.Now() // set before the schedule starts: the runner and the catalog read it
	f.r.settle = 150 * time.Millisecond
	f.expanse("2011", "2012")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	runSchedule(t, f)
	var last time.Time
	for range 5 {
		f.r.Kick()
		last = time.Now()
		time.Sleep(25 * time.Millisecond)
	}
	eventually(t, "the kicked run", func() bool { return len(f.md.lookups()) > 0 })
	calls := f.md.lookups()
	if gap := calls[0].Sub(last); gap < f.r.settle/2 {
		t.Fatalf("the run came %v after the last kick, before the kicks settled (%v)", gap, f.r.settle)
	}
	time.Sleep(3 * f.r.settle)
	if n := len(f.md.lookups()); n != 1 {
		t.Fatalf("a burst of kicks made %d runs, want 1", n)
	}
	if f.md.feedCount() != 0 {
		t.Fatal("a light run asks no watch feed")
	}
	if st, _ := f.cat.UpcomingRunState(context.Background()); st.LastRunAt != "" || st.LastSuccessAt != "" {
		t.Fatalf("a light run records no status: %+v", st)
	}
}

// Switching the job on wakes the schedule: with no success in the last day, a
// full run comes at once (not an hour after the start); woken again after it,
// or while off, nothing runs.
func TestRunnerWakeOnSwitchOn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clock = time.Now()
	f.expanse("2011", "2012")
	f.book(t, f.lib, "The Expanse", 1, "A1", "e1")
	f.off.Store(true)
	runSchedule(t, f)
	f.r.Wake() // still off: nothing to run
	time.Sleep(100 * time.Millisecond)
	if len(f.md.lookups()) != 0 || f.md.feedCount() != 0 {
		t.Fatal("a woken job that is off makes no call")
	}
	f.off.Store(false)
	f.r.Wake()
	eventually(t, "the run on switch-on", func() bool {
		st, _ := f.cat.UpcomingRunState(context.Background())
		return st.LastSuccessAt != ""
	})
	if n := f.md.feedCount(); n != 1 {
		t.Fatalf("the run on switch-on is a full one: %d feed requests", n)
	}
	// Woken again within the day: the last success stands, no run.
	f.r.Wake()
	time.Sleep(150 * time.Millisecond)
	if n := f.md.feedCount(); n != 1 {
		t.Fatalf("a wake after a recent success ran again: %d feed requests", n)
	}
	st, _ := f.cat.UpcomingRunState(context.Background())
	if next, ok := parseStamp(st.NextRunAt); !ok || next.Sub(f.clock) < interval-jitter {
		t.Fatalf("after the run, the next is a day on: %+v", st)
	}
}

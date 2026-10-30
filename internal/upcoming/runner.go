// Package upcoming is upcoming releases (Track U1): a background job that reads
// the community metadata for the series the server holds and works out, per
// local series, the volumes it doesn't hold (preorders, recent releases,
// missing volumes), for GET /me/upcoming, and announces a new release to the
// admin (book_released). Everything it stores is derived (catalog/upcoming.go)
// and rebuilt by the next run.
//
// A run, once a day (and an hour after the start):
//  1. each owned book in a series with an identifier gets its community work id
//     (the meta cache's answer, else a lookup under the service's own budget);
//  2. each local series (library, folded name) gets the community series it is,
//     from its books' works' series named like it (match.SeriesKey, as the
//     /meta rails place books; then give or take an article or a trailing
//     "Series"), refreshed monthly or when the series gains books;
//  3. metaserve's watch feed, 50 series a request, says which series have news;
//  4. series/{id} is read for a new series, one with news, or one older than a
//     week; a 404 marks it gone (dropped after a week);
//  5. each local series is classified (Classify) and its items replaced.
//
// A failure keeps what earlier runs stored (a series is replaced only by a
// read that worked), and a failed run is tried again in an hour, three times.
// Metadata off, or the job switched off, makes no call at all.
package upcoming

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/pool"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// Metadata is what the job asks the community metadata service
// (*meta.Service).
type Metadata interface {
	CachedWorkID(ctx context.Context, asin, isbn string) (string, bool)
	WorkIDs(ctx context.Context, books []meta.BookIDs) []meta.WorkID
	WorkSeries(ctx context.Context, id string) ([]meta.SeriesRef, error)
	Series(ctx context.Context, id string) (*meta.SeriesRecord, error)
	WatchFeed(ctx context.Context, series []string, window int) ([]meta.FeedItem, error)
}

// Release is one new release book_released tells the admin about.
type Release struct {
	WorkID   string
	Title    string
	Series   string
	Position string
}

// Options configure a Runner.
type Options struct {
	// On reports whether the job may run now: metadata on AND upcoming.enabled.
	On func() bool
	// Window is the recent-release window in days.
	Window func() int
	// Announce tells the admin about new releases (one call per run, never
	// with none). nil announces nothing.
	Announce func(ctx context.Context, releases []Release)
	Now      func() time.Time
	Log      *slog.Logger
}

// Schedule.
const (
	firstDelay  = time.Hour      // after the start
	interval    = 24 * time.Hour // between successful runs
	jitter      = time.Hour      // +- on interval
	retryDelay  = time.Hour      // after a failed run
	maxRetries  = 3              // failed runs retried before waiting for the next day
	offPoll     = time.Minute    // while off, how often the switch is looked at
	settleKicks = 30 * time.Second
	maxSettle   = 10 * time.Minute

	// resolveAge: a local series' community series are looked up again after
	// it, or when the series gains books; noMatchAge: a book without a match is
	// looked up again after it.
	resolveAge = 30 * 24 * time.Hour
	noMatchAge = 30 * 24 * time.Hour
	// seriesAge: a series without news is read again after it; goneGrace: a
	// series answering 404 is dropped after it; goneRetry: how often a gone one
	// is asked again meanwhile.
	seriesAge = 7 * 24 * time.Hour
	goneGrace = 7 * 24 * time.Hour
	goneRetry = 24 * time.Hour

	// workBatch is how many uncached identifiers one WorkIDs call takes (each
	// call runs under the service's compose budget).
	workBatch = 100
	// worksPerSeries is how many of a local series' works are asked for its
	// series before giving up; fetches how many series reads run at once.
	worksPerSeries = 3
	fetches        = 2
	// staleAfter: GET /me/upcoming says "stale" with no successful run in it.
	staleAfter = 48 * time.Hour
)

// Runner is the upcoming releases job.
type Runner struct {
	cat  *catalog.Catalog
	meta Metadata
	opt  Options
	log  *slog.Logger

	kick chan struct{}
	wake chan struct{}
	// mu makes one run at a time (the schedule's, a kick's, a test's).
	mu sync.Mutex
	// settle is how long kicks must be quiet before a kick's run.
	settle time.Duration
}

// New returns a Runner; Run starts its schedule.
func New(cat *catalog.Catalog, md Metadata, opt Options) *Runner {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Window == nil {
		opt.Window = func() int { return 90 }
	}
	if opt.On == nil {
		opt.On = func() bool { return true }
	}
	return &Runner{cat: cat, meta: md, opt: opt, log: opt.Log,
		kick: make(chan struct{}, 1), wake: make(chan struct{}, 1), settle: settleKicks}
}

// Kick asks for a light run once the kicks settle: a scan or an edit may have
// added books (new works to resolve, new series to read). Never blocks.
func (r *Runner) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Wake makes the schedule look again at once (the job was switched on).
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run keeps the schedule until ctx ends: the first run an hour after the start
// (sooner when an earlier success is due again), then daily with jitter, a
// failed run again in an hour (three times), and a light run after kicks.
func (r *Runner) Run(ctx context.Context) {
	next := r.firstRun(ctx)
	r.saveNext(ctx, next)
	for {
		wait := time.Until(next)
		if !r.opt.On() {
			wait = min(wait, offPoll)
		}
		t := time.NewTimer(max(wait, 0))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-r.wake:
			t.Stop()
			if r.opt.On() {
				st, _ := r.cat.UpcomingRunState(ctx)
				if r.due(st) {
					next = r.opt.Now()
				}
			}
			continue
		case <-r.kick:
			t.Stop()
			if !r.settled(ctx) {
				return
			}
			if r.opt.On() {
				_ = r.RunOnce(ctx, false)
			}
			continue
		case <-t.C:
		}
		if !r.opt.On() {
			if !r.opt.Now().Before(next) {
				next = r.opt.Now().Add(offPoll) // looked at again until it is on
			}
			continue
		}
		if !r.opt.Now().Before(next) {
			err := r.RunOnce(ctx, true)
			next = r.after(ctx, err)
			r.saveNext(ctx, next)
		}
	}
}

// due reports whether no run has succeeded within the interval.
func (r *Runner) due(st catalog.UpcomingRun) bool {
	last, ok := parseStamp(st.LastSuccessAt)
	return !ok || r.opt.Now().Sub(last) >= interval
}

// firstRun is when the first run after the start is: an hour on, or the next
// daily run when a success is recent.
func (r *Runner) firstRun(ctx context.Context) time.Time {
	now := r.opt.Now()
	st, err := r.cat.UpcomingRunState(ctx)
	if err == nil {
		if last, ok := parseStamp(st.LastSuccessAt); ok {
			return maxTime(now.Add(firstDelay), last.Add(interval))
		}
	}
	return now.Add(firstDelay)
}

// after is when the run after a full one is.
func (r *Runner) after(ctx context.Context, err error) time.Time {
	now := r.opt.Now()
	if err != nil && !errors.Is(err, context.Canceled) {
		if st, e := r.cat.UpcomingRunState(ctx); e == nil && st.Failures <= maxRetries {
			return now.Add(retryDelay)
		}
	}
	return now.Add(interval + time.Duration(rand.Int64N(int64(2*jitter))) - jitter) //nolint:gosec // schedule jitter, not a secret
}

func (r *Runner) saveNext(ctx context.Context, next time.Time) {
	st, err := r.cat.UpcomingRunState(ctx)
	if err != nil {
		return
	}
	st.NextRunAt = stamp(next)
	if err := r.cat.SaveUpcomingRunState(ctx, st); err != nil && ctx.Err() == nil {
		r.log.Debug("upcoming: saving the schedule failed", "err", err)
	}
}

// settled waits until no Kick has come for r.settle, at most maxSettle.
func (r *Runner) settled(ctx context.Context) bool {
	quiet := time.NewTimer(r.settle)
	defer quiet.Stop()
	limit := time.NewTimer(maxSettle)
	defer limit.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-quiet.C:
			return true
		case <-limit.C:
			return true
		case <-r.kick:
			quiet.Reset(r.settle)
		}
	}
}

// localSeries is one series of one library, with its books.
type localSeries struct {
	libraryID int64
	key       string
	name      string
	books     []catalog.UpcomingBook
}

type seriesKey struct {
	lib int64
	key string
}

// RunOnce runs the pipeline once. full is a scheduled run (the watch feed and
// the due series reads, then the run status); a light one (after kicks)
// resolves new books and series and reads only series never read, and leaves
// the status alone. The error is the run's failure (some reads failed; what
// worked is kept).
func (r *Runner) RunOnce(ctx context.Context, full bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opt.Now()
	err := r.run(ctx, full, now)
	if full && ctx.Err() == nil {
		r.record(ctx, now, err)
	}
	if err != nil && ctx.Err() == nil {
		r.log.Warn("upcoming releases: run failed", "err", err)
	}
	return err
}

func (r *Runner) run(ctx context.Context, full bool, now time.Time) error {
	books, err := r.cat.UpcomingBooks(ctx)
	if err != nil {
		return fmt.Errorf("reading the books: %w", err)
	}
	works, err := r.resolveWorks(ctx, books, now)
	if err != nil {
		return err
	}
	locals := groupSeries(books)
	failed, err := r.resolveSeries(ctx, locals, works, now)
	if err != nil {
		return err
	}
	n, err := r.fetchSeries(ctx, full, now)
	if err != nil {
		return err
	}
	failed += n
	stored, err := r.classify(ctx, locals, works, now)
	if err != nil {
		return err
	}
	if err := r.announce(ctx, stored, now); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d community reads failed", failed)
	}
	return nil
}

// groupSeries groups the books by local series, in library order.
func groupSeries(books []catalog.UpcomingBook) []*localSeries {
	var out []*localSeries
	by := map[seriesKey]*localSeries{}
	for _, b := range books {
		k := seriesKey{b.LibraryID, b.SeriesKey}
		s := by[k]
		if s == nil {
			s = &localSeries{libraryID: b.LibraryID, key: b.SeriesKey, name: b.SeriesName}
			by[k] = s
			out = append(out, s)
		}
		s.books = append(s.books, b)
	}
	return out
}

// byLocal groups the resolved rows by local series.
func byLocal(rows []catalog.UpcomingSeries) map[seriesKey][]catalog.UpcomingSeries {
	out := map[seriesKey][]catalog.UpcomingSeries{}
	for _, row := range rows {
		k := seriesKey{row.LibraryID, row.SeriesKey}
		out[k] = append(out[k], row)
	}
	return out
}

// distinct is the non-empty keys of xs, each once, in order.
func distinct[T any](xs []T, key func(T) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if k := key(x); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// resolveWorks returns each book's community work id ("" for none or not yet
// known), resolving the books whose identifiers it hasn't resolved (or whose
// identifiers changed, or whose no-match is a month old): from the cache, else
// by lookups a batch at a time. A lookup that fails is left for the next run.
func (r *Runner) resolveWorks(ctx context.Context, books []catalog.UpcomingBook, now time.Time) (map[catalog.Ref]string, error) {
	stored, err := r.cat.UpcomingOwnedAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the resolved books: %w", err)
	}
	works := map[catalog.Ref]string{}
	type want struct {
		ref catalog.Ref
		key string
		ids meta.BookIDs
	}
	var asks []want
	seen := map[catalog.Ref]bool{}
	var saved []catalog.UpcomingOwned
	for _, b := range books {
		if seen[b.Ref] {
			continue
		}
		seen[b.Ref] = true
		key := meta.IDKey(b.ASIN, b.ISBN)
		if key == "" {
			continue
		}
		if o, ok := stored[b.Ref]; ok && o.IDKey == key {
			checked, _ := parseStamp(o.CheckedAt)
			if o.WorkID != "" || now.Sub(checked) < noMatchAge {
				works[b.Ref] = o.WorkID
				continue
			}
		}
		if id, ok := r.meta.CachedWorkID(ctx, b.ASIN, b.ISBN); ok {
			works[b.Ref] = id
			saved = append(saved, catalog.UpcomingOwned{Ref: b.Ref, IDKey: key, WorkID: id, CheckedAt: stamp(now)})
			continue
		}
		asks = append(asks, want{b.Ref, key, meta.BookIDs{ASIN: b.ASIN, ISBN: b.ISBN}})
	}
	for chunk := range slices.Chunk(asks, workBatch) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		ids := make([]meta.BookIDs, len(chunk))
		for i, a := range chunk {
			ids[i] = a.ids
		}
		for i, w := range r.meta.WorkIDs(ctx, ids) {
			if w.Failed {
				continue
			}
			works[chunk[i].ref] = w.ID
			saved = append(saved, catalog.UpcomingOwned{Ref: chunk[i].ref, IDKey: chunk[i].key, WorkID: w.ID, CheckedAt: stamp(now)})
		}
	}
	if err := r.cat.SaveUpcomingOwned(ctx, saved); err != nil {
		return nil, fmt.Errorf("saving the resolved books: %w", err)
	}
	return works, nil
}

// resolveSeries finds the community series of each local series that is due
// (never resolved, a month old, or holding more books than when it was), and
// forgets those the library no longer holds. It returns how many failed.
func (r *Runner) resolveSeries(ctx context.Context, locals []*localSeries, works map[catalog.Ref]string, now time.Time) (int, error) {
	rows, err := r.cat.UpcomingSeriesAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading the series: %w", err)
	}
	have := byLocal(rows)
	live := map[seriesKey]bool{}
	failed := 0
	for _, l := range locals {
		k := seriesKey{l.libraryID, l.key}
		live[k] = true
		if ctx.Err() != nil {
			return failed, ctx.Err()
		}
		if prior := have[k]; len(prior) > 0 {
			resolved, _ := parseStamp(prior[0].ResolvedAt)
			if now.Sub(resolved) < resolveAge && len(l.books) <= prior[0].Books {
				continue
			}
		}
		var ids []string
		seenWork := map[string]bool{}
		for _, b := range l.books {
			if w := works[b.Ref]; w != "" && !seenWork[w] && len(seenWork) < worksPerSeries {
				seenWork[w] = true
				ids = append(ids, w)
			}
		}
		if len(ids) == 0 {
			continue // nothing to ask by yet: a later run, once a book resolves
		}
		found, ok := r.seriesOf(ctx, l.key, ids)
		if !ok {
			failed++
			continue
		}
		if err := r.cat.SetUpcomingSeries(ctx, l.libraryID, l.key, l.name, len(l.books), found, stamp(now)); err != nil {
			return failed, fmt.Errorf("saving a series: %w", err)
		}
	}
	var gone []catalog.UpcomingSeries
	for k, prior := range have {
		if !live[k] {
			gone = append(gone, prior[0])
		}
	}
	if err := r.cat.DropUpcomingSeries(ctx, gone); err != nil {
		return failed, fmt.Errorf("dropping series: %w", err)
	}
	return failed, nil
}

// seriesOf is the community series a local series (by its folded key) is,
// read from the series of its works: those named like it, primaries first (a
// variant reading order only when no primary is); else those named like it give
// or take an article or a trailing "Series". ok is false when every work's read
// failed.
func (r *Runner) seriesOf(ctx context.Context, key string, workIDs []string) (ids []string, ok bool) {
	var refs []meta.SeriesRef
	for _, w := range workIDs {
		got, err := r.meta.WorkSeries(ctx, w)
		switch {
		case err == nil:
			ok = true
			refs = append(refs, got...)
		case errors.Is(err, meta.ErrNotFound):
			ok = true
		default:
			if ctx.Err() == nil {
				r.log.Debug("upcoming: reading a work's series failed", "work", w, "err", err)
			}
		}
	}
	for _, fold := range []func(string) string{
		func(n string) string { return cmp.Or(match.SeriesKey(n), n) },
		func(n string) string { return match.SeriesKey(match.NormalizeSeries(n)) },
	} {
		want := fold(key)
		var primaries, variants []string
		for _, ref := range refs {
			if want == "" || fold(ref.Name) != want {
				continue
			}
			if ref.OrderingOf == "" {
				primaries = appendNew(primaries, ref.ID)
			} else {
				variants = appendNew(variants, ref.ID)
			}
		}
		if len(primaries) > 0 {
			return primaries, ok
		}
		if len(variants) > 0 {
			return variants, ok
		}
	}
	return nil, ok
}

func appendNew(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

// fetchSeries reads the community series that are due, by the watch feed's
// news (a full run) or because they were never read (any run), and drops the
// series no local series names any more and those gone past their grace. It
// returns how many reads failed.
func (r *Runner) fetchSeries(ctx context.Context, full bool, now time.Time) (int, error) {
	rows, err := r.cat.UpcomingSeriesAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading the series: %w", err)
	}
	stored, err := r.cat.MetaSeriesAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading the community series: %w", err)
	}
	ids := distinct(rows, func(row catalog.UpcomingSeries) string { return row.SeriesID })
	slices.Sort(ids)
	var news map[string]bool
	if full {
		news = r.news(ctx, ids, stored)
	}
	var due []string
	for _, id := range ids {
		rec, ok := stored[id]
		fetched, _ := parseStamp(rec.FetchedAt)
		switch {
		case !ok:
			due = append(due, id)
		case !full:
		case rec.Gone:
			if now.Sub(fetched) >= goneRetry {
				due = append(due, id)
			}
		case news[id] || now.Sub(fetched) >= seriesAge:
			due = append(due, id)
		}
	}
	var mu sync.Mutex
	failed := 0
	var saveErr error
	pool.Each(ctx, fetches, due, func() bool { return ctx.Err() == nil }, func(id *string) {
		rec, err := r.meta.Series(ctx, *id)
		var werr error
		switch {
		case err == nil:
			werr = r.cat.SaveMetaSeries(ctx, toRecord(*id, rec, now))
		case errors.Is(err, meta.ErrNotFound):
			werr = r.cat.MarkMetaSeriesGone(ctx, *id, stamp(now))
		default:
			if ctx.Err() == nil {
				r.log.Debug("upcoming: reading a series failed", "series", *id, "err", err)
			}
			mu.Lock()
			failed++
			mu.Unlock()
			return
		}
		if werr != nil {
			mu.Lock()
			saveErr = cmp.Or(saveErr, werr)
			mu.Unlock()
		}
	})
	if err := cmp.Or(ctx.Err(), saveErr); err != nil {
		return failed, err
	}
	// Forget a series no local series names, and one gone past its grace.
	stored, err = r.cat.MetaSeriesAll(ctx)
	if err != nil {
		return failed, err
	}
	var drop []string
	for id, rec := range stored {
		goneAt, _ := parseStamp(rec.GoneAt)
		if _, named := slices.BinarySearch(ids, id); !named || (rec.Gone && now.Sub(goneAt) >= goneGrace) {
			drop = append(drop, id)
		}
	}
	slices.Sort(drop)
	return failed, r.cat.DropMetaSeries(ctx, drop)
}

func toRecord(id string, rec *meta.SeriesRecord, now time.Time) catalog.MetaSeriesRecord {
	out := catalog.MetaSeriesRecord{ID: id, Name: rec.Name, FetchedAt: stamp(now)}
	for _, m := range rec.Members {
		var authors []catalog.MetaSeriesPerson
		for _, a := range m.Authors {
			authors = append(authors, catalog.MetaSeriesPerson(a))
		}
		out.Members = append(out.Members, catalog.MetaSeriesMember{WorkID: m.WorkID, Position: m.Position,
			Title: m.Title, Authors: authors, CoverURL: m.CoverURL, ReleaseDate: m.ReleaseDate, AddedAt: m.AddedAt})
	}
	return out
}

// news reads the watch feed for ids, a batch at a time, and returns the series
// with news: a work the feed lists that the stored series doesn't hold, or
// holds with another date; a work no stored series of the batch holds marks
// the series it is named after (the item's title starts with the series' name),
// else every series of the batch. A batch the feed can't answer, or one that
// fills the feed (its oldest items may be cut), is split, down to a single
// series, which then counts as news.
func (r *Runner) news(ctx context.Context, ids []string, stored map[string]catalog.MetaSeriesRecord) map[string]bool {
	out := map[string]bool{}
	var walk func(batch []string)
	walk = func(batch []string) {
		if len(batch) == 0 || ctx.Err() != nil {
			return
		}
		items, err := r.meta.WatchFeed(ctx, batch, r.opt.Window())
		if err != nil || len(items) >= meta.MaxFeedItems {
			if len(batch) == 1 {
				out[batch[0]] = true
				return
			}
			if err != nil && !errors.Is(err, meta.ErrInvalidSlug) {
				// The feed is down (or an older metaserve without one): no
				// sense asking it again per series; every series is news.
				for _, id := range batch {
					out[id] = true
				}
				return
			}
			walk(batch[:len(batch)/2])
			walk(batch[len(batch)/2:])
			return
		}
		markNews(batch, items, stored, out)
	}
	for chunk := range slices.Chunk(ids, meta.MaxFeedSeries) {
		walk(chunk)
	}
	return out
}

// markNews marks in out the series of batch that items say have news.
func markNews(batch []string, items []meta.FeedItem, stored map[string]catalog.MetaSeriesRecord, out map[string]bool) {
	for _, it := range items {
		known := false
		for _, id := range batch {
			rec, ok := stored[id]
			if !ok {
				continue
			}
			for _, m := range rec.Members {
				if m.WorkID != it.WorkID {
					continue
				}
				known = true
				if it.Date != "" && datePrefix(m.ReleaseDate) != it.Date {
					out[id] = true
				}
			}
		}
		if known {
			continue
		}
		named := false
		for _, id := range batch {
			if rec, ok := stored[id]; ok && rec.Name != "" &&
				(strings.HasPrefix(it.Title, rec.Name+" #") || strings.HasPrefix(it.Title, rec.Name+":")) {
				out[id] = true
				named = true
			}
		}
		if !named {
			for _, id := range batch {
				out[id] = true
			}
		}
	}
}

// datePrefix is a stated release date as the feed dates it: its first day
// (YYYY-01-01 for a bare year).
func datePrefix(v string) string {
	t, ok := parseReleaseDate(v)
	if !ok {
		return ""
	}
	return t.Format(time.DateOnly)
}

// classify works out every local series' items from what is stored, and
// returns the community series it read.
func (r *Runner) classify(ctx context.Context, locals []*localSeries, works map[catalog.Ref]string, now time.Time) (map[string]catalog.MetaSeriesRecord, error) {
	rows, err := r.cat.UpcomingSeriesAll(ctx)
	if err != nil {
		return nil, err
	}
	stored, err := r.cat.MetaSeriesAll(ctx)
	if err != nil {
		return nil, err
	}
	by := byLocal(rows)
	window := r.opt.Window()
	for _, l := range locals {
		var series []Series
		classified := true
		for _, row := range by[seriesKey{l.libraryID, l.key}] {
			classified = classified && row.Classified
			if rec, ok := stored[row.SeriesID]; ok && len(rec.Members) > 0 {
				series = append(series, Series{ID: rec.ID, Members: rec.Members})
			}
		}
		if len(series) == 0 {
			continue // no community series read for it (yet): earlier items stay
		}
		owned := make([]OwnedBook, len(l.books))
		for i, b := range l.books {
			owned[i] = OwnedBook{WorkID: works[b.Ref], Position: b.Position, PositionEnd: b.PositionEnd}
		}
		items := Classify(series, owned, now, window)
		if err := r.cat.ReplaceUpcomingItems(ctx, l.libraryID, l.key, items, !classified, stamp(now)); err != nil {
			return nil, fmt.Errorf("saving a series' items: %w", err)
		}
	}
	return stored, nil
}

// announce tells the admin about the releases no earlier run has: one call for
// all of them, each work once. A work the server holds a copy of somewhere, or
// one announced before through another series, is marked without a word.
// stored is the community series classify read.
func (r *Runner) announce(ctx context.Context, stored map[string]catalog.MetaSeriesRecord, now time.Time) error {
	items, already, err := r.cat.UnannouncedReleases(ctx)
	if err != nil || len(items) == 0 {
		return err
	}
	ids := distinct(items, func(it catalog.UpcomingItem) string { return it.WorkID })
	held, err := r.cat.UpcomingOwnedWorks(ctx, nil, true, ids)
	if err != nil {
		return err
	}
	var news []Release
	done := map[string]bool{}
	for _, it := range items {
		if done[it.WorkID] || already[it.WorkID] || held[it.WorkID] {
			continue
		}
		done[it.WorkID] = true
		rec := stored[it.SeriesID]
		rel := Release{WorkID: it.WorkID, Series: rec.Name, Position: it.Position}
		for _, m := range rec.Members {
			if m.WorkID == it.WorkID {
				rel.Title = m.Title
				break
			}
		}
		news = append(news, rel)
	}
	if len(news) > 0 && r.opt.Announce != nil {
		r.opt.Announce(ctx, news)
	}
	return r.cat.MarkUpcomingAnnounced(ctx, ids, stamp(now))
}

// record stores a full run's status.
func (r *Runner) record(ctx context.Context, at time.Time, runErr error) {
	st, err := r.cat.UpcomingRunState(ctx)
	if err != nil {
		return
	}
	st.LastRunAt = stamp(at)
	if runErr == nil {
		st.LastSuccessAt, st.LastError, st.Failures = stamp(at), "", 0
	} else {
		st.LastError, st.Failures = runErr.Error(), st.Failures+1
	}
	if rows, err := r.cat.UpcomingSeriesAll(ctx); err == nil {
		st.Series = 0
		for _, row := range rows {
			if row.SeriesID != "" {
				st.Series++
			}
		}
	}
	if n, err := r.cat.CountUpcomingItems(ctx); err == nil {
		st.Items = n
	}
	if err := r.cat.SaveUpcomingRunState(ctx, st); err != nil {
		r.log.Warn("upcoming: saving the run status failed", "err", err)
	}
}

// Status is the job's state for the console.
type Status struct {
	LastRunAt     string `json:"last_run_at,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	NextRunAt     string `json:"next_run_at,omitempty"`
	Series        int    `json:"series"`
	Items         int    `json:"items"`
}

// Status reads the job's last run.
func (r *Runner) Status(ctx context.Context) (Status, error) {
	st, err := r.cat.UpcomingRunState(ctx)
	if err != nil {
		return Status{}, err
	}
	return Status{LastRunAt: st.LastRunAt, LastSuccessAt: st.LastSuccessAt, LastError: st.LastError,
		NextRunAt: st.NextRunAt, Series: st.Series, Items: st.Items}, nil
}

// stamp is the catalog's fixed-width UTC millisecond time.
var stamp = catalog.FormatStamp

func parseStamp(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil && s != ""
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

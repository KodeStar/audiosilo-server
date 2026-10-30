// Package upcoming is upcoming releases (Track U1): a background job that reads
// the community metadata for the series the server holds and works out, per
// local series, the volumes it doesn't hold (preorders, recent releases,
// missing volumes), for GET /me/upcoming, and announces a new release to the
// admin (book_released). Everything it stores is derived (catalog/upcoming.go)
// and rebuilt by the next run.
//
// A run, once a day (and an hour after the start):
//  1. each owned book with an identifier (in a series or not: a copy filed under
//     none still holds its work) gets its community work id (the meta cache's
//     answer, else a lookup under the service's own budget), looked at again
//     monthly;
//  2. each local series (library, folded name) gets the community series it is,
//     from its books' works' series named like it (match.SeriesKey, as the
//     /meta rails place books; then give or take an article or a trailing
//     "Series"), refreshed monthly or when the series gains books or one of
//     them a new work;
//  3. metaserve's watch feed, 50 series a request, says which series have news;
//  4. series/{id} is read for a new series, one with news, or one older than a
//     week; a 404 marks it gone (dropped after a week);
//  5. each local series is classified (Classify) and its items replaced.
//
// A failure keeps what earlier runs stored (a series is replaced only by a
// read that worked), and a failed run is tried again in an hour, three times.
// Metadata off, or the job switched off, makes no call at all: a run in flight
// stops within a second (whileOn), as if cancelled.
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

// Label is how the bell and the calendar name a release: "Title (Series
// #position)", the series part as far as it is known, the work id for a work
// without a title.
func (r Release) Label() string {
	t := cmp.Or(r.Title, r.WorkID)
	switch {
	case r.Series != "" && r.Position != "":
		return t + " (" + r.Series + " #" + r.Position + ")"
	case r.Series != "":
		return t + " (" + r.Series + ")"
	}
	return t
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
	// it, or when the series gains books or a book of it a new work;
	// recheckAge: a book's work (or its lack of one) is looked up again after it
	// (a merge upstream moves a book to another work), at most maxRechecks a
	// full run (the rest wait for the next).
	resolveAge  = 30 * 24 * time.Hour
	recheckAge  = 30 * 24 * time.Hour
	maxRechecks = 500
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
	// announcedKeep is how long a work's announcement is remembered: longer
	// than any release window (365 days), so it is never announced twice.
	announcedKeep = 400 * 24 * time.Hour
)

// errSwitchedOff ends a run the job (or metadata) was switched off during. It
// is a cancellation: no status is recorded and no retry is due.
var errSwitchedOff = fmt.Errorf("upcoming releases switched off: %w", context.Canceled)

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
	// settle is how long kicks must be quiet before a kick's run; offCheck
	// how often a run looks at the switch (whileOn).
	settle   time.Duration
	offCheck time.Duration
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
		kick: make(chan struct{}, 1), wake: make(chan struct{}, 1), settle: settleKicks, offCheck: time.Second}
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
// worked is kept). A run the job is switched off before or during makes no
// further call and ends with errSwitchedOff (a cancellation: no status).
func (r *Runner) RunOnce(ctx context.Context, full bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.opt.On() {
		return errSwitchedOff
	}
	now := r.opt.Now()
	rctx, stop := r.whileOn(ctx)
	defer stop()
	err := r.run(rctx, full, now)
	if rctx.Err() != nil {
		return context.Cause(rctx) // cancelled, or switched off
	}
	if full {
		r.record(ctx, now, err)
	}
	if err != nil {
		r.log.Warn("upcoming releases: run failed", "err", err)
	}
	return err
}

// whileOn is ctx, cancelled with errSwitchedOff once the job is switched off
// (looked at every offCheck); stop releases it.
func (r *Runner) whileOn(ctx context.Context) (rctx context.Context, stop func()) {
	rctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(r.offCheck)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-rctx.Done():
				return
			case <-t.C:
				if !r.opt.On() {
					cancel(errSwitchedOff)
					return
				}
			}
		}
	}()
	return rctx, func() {
		close(done)
		cancel(nil)
	}
}

func (r *Runner) run(ctx context.Context, full bool, now time.Time) error {
	books, err := r.cat.UpcomingBooks(ctx)
	if err != nil {
		return fmt.Errorf("reading the books: %w", err)
	}
	works, changed, err := r.resolveWorks(ctx, books, full, now)
	if err != nil {
		return err
	}
	locals := groupSeries(books)
	failed, err := r.resolveSeries(ctx, locals, works, changed, now)
	if err != nil {
		return err
	}
	n, err := r.fetchSeries(ctx, full, now)
	if err != nil {
		return err
	}
	failed += n
	if err := r.classify(ctx, locals, works, now); err != nil {
		return err
	}
	if err := r.announce(ctx, now); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d community reads failed", failed)
	}
	return nil
}

// groupSeries groups the books in a series by local series, in library order.
func groupSeries(books []catalog.UpcomingBook) []*localSeries {
	var out []*localSeries
	by := map[seriesKey]*localSeries{}
	for _, b := range books {
		if b.SeriesKey == "" {
			continue // in no series: resolved, never a series of its own
		}
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
// known) and the books whose work this run found new or changed. A book whose
// identifiers have no answer yet (a new book, or its identifiers changed) takes
// the cache's work id, else a lookup; a full run looks again at answers a month
// old (a work merged upstream, a match the catalogue gained), up to maxRechecks,
// by lookup alone: the cache keeps an identifier's last answer however old, so
// it would only say the same again, and a cached "no match" is never taken for
// an answer (WorkIDs asks when its own cache has none). Lookups go a batch at a
// time; one that fails is asked again next run, the book keeping its old answer
// meanwhile. A book whose identifiers went, or that left the index, is
// forgotten, so it holds no work any more.
func (r *Runner) resolveWorks(ctx context.Context, books []catalog.UpcomingBook, full bool, now time.Time) (works map[catalog.Ref]string, changed map[catalog.Ref]bool, err error) {
	stored, err := r.cat.UpcomingOwnedAll(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the resolved books: %w", err)
	}
	works, changed = map[catalog.Ref]string{}, map[catalog.Ref]bool{}
	type want struct {
		ref catalog.Ref
		key string
		ids meta.BookIDs
	}
	var asks []want
	seen := map[catalog.Ref]bool{}
	keyed := map[catalog.Ref]bool{}
	var saved []catalog.UpcomingOwned
	rechecks := 0
	for _, b := range books {
		if seen[b.Ref] {
			continue
		}
		seen[b.Ref] = true
		key := meta.IDKey(b.ASIN, b.ISBN)
		if key == "" {
			continue
		}
		keyed[b.Ref] = true
		if o, ok := stored[b.Ref]; ok && o.IDKey == key {
			works[b.Ref] = o.WorkID
			checked, _ := parseStamp(o.CheckedAt)
			if !full || now.Sub(checked) < recheckAge || rechecks >= maxRechecks {
				continue
			}
			rechecks++
		} else if id, ok := r.meta.CachedWorkID(ctx, b.ASIN, b.ISBN); ok && id != "" {
			works[b.Ref] = id
			changed[b.Ref] = id != stored[b.Ref].WorkID
			saved = append(saved, catalog.UpcomingOwned{Ref: b.Ref, IDKey: key, WorkID: id, CheckedAt: stamp(now)})
			continue
		}
		asks = append(asks, want{b.Ref, key, meta.BookIDs{ASIN: b.ASIN, ISBN: b.ISBN}})
	}
	var drop []catalog.Ref
	for ref := range stored {
		if !keyed[ref] {
			drop = append(drop, ref)
		}
	}
	for chunk := range slices.Chunk(asks, workBatch) {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		ids := make([]meta.BookIDs, len(chunk))
		for i, a := range chunk {
			ids[i] = a.ids
		}
		for i, w := range r.meta.WorkIDs(ctx, ids) {
			if w.Failed {
				continue
			}
			a := chunk[i]
			works[a.ref] = w.ID
			changed[a.ref] = w.ID != "" && w.ID != stored[a.ref].WorkID
			saved = append(saved, catalog.UpcomingOwned{Ref: a.ref, IDKey: a.key, WorkID: w.ID, CheckedAt: stamp(now)})
		}
	}
	if err := r.cat.SaveUpcomingOwned(ctx, saved, drop); err != nil {
		return nil, nil, fmt.Errorf("saving the resolved books: %w", err)
	}
	return works, changed, nil
}

// resolveSeries finds the community series of each local series that is due
// (never resolved, a month old, holding more books than when it was, or with a
// book whose work changed), and forgets those the library no longer holds. It
// returns how many failed.
func (r *Runner) resolveSeries(ctx context.Context, locals []*localSeries, works map[catalog.Ref]string, changed map[catalog.Ref]bool, now time.Time) (int, error) {
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
			if now.Sub(resolved) < resolveAge && len(l.books) <= prior[0].Books &&
				!slices.ContainsFunc(l.books, func(b catalog.UpcomingBook) bool { return changed[b.Ref] }) {
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
		found, ok := r.seriesOf(ctx, l.name, ids)
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

// seriesOf is the community series a local series (by its spelling) is, read
// from the series of its works: those named like it, primaries first (a variant
// reading order only when no primary is); else those named like it give or take
// an article or a trailing "Series". ok is false when a work's read failed and
// none matched: what that work would have said is unknown, so the series keeps
// what it had until a later run.
func (r *Runner) seriesOf(ctx context.Context, name string, workIDs []string) (ids []string, ok bool) {
	var refs []meta.SeriesRef
	failed := false
	for _, w := range workIDs {
		got, err := r.meta.WorkSeries(ctx, w)
		switch {
		case err == nil:
			refs = append(refs, got...)
		case errors.Is(err, meta.ErrNotFound):
		default:
			failed = true
			if ctx.Err() == nil {
				r.log.Debug("upcoming: reading a work's series failed", "work", w, "err", err)
			}
		}
	}
	for _, fold := range []func(string) string{
		func(n string) string { return cmp.Or(match.SeriesKey(n), n) },
		func(n string) string { return match.SeriesKey(match.NormalizeSeries(n)) },
	} {
		want := fold(name)
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
			return primaries, true
		}
		if len(variants) > 0 {
			return variants, true
		}
	}
	return nil, !failed
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
// holds with another date (an undated item: with a date at all); a work no
// stored series of the batch holds marks the series it is named after (the
// item's title starts with the series' name), else every series of the batch.
// A batch the feed can't answer, or one that fills the feed (its oldest items
// may be cut), is split, down to a single series, which then counts as news.
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
				// An undated item is dated by its addition to the catalogue, so
				// only a date the stored member has is news there.
				if (it.Undated && datePrefix(m.ReleaseDate) != "") ||
					(!it.Undated && it.Date != "" && datePrefix(m.ReleaseDate) != it.Date) {
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

// classify works out every local series' items from what is stored. A
// released item is news only when a community series the local series was
// classified against before lists its work; any other is the baseline of a
// series read for the first time (out before the server watched it): marked,
// never announced. The series read are marked classified.
func (r *Runner) classify(ctx context.Context, locals []*localSeries, works map[catalog.Ref]string, now time.Time) error {
	rows, err := r.cat.UpcomingSeriesAll(ctx)
	if err != nil {
		return err
	}
	stored, err := r.cat.MetaSeriesAll(ctx)
	if err != nil {
		return err
	}
	by := byLocal(rows)
	window := r.opt.Window()
	for _, l := range locals {
		var series []Series
		var read []string
		watched := map[string]bool{}
		for _, row := range by[seriesKey{l.libraryID, l.key}] {
			rec, ok := stored[row.SeriesID]
			if !ok || len(rec.Members) == 0 {
				continue
			}
			series = append(series, Series{ID: row.SeriesID, Members: rec.Members})
			read = append(read, row.SeriesID)
			if row.Classified {
				for _, m := range rec.Members {
					watched[m.WorkID] = true
				}
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
		baseline := map[string]bool{}
		for _, it := range items {
			if !watched[it.WorkID] {
				baseline[it.WorkID] = true
			}
		}
		if err := r.cat.ReplaceUpcomingItems(ctx, l.libraryID, l.key, items, baseline, read, stamp(now)); err != nil {
			return fmt.Errorf("saving a series' items: %w", err)
		}
	}
	return nil
}

// announce tells the admin about the releases no earlier run has: one call for
// all of them, each work once. A work the server holds a copy of somewhere, or
// one announced before (through another series, or before its item left and
// came back), is marked without a word.
func (r *Runner) announce(ctx context.Context, now time.Time) error {
	items, already, err := r.cat.UnannouncedReleases(ctx)
	if err != nil || len(items) == 0 {
		return err
	}
	ids := distinct(items, func(it catalog.UpcomingEntry) string { return it.WorkID })
	held, err := r.cat.UpcomingOwnedWorks(ctx, nil, true, ids)
	if err != nil {
		return err
	}
	var news []Release
	var announced []string
	done := map[string]bool{}
	for _, it := range items {
		if done[it.WorkID] || already[it.WorkID] || held[it.WorkID] {
			continue
		}
		done[it.WorkID] = true
		news = append(news, Release{WorkID: it.WorkID, Title: it.Title, Series: it.SeriesName, Position: it.Position})
		announced = append(announced, it.WorkID)
	}
	if len(news) > 0 && r.opt.Announce != nil {
		r.opt.Announce(ctx, news)
	}
	// Once announced, the mark lands even when the run is being stopped (a
	// switch-off or shutdown), or the next run would announce them again.
	return r.cat.MarkUpcomingAnnounced(context.WithoutCancel(ctx), ids, announced, stamp(now), stamp(now.Add(-announcedKeep)))
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

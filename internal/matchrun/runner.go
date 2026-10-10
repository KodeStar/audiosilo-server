package matchrun

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/pool"
)

// Bounds on what a run asks of the community service, which is shared: a few
// books at a time (the match dialog asks for one), only the best two works
// expanded per book (the second only tells how far ahead the first is).
const (
	workers        = 2
	bulkCandidates = 2
	// maxFailStreak consecutive books the service failed for stop the run: it is
	// down, and the rest would only record the same error.
	maxFailStreak = 5
)

// Codes a run that stopped by itself records (catalog.MatchRun.Error).
const (
	ErrCodeMetadataOff = "metadata_off"
	ErrCodeUnavailable = "metadata_unavailable"
	ErrCodeInternal    = "internal"
)

// DetailIdentifierUnknown is a refresh item's detail when the community doesn't
// know the book's ASIN or ISBN.
const DetailIdentifierUnknown = "identifier_unknown"

// ErrBusy is a start or apply asked while a run is working: one at a time.
var ErrBusy = errors.New("a match run is already working")

// ErrScope is an apply naming a scope its run's mode doesn't take (ScopesFor).
var ErrScope = errors.New("the scope doesn't apply to this match run")

// Matcher finds the community works a book might be (meta.Service.Candidates).
type Matcher interface {
	Candidates(ctx context.Context, q meta.MatchQuery) ([]meta.MatchCandidate, error)
}

// CoverSaver keeps a community cover image as a book's custom cover, as the match
// dialog's cover tick does (the api's fetch, check and SetCover).
type CoverSaver func(ctx context.Context, libraryID int64, path, url string, userID int64) error

// Runner runs match runs, one at a time, in the background.
type Runner struct {
	cat       *catalog.Catalog
	matcher   Matcher
	saveCover CoverSaver
	// enabled is the live metadata switch: a run stops once it is off.
	enabled func() bool
	log     *slog.Logger

	mu        sync.Mutex
	active    int64 // the working run's id, 0 when none
	cancel    context.CancelFunc
	cancelled bool // the admin stopped it (not the server shutting down)
	wg        sync.WaitGroup
}

// New returns a Runner. matcher is nil with no community service configured:
// then only Clear and the reads may be used (enabled must say off, as the api's
// metadataOn does, so nothing starts or applies a run).
func New(cat *catalog.Catalog, matcher Matcher, saveCover CoverSaver, enabled func() bool, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{cat: cat, matcher: matcher, saveCover: saveCover, enabled: enabled, log: log}
}

// StartOptions is what a run matches: the books of one library (0 = every
// library), in mode (catalog.MatchModeMatch, MatchModeRepick or
// MatchModeRefresh), preferring region's ASINs, for the admin userID.
type StartOptions struct {
	LibraryID int64
	Mode      string
	Region    string
	UserID    int64
}

// Start records a run and starts matching in the background, under base (the
// server's lifetime), returning the run as it starts. ErrBusy while another run
// is working.
func (r *Runner) Start(ctx, base context.Context, o StartOptions) (*catalog.MatchRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 {
		return nil, ErrBusy
	}
	var (
		books []catalog.Book
		err   error
	)
	switch o.Mode {
	case catalog.MatchModeRepick:
		books, err = r.cat.CommunityASINBooks(ctx, o.LibraryID)
	case catalog.MatchModeRefresh:
		books, err = r.cat.MatchedBooks(ctx, o.LibraryID)
	default:
		books, err = r.cat.UnmatchedBooks(ctx, o.LibraryID)
	}
	if err != nil {
		return nil, err
	}
	run := catalog.MatchRun{Mode: o.Mode, Region: o.Region, Total: len(books)}
	if o.LibraryID != 0 {
		run.LibraryID = &o.LibraryID
	}
	if o.UserID > 0 {
		run.StartedBy = &o.UserID
	}
	id, err := r.cat.StartMatchRun(ctx, run)
	if err != nil {
		return nil, err
	}
	work := r.begin(base, id)
	r.wg.Go(func() {
		defer r.end()
		r.match(work, id, o, books)
	})
	return r.cat.GetMatchRun(ctx, id)
}

// begin marks run id working (r.mu held) and returns the context it works under.
func (r *Runner) begin(base context.Context, id int64) context.Context {
	ctx, cancel := context.WithCancel(base)
	r.active, r.cancel, r.cancelled = id, cancel, false
	return ctx
}

func (r *Runner) end() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancel()
	r.active, r.cancel, r.cancelled = 0, nil, false
}

// stoppedByAdmin reports whether the working run's context ended because the
// admin cancelled it, rather than because the server is shutting down (then the
// run is left as it is, and settled at the next start: InterruptMatchRuns).
func (r *Runner) stoppedByAdmin() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

// Cancel stops run id if it is the one working: matching stops (the run is
// cancelled), applying stops after the books in hand (the run is ready again, the
// rest still to apply). false when it isn't working.
func (r *Runner) Cancel(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == 0 || r.active != id {
		return false
	}
	r.cancelled = true
	r.cancel()
	return true
}

// Clear undoes the community matches of one library (0 = every library) and
// drops the runs (catalog.ClearCommunityMatches), holding off any run meanwhile.
// ErrBusy while a run is working: it would write matches back, or apply a review
// the clear removes.
func (r *Runner) Clear(ctx context.Context, libraryID int64) (*catalog.ClearedMatches, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 {
		return nil, ErrBusy
	}
	return r.cat.ClearCommunityMatches(ctx, libraryID)
}

// Wait blocks until no run is working (tests, shutdown).
func (r *Runner) Wait() { r.wg.Wait() }

// match matches each book, a few at a time, recording an item per book, then
// settles the run.
func (r *Runner) match(ctx context.Context, runID int64, o StartOptions, books []catalog.Book) {
	work, halt := context.WithCancel(ctx)
	defer halt()
	var (
		once     sync.Once
		stopCode string
		stopErr  error
		streak   atomic.Int32
	)
	// stop ends the run by itself, the first reason given being the one recorded.
	stop := func(code string, err error) {
		once.Do(func() { stopCode, stopErr = code, err })
		halt()
	}
	enabled := func() bool {
		if r.enabled() {
			return true
		}
		stop(ErrCodeMetadataOff, nil)
		return false
	}
	pool.Each(work, workers, books, enabled, func(b *catalog.Book) {
		item, err := r.matchBook(work, o, b)
		switch {
		case work.Err() != nil:
			return
		case err == nil:
			streak.Store(0)
		case streak.Add(1) >= maxFailStreak:
			stop(ErrCodeUnavailable, err)
			return
		}
		if err := r.cat.RecordMatchItem(work, runID, item); err != nil && work.Err() == nil {
			stop(ErrCodeInternal, err)
		}
	})

	status := catalog.MatchReady
	switch {
	case stopCode != "": // each has returned: every worker is done with it
		status = catalog.MatchFailed
		r.log.Warn("match run stopped", "run", runID, "code", stopCode, "err", stopErr)
	case ctx.Err() != nil && !r.stoppedByAdmin():
		return // the server is stopping: the next start settles it
	case ctx.Err() != nil:
		status = catalog.MatchCancelled
	}
	if err := r.cat.FinishMatching(context.WithoutCancel(ctx), runID, status, stopCode); err != nil {
		r.log.Error("finish match run failed", "run", runID, "err", err)
	}
}

// matchBook matches one book: its item (nil = nothing worth recording), and the
// community service's failure, if that's what the item records.
func (r *Runner) matchBook(ctx context.Context, o StartOptions, b *catalog.Book) (*catalog.MatchRunItem, error) {
	item := &catalog.MatchRunItem{LibraryID: b.LibraryID, Path: b.RelPath, Proposal: catalog.MatchProposal{Values: map[string]string{}}}
	switch o.Mode {
	case catalog.MatchModeRepick:
		return r.repickBook(ctx, o.Region, b, item)
	case catalog.MatchModeRefresh:
		return r.refreshBook(ctx, o.Region, b, item)
	}
	q := QueryFor(b, o.Region)
	q.Limit = bulkCandidates
	cands, err := r.candidates(ctx, q, item)
	if err != nil {
		return item, err
	}
	if len(cands) > 0 {
		item.Proposal = Propose(&cands[0], meta.DefaultRecording(&cands[0], b.Duration, o.Region))
	}
	item.Outcome, item.Score, item.RunnerUp = Classify(cands, item.Proposal)
	return item, nil
}

// candidates asks the community service for q's candidates, marking item as the
// service's failure when it fails.
func (r *Runner) candidates(ctx context.Context, q meta.MatchQuery, item *catalog.MatchRunItem) ([]meta.MatchCandidate, error) {
	cands, err := r.matcher.Candidates(ctx, q)
	if err != nil {
		item.Outcome, item.Detail = catalog.OutcomeError, ErrCodeUnavailable
	}
	return cands, err
}

// repickBook looks the book's ASIN up: when the recording it names sells in the
// preferred marketplace under another ASIN, that is the item (confident: it is
// the same recording); else there is nothing to record.
func (r *Runner) repickBook(ctx context.Context, region string, b *catalog.Book, item *catalog.MatchRunItem) (*catalog.MatchRunItem, error) {
	cands, err := r.candidates(ctx, meta.MatchQuery{ASIN: b.ASIN, Region: region, Limit: 1}, item)
	if err != nil {
		return item, err
	}
	if region == "" || len(cands) == 0 || cands[0].RecordingID == "" {
		return nil, nil
	}
	c := &cands[0]
	rec := meta.DefaultRecording(c, b.Duration, region)
	if rec == nil || rec.ID != c.RecordingID {
		return nil, nil
	}
	// Nothing when it sells there under none, or the book already has one of the
	// ASINs it sells there under (a recording can be listed twice in one store).
	want := rec.RegionASIN(region)
	if want == "" || slices.Contains(rec.ASINRefs, meta.ASINRef{Region: region, ASIN: b.ASIN}) {
		return nil, nil
	}
	item.Outcome, item.Score = catalog.OutcomeAuto, c.Score
	item.Proposal = Propose(c, rec)
	// The ASIN is all a repick changes.
	item.Proposal.Values, item.Proposal.ASINRegion = map[string]string{catalog.FieldASIN: want}, region
	return item, nil
}

// refreshBook looks the book up by its own ASIN or ISBN alone: the record it
// names is the book (confident, whatever the score), its recording the one the
// identifier resolved to. The lookup takes the ASIN when there is one, so an ISBN
// beside an ASIN the community doesn't know gets a look of its own. An identifier
// the community doesn't know is no match: a book that names its record is never
// offered another by title.
func (r *Runner) refreshBook(ctx context.Context, region string, b *catalog.Book, item *catalog.MatchRunItem) (*catalog.MatchRunItem, error) {
	q := meta.MatchQuery{ASIN: b.ASIN, ISBN: b.ISBN, Duration: b.Duration, Region: region, Limit: 1}
	cands, err := r.candidates(ctx, q, item)
	if err == nil && len(cands) == 0 && q.ASIN != "" && q.ISBN != "" {
		q.ASIN = ""
		cands, err = r.candidates(ctx, q, item)
	}
	if err != nil {
		return item, err
	}
	if len(cands) == 0 {
		item.Outcome, item.Detail = catalog.OutcomeNone, DetailIdentifierUnknown
		return item, nil
	}
	c := &cands[0]
	item.Outcome, item.Score = catalog.OutcomeAuto, c.Score
	item.Proposal = Propose(c, meta.DefaultRecording(c, b.Duration, region))
	// The book keeps its own identifiers (planFor), so the review mustn't show the
	// community's (another marketplace's ASIN, say) as if applying set them.
	delete(item.Proposal.Values, catalog.FieldASIN)
	delete(item.Proposal.Values, catalog.FieldISBN)
	item.Proposal.ASINRegion = ""
	return item, nil
}

// ApplyOptions is what an apply writes: the run's confident items less Exclude,
// plus the Include items (a "review" item the admin chose), under Scope (one of
// ScopesFor the run's mode; a repick run's scope is its ASIN, whatever this says),
// as UserID.
type ApplyOptions struct {
	Scope   string
	Include []int64
	Exclude []int64
	UserID  int64
}

// Apply starts applying a ready run in the background, under base, returning the
// run as it starts. ErrBusy while a run is working; catalog.ErrRunNotReady when
// this one isn't ready; ErrScope for a scope its mode doesn't take.
func (r *Runner) Apply(ctx, base context.Context, runID int64, o ApplyOptions) (*catalog.MatchRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 {
		return nil, ErrBusy
	}
	run, err := r.cat.GetMatchRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != catalog.MatchReady {
		return nil, catalog.ErrRunNotReady
	}
	if scopes := ScopesFor(run.Mode); scopes != nil && !slices.Contains(scopes, o.Scope) {
		return nil, ErrScope
	}
	items, err := r.cat.MatchItemsToApply(ctx, runID, o.Include, o.Exclude)
	if err != nil {
		return nil, err
	}
	if err := r.cat.BeginApply(ctx, runID, o.Scope, len(items)); err != nil {
		return nil, err
	}
	work := r.begin(base, runID)
	r.wg.Go(func() {
		defer r.end()
		r.apply(work, run, items, o)
	})
	return r.cat.GetMatchRun(ctx, runID)
}

// apply applies each item, a few at a time, then settles the run. Turning
// community metadata off stops it as a cancel does (taking a community cover is
// an outbound fetch, and every match run endpoint, Stop included, answers
// metadata_off then): back to ready, the rest still to apply.
func (r *Runner) apply(ctx context.Context, run *catalog.MatchRun, items []catalog.MatchRunItem, o ApplyOptions) {
	off := false // only the feeding goroutine (this one) reads and writes it
	enabled := func() bool {
		off = off || !r.enabled()
		return !off
	}
	pool.Each(ctx, workers, items, enabled, func(it *catalog.MatchRunItem) {
		applied, detail := r.applyItem(ctx, run.Mode, it, o)
		// Stopped mid-item: what the stop may have cut short (a failure, a cover
		// not taken) is not marked, so the next apply takes it again; what went in
		// is, so it counts as applied rather than coming back as nothing to change.
		if ctx.Err() != nil && (applied == catalog.ItemFailed || detail == "cover_failed") {
			return
		}
		if err := r.cat.MarkMatchItem(context.WithoutCancel(ctx), run.ID, it.ID, applied, detail); err != nil {
			r.log.Error("mark match item failed", "run", run.ID, "item", it.ID, "err", err)
		}
	})
	if ctx.Err() != nil && !r.stoppedByAdmin() {
		return // the server is stopping: the next start puts it back to ready
	}
	if off {
		r.log.Info("match apply stopped: community metadata is off", "run", run.ID)
	}
	if err := r.cat.FinishApply(context.WithoutCancel(ctx), run.ID, ctx.Err() != nil || off); err != nil {
		r.log.Error("finish match apply failed", "run", run.ID, "err", err)
	}
}

// applyItem writes one item's plan to its book: applied, skipped (nothing to
// change, or no book there now) or failed, with a code saying why.
func (r *Runner) applyItem(ctx context.Context, mode string, it *catalog.MatchRunItem, o ApplyOptions) (applied, detail string) {
	st, err := r.cat.BookMatchState(ctx, it.LibraryID, it.Path)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		return catalog.ItemSkipped, "book_gone"
	case err != nil:
		if ctx.Err() == nil {
			r.log.Warn("load book for match apply failed", "library", it.LibraryID, "path", it.Path, "err", err)
		}
		return catalog.ItemFailed, "edit_failed"
	}
	set, cover := planFor(mode, o.Scope, st, it.Proposal)
	if len(set) == 0 && !cover {
		return catalog.ItemSkipped, "nothing_to_change"
	}
	if len(set) > 0 {
		edit := catalog.BookEdit{Set: set, Source: catalog.SourceCommunity, UserID: o.UserID}
		if err := r.cat.EditBook(ctx, it.LibraryID, it.Path, edit); err != nil {
			if errors.Is(err, catalog.ErrNotFound) {
				return catalog.ItemSkipped, "book_gone"
			}
			if ctx.Err() == nil {
				r.log.Warn("match apply edit failed", "library", it.LibraryID, "path", it.Path, "err", err)
			}
			return catalog.ItemFailed, "edit_failed"
		}
	}
	if cover {
		if err := r.saveCover(ctx, it.LibraryID, it.Path, it.Proposal.CoverURL, o.UserID); err != nil {
			if ctx.Err() == nil {
				r.log.Info("match apply cover failed", "library", it.LibraryID, "path", it.Path, "err", err)
			}
			if len(set) == 0 {
				return catalog.ItemFailed, "cover_failed"
			}
			return catalog.ItemApplied, "cover_failed"
		}
	}
	return catalog.ItemApplied, ""
}

// Change is what applying an item under one scope would write: its fields (in
// catalog.OverrideFields order) and whether it takes the community cover.
type Change struct {
	Fields []string `json:"fields"`
	Cover  bool     `json:"cover"`
}

// ItemView is an item as the review shows it: the book as it is now (Gone when
// nothing is indexed at its path any more) and what each scope would change.
type ItemView struct {
	catalog.MatchRunItem
	Book    ItemBook          `json:"book"`
	Gone    bool              `json:"gone,omitempty"`
	Changes map[string]Change `json:"changes"`
}

// ItemBook is the book an item is for, as it is now.
type ItemBook struct {
	Title  string `json:"title"`
	Author string `json:"author"`
}

// itemScopes is the scopes the review shows a run in mode's changes under: its
// own (ScopesFor), and for a repick, which ignores the scope, ids alone.
func itemScopes(mode string) []string {
	if scopes := ScopesFor(mode); scopes != nil {
		return scopes
	}
	return []string{ScopeIDs}
}

// Items is a page of run's items (outcome "" = all) as the review shows them,
// and the id the next page reads after (0 = the last page).
func (r *Runner) Items(ctx context.Context, run *catalog.MatchRun, outcome string, after int64, limit int) ([]ItemView, int64, error) {
	items, next, err := r.cat.ListMatchRunItems(ctx, run.ID, outcome, after, limit)
	if err != nil {
		return nil, 0, err
	}
	out := make([]ItemView, len(items))
	scopes := itemScopes(run.Mode)
	for i, it := range items {
		v := ItemView{MatchRunItem: it, Changes: map[string]Change{}}
		st, err := r.cat.BookMatchState(ctx, it.LibraryID, it.Path)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			v.Gone = true
		case err != nil:
			return nil, 0, err
		default:
			v.Book = ItemBook{Title: st.Title, Author: st.Author}
			for _, scope := range scopes {
				set, cover := planFor(run.Mode, scope, st, it.Proposal)
				ch := Change{Fields: []string{}, Cover: cover}
				for _, f := range catalog.OverrideFields {
					if _, ok := set[f]; ok {
						ch.Fields = append(ch.Fields, f)
					}
				}
				v.Changes[scope] = ch
			}
		}
		out[i] = v
	}
	return out, next, nil
}

package matchrun

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/names"
	"github.com/kodestar/audiosilo-server/internal/pool"
)

// Bounds on what a run asks of the community service, which is shared. Nearly all
// of a run's time online is round trips (measured against meta.audiosilo.app: an
// 81 ms round trip, a 20 ms median match, an idle host), so it asks for few of them:
// the best two works per book, the second only telling how far ahead the first is, and
// only the first with its details (MatchQuery.BestOnly). maxRequestsPerSecond, not the
// worker count, is what holds a run under the service's per-client limit (100 a second,
// audiosilo-meta's DefaultRateLimit); the workers only fill the round trips. Mirror
// mode shares the workers, where the copy's own cap on its matches (half the cores,
// internal/metamirror) bounds the cores a run takes, and no pacing (the local copy
// answers before the Pacer's transport).
const (
	workers              = 6
	maxRequestsPerSecond = 60
	bulkCandidates       = 2
	// applyWorkers is how many books an apply writes at a time: its waits are the
	// database's and each cover's fetch, not the match's.
	applyWorkers = 2
	// maxFailStreak consecutive books the service failed for stop the run: it is
	// down, and the rest would only record the same error. It stands above the
	// workers, as one moment's failure meets them all at once (a lost connection takes
	// every stream on it, a stall every request's timeout): some of the books it counts
	// were asked after that moment, and failed too.
	maxFailStreak = workers + 4
)

// Codes a run that stopped by itself records (catalog.MatchRun.Error). A run the
// service's outage stops (metadata_unavailable) is ready, its answers kept to
// review and apply; the others are failed.
const (
	ErrCodeMetadataOff = "metadata_off"
	ErrCodeUnavailable = "metadata_unavailable"
	ErrCodeInternal    = "internal"
)

// DetailIdentifierUnknown is a refresh item's detail when the community doesn't
// know any of the book's identifiers and it has no recorded work to fall back to,
// or the recorded work it fell back to has nothing to change (server#342).
const DetailIdentifierUnknown = "identifier_unknown"

// DetailWorkUnknown is a refresh item's detail when the community no longer knows
// the work a book's match recorded (a direct lookup or an identifier fallback,
// a 404 after any redirect): no match, and the book keeps its recorded match (a release that
// dropped the work by mistake must not unmatch the book).
const DetailWorkUnknown = "work_unknown"

// DetailLengthDiffers is a review item's detail when its recording is another
// edition by the book's length (lengthFits: more than meta.PlausibleRuntimeOff off,
// or meta.NarratedRuntimeOff for the book's narrator's recording): held for review
// for that, however confident the match (#134). The review words it with both
// lengths (ItemBook.Duration, the proposal's runtime_min).
const DetailLengthDiffers = "length_differs"

// DetailDramatizationOnly is a review item's detail when the work fits the book but
// holds only a dramatization, which the book doesn't state it is: meta.DefaultRecording
// offers no recording, so there is no identifier to attach unasked.
const DetailDramatizationOnly = "dramatization_only"

// DetailIdentifierDisagrees is a review item's detail when its best candidate was
// found by the book's hint ASIN or ISBN (an Audiobookshelf import's or its tags':
// QueryFor) but the book's own title or series position doesn't agree with it
// (identifierAgrees): the hint most likely names another book, so it is held for
// review however high it scored (server#306).
const DetailIdentifierDisagrees = "identifier_disagrees"

// DetailIdentifierElsewhere is a review item's detail when the book's hint ASIN or
// ISBN found a work the book doesn't agree with (as DetailIdentifierDisagrees),
// so the run asked again by the book's own facts alone and found another work:
// that one is proposed, held for review however confident, as the book still
// carries an identifier naming something else (server#352).
const DetailIdentifierElsewhere = "identifier_elsewhere"

// DetailWeakMatch is a match item's detail when its best candidate scored
// below ReviewFloor: recorded as no match (the community most likely doesn't
// have the book), its score kept for the review's wording.
const DetailWeakMatch = "weak_match"

// DetailCoverPending is a match item's detail while its fields went in but a stop
// cut its cover short: still to apply, so the next apply takes the cover, the
// ASIN or ISBN and recorded match the book has now being the run's own (not
// matchedSince, unless the book was matched again by hand since; its match is
// not recorded again: planFor).
const DetailCoverPending = "cover_pending"

// DetailCoverSuperseded is an item's detail when its cover was not written because
// the book changed while the apply fetched it (catalog.ErrCoverSuperseded: a
// custom cover uploaded, another match accepted, an Unlink): applied when its
// fields went in, else skipped. Final, never cover_pending: the book is no longer
// the one the cover was planned for.
const DetailCoverSuperseded = "cover_superseded"

// DetailASINKnown is the detail of an unknown_asin run's item an apply skipped
// because the community knows the book's ASIN now (asinKnownSince): the book is
// matched by that ASIN, and the run's match by its title is not written.
const DetailASINKnown = "asin_known"

// ErrBusy is a start or apply asked while a run is working: one at a time. It is
// catalog.ErrMatchRunBusy, the Unlink refusal, so one errors.Is matches both.
var ErrBusy = catalog.ErrMatchRunBusy

// ErrScope is an apply naming a scope its run's mode doesn't take (ScopesFor).
var ErrScope = errors.New("the scope doesn't apply to this match run")

// Matcher finds the community works a book might be (meta.Service.Candidates).
type Matcher interface {
	Candidates(ctx context.Context, q meta.MatchQuery) ([]meta.MatchCandidate, error)
}

// CoverSaver keeps a community cover image as a book's custom cover, as the match
// dialog's cover tick does (the api's fetch and check), only while the book is
// still as expect says (catalog.SetCoverIf): catalog.ErrCoverSuperseded, writing
// nothing, when it changed since the apply planned the cover.
type CoverSaver func(ctx context.Context, libraryID int64, path, url string, userID int64, expect catalog.CoverExpect) error

// Runner runs match runs, one at a time, in the background.
type Runner struct {
	cat       *catalog.Catalog
	matcher   Matcher
	saveCover CoverSaver
	// enabled is the live metadata switch: a run stops once it is off.
	enabled func() bool
	log     *slog.Logger
	// beforeEdit, when set (tests only), runs between an apply reading a book
	// and writing its edit: where a hand edit can land meanwhile.
	beforeEdit func(it *catalog.MatchRunItem)

	mu        sync.Mutex
	active    int64 // the working run's id, 0 when none
	cancel    context.CancelFunc
	cancelled bool        // the admin stopped it (not the server shutting down)
	pacer     *meta.Pacer // the active run's, while it matches (WaitingUntil)
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
// library), in mode (catalog.MatchModeMatch, MatchModeRepick, MatchModeRefresh
// or MatchModeUnknownASIN), preferring region's ASINs, for the admin userID.
type StartOptions struct {
	LibraryID int64
	Mode      string
	Region    string
	UserID    int64
}

// Start records a run and starts matching in the background, under base (the
// server's lifetime), returning the run as it starts. ErrBusy while another run
// is working. A run left marked working with nothing behind it is settled first
// (settleBeforeStart).
func (r *Runner) Start(ctx, base context.Context, o StartOptions) (*catalog.MatchRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 {
		return nil, ErrBusy
	}
	r.settleBeforeStart(ctx)
	var (
		books     []catalog.Book
		languages map[int64]string // a match's and an unknown_asin run's only (newMatches)
		err       error
	)
	switch o.Mode {
	case catalog.MatchModeRepick:
		books, err = r.cat.CommunityASINBooks(ctx, o.LibraryID)
	case catalog.MatchModeRefresh:
		books, err = r.cat.MatchedBooks(ctx, o.LibraryID)
	case catalog.MatchModeUnknownASIN:
		books, err = r.cat.UnknownASINBooks(ctx, o.LibraryID)
	default:
		books, err = r.cat.UnmatchedBooks(ctx, o.LibraryID)
	}
	if err == nil && newMatches(o.Mode) {
		languages, err = r.libraryLanguages(ctx, books)
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
		r.match(work, id, o, books, languages)
	})
	return r.cat.GetMatchRun(ctx, id)
}

// libraryLanguages is the language of each library books are in (LibraryLanguage, ""
// for none), read once a library for the run.
func (r *Runner) libraryLanguages(ctx context.Context, books []catalog.Book) (map[int64]string, error) {
	out := map[int64]string{}
	for i := range books {
		id := books[i].LibraryID
		if _, ok := out[id]; ok {
			continue
		}
		l, err := LibraryLanguageOf(ctx, r.cat, id, nil)
		if err != nil {
			return nil, err
		}
		out[id] = l
	}
	return out, nil
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
	r.active, r.cancel, r.cancelled, r.pacer = 0, nil, false, nil
}

// WaitingUntil is when run id goes on asking the community service, while it
// waits as the service asked (a 429 or 503's Retry-After), zero otherwise: the
// console's hint that the run's progress is held, not stuck (server#307).
func (r *Runner) WaitingUntil(id int64) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != id || r.pacer == nil {
		return time.Time{}
	}
	return r.pacer.WaitingUntil()
}

// settleLocked settles the runs still marked working that no worker is behind
// (catalog.SettleMatchRuns): a run whose finishing write and its own settle
// both failed (settleOwn; end clears r.active anyway) or one a stopped server
// left whose settle at startup failed. Such a row would refuse every
// Unlink of its library, and a stuck applying one could never be applied again.
// It returns how many it settled. r.mu held, and safe only because of that
// lock: Start and Apply hold it across StartMatchRun / BeginApply and begin, so
// no row this sees working is about to get workers, and end runs only after
// every worker of the active run, its cover writes included, has returned, so
// the active run (r.active, kept) is the only one with workers behind it.
func (r *Runner) settleLocked(ctx context.Context) (int, error) {
	n, err := r.cat.SettleMatchRuns(ctx, r.active)
	if n > 0 {
		r.log.Warn("match run: settled runs left marked working", "runs", n)
	}
	return n, err
}

// settleBeforeStart is settleLocked before a Start or an Apply: housekeeping,
// so a failure is logged and never refuses the run (server#350); the rows it
// left are tried again at the next start, Stop or Unlink. r.mu held.
func (r *Runner) settleBeforeStart(ctx context.Context) {
	if _, err := r.settleLocked(ctx); err != nil && ctx.Err() == nil {
		r.log.Warn("match run: settle runs left marked working failed", "err", err)
	}
}

// settleOwn settles run id once its finishing write (FinishMatching,
// FinishApply) failed, every worker of it done: left marked working it would
// refuse every Unlink of its library until the next start (server#350), with
// the Unlink's own settle (SettleOrphans) only a backstop. A matching run that
// recorded every book settles ready, its review kept (catalog.SettleMatchRuns).
func (r *Runner) settleOwn(ctx context.Context, id int64) {
	if _, err := r.cat.SettleMatchRun(context.WithoutCancel(ctx), id); err != nil {
		r.log.Error("settle match run failed", "run", id, "err", err)
	}
}

// SettleOrphans settles the runs left marked working with nothing behind them
// (settleLocked), keeping the active run, and returns how many it settled: an
// Unlink refused as busy asks it before trying once more.
func (r *Runner) SettleOrphans(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settleLocked(ctx)
}

// stoppedByAdmin reports whether the working run's context ended because the
// admin cancelled it, rather than because the server is shutting down (then the
// run is left as it is, and settled at the next start, Apply, Stop or Unlink:
// settleLocked).
func (r *Runner) stoppedByAdmin() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

// Stop results (Cancel).
type StopResult int

const (
	// NotWorking: the run is neither working nor marked working.
	NotWorking StopResult = iota
	// Stopping: the working run was asked to stop.
	Stopping
	// Settled: the run was marked working with nothing behind it (a finishing
	// write failed, or a stopped server left it) and is settled now: matching ->
	// interrupted (ready once every book was recorded), applying -> ready.
	Settled
)

// Cancel stops run id if it is the one working: matching stops (the run is
// cancelled), applying stops after the books in hand (the run is ready again, the
// rest still to apply). A run marked working that isn't the active one has
// nothing behind it, and is settled (catalog.SettleMatchRun) under the lock, so
// Stop always releases a stuck run. NotWorking when it is neither.
func (r *Runner) Cancel(ctx context.Context, id int64) (StopResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 && r.active == id {
		r.cancelled = true
		r.cancel()
		return Stopping, nil
	}
	settled, err := r.cat.SettleMatchRun(ctx, id)
	switch {
	case err != nil:
		return NotWorking, err
	case settled:
		r.log.Warn("match run: settled a run left marked working", "run", id)
		return Settled, nil
	}
	return NotWorking, nil
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
// settles the run. languages holds each library's language (libraryLanguages; a
// match run's and an unknown_asin run's only).
func (r *Runner) match(ctx context.Context, runID int64, o StartOptions, books []catalog.Book, languages map[int64]string) {
	pacer := meta.NewPacer(maxRequestsPerSecond, r.log.With("run", runID))
	r.mu.Lock()
	r.pacer = pacer
	r.mu.Unlock()
	work, halt := context.WithCancel(meta.WithPacer(ctx, pacer))
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
		item, err := r.matchBook(work, o, b, languages[b.LibraryID])
		if work.Err() != nil {
			return
		}
		failed := false
		if err == nil {
			streak.Store(0)
		} else {
			failed = streak.Add(1) >= maxFailStreak
		}
		// The book that trips the stop keeps its error item too, recorded before
		// the stop cancels work (server#307).
		if err := r.cat.RecordMatchItem(work, runID, item); err != nil && work.Err() == nil {
			stop(ErrCodeInternal, err)
		}
		if failed {
			stop(ErrCodeUnavailable, err)
		}
	})
	if waits, waited := pacer.Waits(); waits > 0 {
		r.log.Info("match run: the community service asked it to wait", "run", runID, "waits", waits, "waited", waited)
	}

	status := catalog.MatchReady
	switch {
	case stopCode == ErrCodeUnavailable: // each has returned: every worker is done with it
		// The service went away: what the run answered before stays to review and
		// apply (server#306), the books it never reached are still unmatched, and the
		// next run takes them. The code says it stopped early.
		r.log.Warn("match run stopped early, its answers kept", "run", runID, "code", stopCode, "err", stopErr)
	case stopCode != "":
		status = catalog.MatchFailed
		r.log.Warn("match run stopped", "run", runID, "code", stopCode, "err", stopErr)
	case ctx.Err() != nil && !r.stoppedByAdmin():
		return // the server is stopping: the next start settles it (InterruptMatchRuns, settleLocked)
	case ctx.Err() != nil:
		status = catalog.MatchCancelled
	}
	if err := r.cat.FinishMatching(context.WithoutCancel(ctx), runID, status, stopCode); err != nil {
		r.log.Error("finish match run failed", "run", runID, "err", err)
		r.settleOwn(ctx, runID)
	}
}

// matchBook matches one book of a library in language (LibraryLanguage, "" for none):
// its item (nil = nothing worth recording), and the community service's failure, if
// that's what the item records. The recording proposed is the match dialog's default
// (meta.ProposedRecording: a hint, an Audiobookshelf import's or a tag's, gives way to
// the recording the length agrees with; the book's own ASIN, an unknown_asin run's,
// keeps its recording).
func (r *Runner) matchBook(ctx context.Context, o StartOptions, b *catalog.Book, language string) (*catalog.MatchRunItem, error) {
	item := &catalog.MatchRunItem{LibraryID: b.LibraryID, Path: b.RelPath, Proposal: catalog.MatchProposal{Values: map[string]string{}}}
	switch o.Mode {
	case catalog.MatchModeRepick:
		return r.repickBook(ctx, o.Region, b, item)
	case catalog.MatchModeRefresh:
		return r.refreshBook(ctx, o.Region, b, item)
	case catalog.MatchModeUnknownASIN:
		// Found by the title, for a book with this ASIN: an apply writes only
		// while it still has it (matchedSince). Set last (the proposal is
		// replaced below) and on every item, a failed lookup's too, which the
		// review would otherwise read as matched since.
		defer func() { item.Proposal.FoundBy = identifierFoundBy(catalog.FieldASIN, b.ASIN) }()
	}
	q := QueryFor(b, o.Region, language)
	q.Limit, q.BestOnly, q.NoDetailsBelow = bulkCandidates, true, ReviewFloor
	cands, err := r.candidates(ctx, q, item)
	if err != nil {
		return item, err
	}
	m := judge(cands, q, b.Duration, o.Region)
	if o.Mode == catalog.MatchModeMatch && len(cands) > 0 && !identifierAgrees(&cands[0]) &&
		(q.BookASIN != "" || q.BookISBN != "") {
		// The hint names another book (a wrong volume's ASIN, server#352), and
		// metaserve answers one work per request, so the right one is lost
		// behind it: asked again by the book's own facts alone. What that finds,
		// when it is another work worth a look, is the item, held for review
		// however confident (the book still carries an identifier naming
		// something else); else, or when that ask fails, the hint's work
		// stays held. Not an unknown_asin run's book: its ASIN is its own, no
		// hint, and one the community knows now is never applied over
		// (asinKnownSince), so a second ask would be spent for nothing.
		bare := q
		bare.BookASIN, bare.BookISBN, bare.BookIdentifierHint = "", "", false
		again, err := r.matcher.Candidates(ctx, bare)
		if err != nil && ctx.Err() == nil {
			// Not the item's failure (the hint's work stands), so said here or
			// nowhere.
			r.log.Info("match run: asking again without the identifier failed", "library", b.LibraryID, "path", b.RelPath, "err", err)
		}
		if err == nil && len(again) > 0 && again[0].WorkID != cands[0].WorkID {
			if other := judge(again, bare, b.Duration, o.Region); other.outcome != catalog.OutcomeNone {
				m, cands = other, again
				m.outcome = catalog.OutcomeReview
				m.holds = slices.Insert(m.holds, 0, DetailIdentifierElsewhere)
			}
		}
	}
	item.Proposal, item.Outcome, item.Score, item.RunnerUp = m.proposal, m.outcome, m.score, m.runnerUp
	switch {
	case len(m.holds) > 0:
		// A review item: every reason it is held for, the first its detail
		// (server#352).
		item.Detail = m.holds[0]
		if len(m.holds) > 1 {
			item.Holds = m.holds
		}
	case item.Outcome == catalog.OutcomeNone && len(cands) > 0:
		// A weak fit is no proposal: the review offers nothing to apply.
		item.Detail, item.Proposal = DetailWeakMatch, catalog.MatchProposal{Values: map[string]string{}}
	}
	return item, nil
}

// judged is a match's verdict on its best candidate (judge): the proposal, the
// outcome with the best score and the next one's, and every reason a review
// item is held for (DetailIdentifierDisagrees, DetailDramatizationOnly,
// DetailLengthDiffers, in that order; none for one only short of the score or
// lead).
type judged struct {
	proposal        catalog.MatchProposal
	outcome         string
	score, runnerUp int
	holds           []string
}

// judge is matchBook's verdict on cands, the answer to q for a book of seconds
// in region's marketplace. The recording proposed is the match dialog's default
// (meta.ProposedRecording).
func judge(cands []meta.MatchCandidate, q meta.MatchQuery, seconds float64, region string) judged {
	j := judged{proposal: catalog.MatchProposal{Values: map[string]string{}}}
	noRecording, narrated := false, false
	if len(cands) > 0 {
		q.Duration, q.Region = seconds, region
		rec := meta.ProposedRecording(&cands[0], q)
		j.proposal = Propose(&cands[0], rec)
		noRecording = rec == nil && len(cands[0].Recordings) > 0
		narrated = narratorReads(&cands[0], j.proposal, q.Narrators)
	}
	j.outcome, j.score, j.runnerUp = Classify(cands, j.proposal, seconds, narrated)
	if j.outcome != catalog.OutcomeReview {
		return j
	}
	if !identifierAgrees(&cands[0]) {
		j.holds = append(j.holds, DetailIdentifierDisagrees)
	}
	if noRecording {
		j.holds = append(j.holds, DetailDramatizationOnly)
	}
	if !lengthFits(j.proposal, seconds, narrated) {
		j.holds = append(j.holds, DetailLengthDiffers)
		j.proposal.LengthTolerance = lengthTolerance(narrated)
	}
	return j
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
	cands, err := r.candidates(ctx, meta.MatchQuery{ASIN: b.ASIN, Region: region, Limit: 1, NoSeries: true}, item)
	if err != nil {
		return item, err
	}
	if region == "" || len(cands) == 0 || cands[0].RecordingID == "" {
		return nil, nil
	}
	c := &cands[0]
	rec := meta.DefaultRecording(c, b.Duration, region, nil)
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
	// The ASIN is all a repick changes, and only while the book still has the
	// one looked up here (PlanRepick: a hand match since keeps its own).
	item.Proposal.Values, item.Proposal.ASINRegion = map[string]string{catalog.FieldASIN: want}, region
	item.Proposal.FoundBy = identifierFoundBy(catalog.FieldASIN, b.ASIN)
	return item, nil
}

// refreshBook looks the book up by its own ASIN or ISBN first: the record it
// names is the book (confident, whatever the score, unless its recording is
// another edition by length: lengthFits), its recording the one the identifier
// resolved to. The lookup takes the ASIN when there is one, so an ISBN
// beside an ASIN the community doesn't know gets a look of its own. Identifiers
// the community doesn't know fall back to the book's recorded community match
// (catalog.MatchAnchor), when it has one and neither identifier is an admin edit;
// a book is never offered another record by title. A book matched with neither
// (a store listing) is looked up by that work directly. A work lookup
// (meta.MatchQuery.WorkID) follows a retired id's
// redirect, its recording the recorded one while the work still lists it; a work
// the community no longer knows is no match (DetailWorkUnknown).
func (r *Runner) refreshBook(ctx context.Context, region string, b *catalog.Book, item *catalog.MatchRunItem) (*catalog.MatchRunItem, error) {
	hasIdentifier := b.ASIN != "" || b.ISBN != ""
	if !hasIdentifier && b.MatchWorkID == "" {
		return nil, nil // matched by nothing a refresh can look up
	}
	st, err := r.cat.BookMatchState(ctx, b.LibraryID, b.RelPath)
	if err != nil {
		if !errors.Is(err, catalog.ErrNotFound) && ctx.Err() == nil {
			r.log.Warn("load book for match refresh failed", "library", b.LibraryID, "path", b.RelPath, "err", err)
		}
		return nil, nil // gone meanwhile, or unreadable: nothing to offer now
	}
	q := meta.MatchQuery{ASIN: b.ASIN, ISBN: b.ISBN, Duration: b.Duration, Region: region, Limit: 1}
	var cands []meta.MatchCandidate
	if hasIdentifier {
		cands, err = r.candidates(ctx, q, item)
		if err == nil && len(cands) == 0 && q.ASIN != "" && q.ISBN != "" {
			q.ASIN = ""
			cands, err = r.candidates(ctx, q, item)
		}
	}
	byWork := err == nil && len(cands) == 0 && b.MatchWorkID != "" && (!hasIdentifier || !hasEditedIdentifier(st))
	if byWork {
		q.ASIN, q.ISBN = "", ""
		q.WorkID, q.RecordingID = b.MatchWorkID, b.MatchRecordingID
		cands, err = r.candidates(ctx, q, item)
	}
	if err != nil {
		return item, err
	}
	if len(cands) == 0 {
		item.Outcome, item.Detail = catalog.OutcomeNone, DetailIdentifierUnknown
		if byWork {
			item.Detail = DetailWorkUnknown
		}
		return item, nil
	}
	c := &cands[0]
	item.Outcome, item.Score = catalog.OutcomeAuto, c.Score
	item.Proposal = Propose(c, meta.DefaultRecording(c, b.Duration, region, nil))
	// The record the book's own identifier names, but another edition by length
	// (an unabridged book holding its dramatization's ASIN): its details are not
	// written unasked. The lookup sends no narrator, so metaserve judges none: the
	// book's own credit naming the recording's reader widens the guard as in a match
	// run (readsRecording), so a book a run applied on its narrator's say-so stays
	// up to date.
	narrated := readsRecording(c, item.Proposal, names.Split(b.Narrator))
	if !lengthFits(item.Proposal, b.Duration, narrated) {
		item.Outcome, item.Detail = catalog.OutcomeReview, DetailLengthDiffers
		item.Proposal.LengthTolerance = lengthTolerance(narrated)
	}
	if byWork {
		// Found by the recorded match, the apply's guard (stillFoundBy). A book
		// with neither identifier takes the ASIN or ISBN its recording has (every
		// scope fills a missing identifier) unless it has one of its own by the
		// apply (planRefresh); a fallback keeps its own, and records them, as the
		// apply needs them as they were when their lookups missed (splitFallback).
		item.Proposal.FoundBy = workFoundBy(b.MatchWorkID, b.MatchRecordingID)
		if hasIdentifier {
			item.Proposal.FoundBy += ";" + fallbackIdentifiers(b.ASIN, b.ISBN)
		}
	} else {
		item.Proposal.FoundBy = identifierFoundBy(catalog.FieldISBN, q.ISBN)
		if q.ASIN != "" {
			item.Proposal.FoundBy = identifierFoundBy(catalog.FieldASIN, q.ASIN)
		}
	}
	if hasIdentifier {
		// The book keeps its own identifiers (switching marketplace is a repick's):
		// a proposal without them writes none, and the review shows none of the
		// community's (another marketplace's ASIN, say) as if applying set them.
		delete(item.Proposal.Values, catalog.FieldASIN)
		delete(item.Proposal.Values, catalog.FieldISBN)
		item.Proposal.ASINRegion = ""
	}
	// A book neither scope would change is up to date: nothing to record (most
	// of a matched library, whose proposals would only fill the review). The
	// scopes it has nothing to change under are recorded with it, as the review
	// shows them: an apply under one leaves it out unread (server#332).
	scopes := ScopesFor(catalog.MatchModeRefresh)
	for _, scope := range scopes {
		if planRefresh(scope, st, item.Proposal).Empty() {
			item.Unchanged = append(item.Unchanged, scope)
		}
	}
	if len(item.Unchanged) == len(scopes) {
		if byWork && hasIdentifier {
			// Up to date by its recorded match, but the community doesn't know
			// the book's own identifier (a mistyped ASIN, say): said, as it
			// would be with no match to fall back on, with nothing to apply
			// (server#342).
			item.Outcome, item.Detail, item.Score = catalog.OutcomeNone, DetailIdentifierUnknown, 0
			item.Proposal, item.Unchanged = catalog.MatchProposal{Values: map[string]string{}}, nil
			return item, nil
		}
		return nil, nil
	}
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

// CheckApply is why run can't be applied under scope, nil when it can:
// catalog.ErrRunNotReady unless it is ready, ErrScope for a scope its mode
// doesn't take. Apply asks it, and so does the review's count of what an apply
// would take (POST /admin/match-runs/{id}/preview).
func CheckApply(run *catalog.MatchRun, scope string) error {
	if run.Status != catalog.MatchReady {
		return catalog.ErrRunNotReady
	}
	if scopes := ScopesFor(run.Mode); scopes != nil && !slices.Contains(scopes, scope) {
		return ErrScope
	}
	return nil
}

// Apply starts applying a ready run in the background, under base, returning the
// run as it starts. ErrBusy while a run is working; catalog.ErrRunNotReady when
// this one isn't ready; ErrScope for a scope its mode doesn't take. A run left
// marked working with nothing behind it is settled first (settleBeforeStart), so
// a stuck applying run is ready, and can be applied, again.
func (r *Runner) Apply(ctx, base context.Context, runID int64, o ApplyOptions) (*catalog.MatchRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 {
		return nil, ErrBusy
	}
	r.settleBeforeStart(ctx)
	run, err := r.cat.GetMatchRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if err := CheckApply(run, o.Scope); err != nil {
		return nil, err
	}
	items, err := r.cat.MatchItemsToApply(ctx, runID, o.Scope, o.Include, o.Exclude)
	if err != nil {
		return nil, err
	}
	if err := r.cat.BeginApply(ctx, runID, o.Scope, len(items)); err != nil {
		return nil, err
	}
	// Read before the apply starts, so the run returned (and the audit's
	// "books") is every item it takes, never a racy part of it.
	started, err := r.cat.GetMatchRun(ctx, runID)
	work := r.begin(base, runID)
	r.wg.Go(func() {
		defer r.end()
		r.apply(work, run, items, o)
	})
	return started, err
}

// PreviewApply is the run as an Apply now would find it, with how many items
// it would take (catalog.CountMatchItemsToApply, the selection Apply reads),
// changing nothing else: a run left marked working with nothing behind it is
// settled first, as Apply settles it (settleBeforeStart), so a stuck applying run
// counts as the ready run Apply would apply. CheckApply's refusals as Apply's;
// catalog.ErrNotFound for a run that isn't there.
func (r *Runner) PreviewApply(ctx context.Context, runID int64, o ApplyOptions) (*catalog.MatchRun, int, error) {
	r.mu.Lock()
	r.settleBeforeStart(ctx)
	run, err := r.cat.GetMatchRun(ctx, runID)
	r.mu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	if err := CheckApply(run, o.Scope); err != nil {
		return run, 0, err
	}
	n, err := r.cat.CountMatchItemsToApply(ctx, runID, o.Scope, o.Include, o.Exclude)
	return run, n, err
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
	pool.Each(ctx, applyWorkers, items, enabled, func(it *catalog.MatchRunItem) {
		applied, detail := r.applyItem(ctx, run, it, o)
		// Stopped mid-item: what the stop may have cut short (a failure, a cover
		// not taken) stays to apply, so the next apply takes it again; what went in
		// is marked, so it counts as applied rather than coming back as nothing to
		// change.
		if ctx.Err() != nil && (applied == catalog.ItemFailed || detail == "cover_failed") {
			if applied == catalog.ItemFailed || !newMatches(run.Mode) {
				return
			}
			// A match's fields went in, its cover didn't: still to apply, marked
			// as the run's own, as the ASIN or match the book has now would
			// otherwise read as matched since and the cover would never be taken.
			applied, detail = "", DetailCoverPending
		}
		if err := r.cat.MarkMatchItem(context.WithoutCancel(ctx), run.ID, it.ID, applied, detail); err != nil {
			r.log.Error("mark match item failed", "run", run.ID, "item", it.ID, "err", err)
		}
	})
	if ctx.Err() != nil && !r.stoppedByAdmin() {
		return // the server is stopping: the next start puts it back to ready (InterruptMatchRuns, settleLocked)
	}
	if off {
		r.log.Info("match apply stopped: community metadata is off", "run", run.ID)
	}
	if err := r.cat.FinishApply(context.WithoutCancel(ctx), run.ID, ctx.Err() != nil || off); err != nil {
		r.log.Error("finish match apply failed", "run", run.ID, "err", err)
		r.settleOwn(ctx, run.ID)
	}
}

// applyItem writes one item's plan to its book: applied, skipped (nothing to
// change, or no book there now) or failed, with a code saying why. A community
// match it records (Writes.Anchor) goes in with the fields, in one transaction:
// made "auto" for a confident item, "accepted" for a review item the admin
// included, credited to this run. A refresh's heal or new identifier and a
// repick's ASIN only rewrite the book's own match, which keeps how, when and by
// whom it was made (catalog.MatchAnchor: the anchor write under a MatchIf).
func (r *Runner) applyItem(ctx context.Context, run *catalog.MatchRun, it *catalog.MatchRunItem, o ApplyOptions) (applied, detail string) {
	mode := run.Mode
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
	if newMatches(mode) && matchedSince(mode, st, it) {
		return catalog.ItemSkipped, "matched_since"
	}
	if asinKnownSince(mode, st, it) {
		return catalog.ItemSkipped, DetailASINKnown
	}
	w := planFor(mode, o.Scope, st, it)
	if w.Empty() {
		// A refresh item too: it had a change when the run recorded it (one with
		// none is never read: MatchItemsToApply), so the review showed one, and
		// the book took it since, or no longer has what it was found by
		// (server#355).
		return catalog.ItemSkipped, "nothing_to_change"
	}
	edits := len(w.Set) > 0 || len(w.Revert) > 0 || w.Anchor != nil
	if edits {
		edit := catalog.BookEdit{Set: w.Set, Revert: w.Revert, Source: catalog.SourceCommunity, UserID: o.UserID,
			KeepEdited: true}
		if w.Anchor != nil {
			how := catalog.MatchHowAccepted
			if it.Outcome == catalog.OutcomeAuto {
				how = catalog.MatchHowAuto
			}
			// Only while the book's match is still the one read here (for a
			// match item none, and no ASIN or ISBN either: matchedSince, checked
			// again in the edit's transaction): a match made by hand meanwhile
			// stays with its values, and the whole edit is skipped
			// (BookEdit.MatchIf).
			was := st.Anchor
			edit.MatchIf = &was
			if mode == catalog.MatchModeUnknownASIN && was.WorkID == "" {
				// Matched by its own ASIN alone: only while it still has it,
				// and the community still doesn't know it (asinKnownSince,
				// checked again in the edit's transaction).
				edit.MatchIfASIN = st.Fields[catalog.FieldASIN].Value
			}
			m := *w.Anchor
			m.How, m.RunID = how, run.ID
			edit.Match = &m
		}
		if r.beforeEdit != nil {
			r.beforeEdit(it)
		}
		if err := r.cat.EditBook(ctx, it.LibraryID, it.Path, edit); err != nil {
			if errors.Is(err, catalog.ErrNotFound) {
				return catalog.ItemSkipped, "book_gone"
			}
			if errors.Is(err, catalog.ErrMatchedSince) {
				return catalog.ItemSkipped, "matched_since"
			}
			if ctx.Err() == nil {
				r.log.Warn("match apply edit failed", "library", it.LibraryID, "path", it.Path, "err", err)
			}
			return catalog.ItemFailed, "edit_failed"
		}
	}
	if w.Cover {
		err := r.saveCover(ctx, it.LibraryID, it.Path, it.Proposal.CoverURL, o.UserID, coverExpect(st, w))
		if errors.Is(err, catalog.ErrCoverSuperseded) {
			// The book changed while the cover was fetched (an upload, a match
			// accepted by hand, an Unlink): the cover is no longer its own.
			if !edits {
				return catalog.ItemSkipped, DetailCoverSuperseded
			}
			return catalog.ItemApplied, DetailCoverSuperseded
		}
		if err != nil {
			if ctx.Err() == nil {
				r.log.Info("match apply cover failed", "library", it.LibraryID, "path", it.Path, "err", err)
			}
			if !edits {
				return catalog.ItemFailed, "cover_failed"
			}
			return catalog.ItemApplied, "cover_failed"
		}
	}
	return catalog.ItemApplied, ""
}

// coverExpect is the book a cover planned by w for a book in state st is for, once
// w's edit is in: the match it records (else the book's own) and its ASIN and
// ISBN after the edit (catalog.SetCoverIf's condition). Checked when the cover is
// written, so a change between the edit and the fetch's end is caught too.
func coverExpect(st *catalog.MatchState, w Writes) catalog.CoverExpect {
	anchor := st.Anchor
	if w.Anchor != nil {
		anchor = *w.Anchor
	}
	return catalog.CoverExpect{
		WorkID: anchor.WorkID, RecordingID: anchor.RecordingID,
		ASIN: catalog.CleanOverride(catalog.FieldASIN, w.after(st, catalog.FieldASIN)),
		ISBN: catalog.CleanOverride(catalog.FieldISBN, w.after(st, catalog.FieldISBN)),
	}
}

// Change is what applying an item under one scope would write: its fields (in
// catalog.OverrideFields order; set or reverted), whether it takes the community
// cover and whether it records the book's community match (Match: the work, as a
// new match or a refresh's heal of a merged one), and, when it changes the book's
// series, the series before and after, so a series it removes is shown, never
// silent.
type Change struct {
	Fields []string      `json:"fields"`
	Cover  bool          `json:"cover"`
	Match  bool          `json:"match,omitempty"`
	Series *SeriesChange `json:"series,omitempty"`
}

// SeriesChange is a book's series before (From) and after (To) an apply: the main
// series first, with its position (0 = none), then the others.
type SeriesChange struct {
	From []catalog.SeriesRef `json:"from"`
	To   []catalog.SeriesRef `json:"to"`
}

// changeOf is what w writes to a book in state st, as the review shows it.
func changeOf(st *catalog.MatchState, w Writes) Change {
	ch := Change{Fields: []string{}, Cover: w.Cover, Match: w.Anchor != nil}
	for _, f := range catalog.OverrideFields {
		if _, ok := w.Set[f]; ok || slices.Contains(w.Revert, f) {
			ch.Fields = append(ch.Fields, f)
		}
	}
	series := func(value func(string) string) []catalog.SeriesRef {
		return catalog.SeriesList(catalog.SeriesAt(value(catalog.FieldSeries), value(catalog.FieldSeriesIndex)), value(catalog.FieldMoreSeries))
	}
	from := series(func(f string) string { return st.Fields[f].Value })
	if to := series(func(f string) string { return w.after(st, f) }); !slices.Equal(from, to) {
		ch.Series = &SeriesChange{From: from, To: to}
	}
	return ch
}

// ItemView is an item as the review shows it: the book as it is now (Gone when
// nothing is indexed at its path any more) and what each scope would change.
type ItemView struct {
	catalog.MatchRunItem
	Book ItemBook `json:"book"`
	Gone bool     `json:"gone,omitempty"`
	// MatchedSince: a match or unknown_asin run's book matched since the run
	// (matchedSince; an unknown_asin run's also once the community knows its
	// ASIN, asinKnownSince), which an apply leaves alone; its changes are none.
	MatchedSince      bool              `json:"matched_since,omitempty"`
	IdentifierUnknown bool              `json:"identifier_unknown,omitempty"`
	Changes           map[string]Change `json:"changes"`
}

// ItemBook is the book an item is for, as it is now: Duration its length in
// seconds (omitted when unknown), for a length_differs item's wording, and
// CoverVersion its Book.CoverVersion (the cover's cache buster).
type ItemBook struct {
	Title        string  `json:"title"`
	Author       string  `json:"author"`
	Duration     float64 `json:"duration,omitempty"`
	ASIN         string  `json:"asin,omitempty"`
	ISBN         string  `json:"isbn,omitempty"`
	CoverVersion string  `json:"cover_version,omitempty"`
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
		if run.Mode == catalog.MatchModeRefresh {
			_, missed := splitFallback(it.Proposal.FoundBy)
			v.IdentifierUnknown = missed != ""
		}
		st, err := r.cat.BookMatchState(ctx, it.LibraryID, it.Path)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			v.Gone = true
		case err != nil:
			return nil, 0, err
		default:
			v.Book = ItemBook{Title: st.Title, Author: st.Author, Duration: st.Duration, CoverVersion: st.CoverVersion}
			if it.Outcome == catalog.OutcomeNone {
				v.Book.ASIN, v.Book.ISBN = st.Fields[catalog.FieldASIN].Value, st.Fields[catalog.FieldISBN].Value
			}
			v.MatchedSince = newMatches(run.Mode) && it.Applied == "" &&
				(matchedSince(run.Mode, st, &it) || asinKnownSince(run.Mode, st, &it))
			for _, scope := range scopes {
				if slices.Contains(it.Unchanged, scope) {
					// Recorded with none: an apply under it leaves the item out
					// unread (MatchItemsToApply), so the review shows none either.
					v.Changes[scope] = Change{Fields: []string{}}
					continue
				}
				v.Changes[scope] = changeOf(st, planFor(run.Mode, scope, st, &it))
			}
		}
		out[i] = v
	}
	return out, next, nil
}

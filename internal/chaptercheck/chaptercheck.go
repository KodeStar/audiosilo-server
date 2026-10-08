// Package chaptercheck checks books against the community's chapter lists in the
// background: for each book with an ASIN or ISBN it fetches the matched recording's
// chapters (meta.Service.RecordingChapters), fits them onto the book's audio
// (chapteralign, with ffmpeg finding the pauses) and records the outcome
// (catalog.SaveCommunityChapters), which puts the community chapters in place when
// they should be. A book is checked again when what the check saw changes (a new
// match, other audio) or the check is a month old.
//
// It runs apart from the scan job queue, as bulk matching does: it waits on the
// network, and library must not import meta.
package chaptercheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/chapteralign"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/pool"
)

// Bounds on the background pass. The community service is shared: a couple of
// books at a time, and a pass stops when it keeps failing.
const (
	// interval is how often the background looks for books due a check (a Kick
	// looks at once).
	interval = 10 * time.Minute
	// staleAfter is how old a check gets before the book is checked again.
	staleAfter = 30 * 24 * time.Hour
	// batch is how many due books are read at a time.
	batch   = 50
	workers = 2
	// maxFailStreak consecutive books the service failed for end the pass, and
	// the background then waits for the next interval, however many books a scan
	// changes meanwhile.
	maxFailStreak = 5
	// settleKicks is how long a kick waits for the kicks behind it.
	settleKicks = 5 * time.Second
	// retryAfter is how long a book a check failed for waits before it is tried
	// again: one that keeps failing can't hold up the books behind it.
	retryAfter = time.Hour
	// checkTimeout bounds one book: the lookup, the chapters and every pause
	// window a long book needs.
	checkTimeout = 5 * time.Minute
	// probeBudget is the share of checkTimeout the pause windows get. Past it the
	// boundaries left are placed by proportion alone (approximate) and the fit is
	// still recorded, so a long book on a slow disk can't fail every check.
	probeBudget = 4 * time.Minute
)

// Source is the community's chapter lists (meta.Service.RecordingChapters).
type Source interface {
	RecordingChapters(ctx context.Context, asin, isbn string) (*meta.RecordingChapters, error)
}

// Runner checks books' community chapters in the background, and one book on
// request.
type Runner struct {
	cat *catalog.Catalog
	src Source
	// ffmpeg finds the pauses a fit snaps to; "" fits without them.
	ffmpeg string
	// enabled is the live metadata switch: nothing is checked while it is off.
	enabled func() bool
	log     *slog.Logger
	now     func() time.Time

	kick chan struct{}
	// settle is how long a kick waits before its pass, gathering the kicks behind
	// it (a scan kicks once per book indexed).
	settle time.Duration
	mu     sync.Mutex
	busy   map[catalog.Ref]bool // books being checked now
	// failed are the books the background's checks last failed for, and when.
	failed map[catalog.Ref]time.Time
	wg     sync.WaitGroup // Start's checks
}

// ErrBusy is a check of a book that is being checked already.
var ErrBusy = errors.New("the book is being checked already")

// New returns a Runner; Run starts its background pass.
func New(cat *catalog.Catalog, src Source, ffmpeg string, enabled func() bool, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{cat: cat, src: src, ffmpeg: ffmpeg, enabled: enabled, log: log, now: time.Now,
		kick: make(chan struct{}, 1), settle: settleKicks, busy: map[catalog.Ref]bool{}, failed: map[catalog.Ref]time.Time{}}
}

// Run checks the books due a check every interval, and on a Kick, until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		failing := false
		if r.enabled() {
			failing = r.pass(ctx)
		}
		kick := r.kick
		if failing {
			kick = nil // the service keeps failing: wait for the tick
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
			// Let a burst of kicks (a scan) settle into one pass.
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.settle):
			}
			select {
			case <-r.kick:
			default:
			}
		}
	}
}

// Kick asks the background to look for due books now (a scan, a match or an edit
// may have made some).
func (r *Runner) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Checking reports whether the book is being checked now.
func (r *Runner) Checking(ref catalog.Ref) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busy[ref]
}

// pass checks every due book once, a few at a time, until none is left, the
// service keeps failing (true), or metadata is switched off. A book a check
// failed for lately is left for later.
func (r *Runner) pass(ctx context.Context) bool {
	tried := map[catalog.Ref]bool{}
	waiting := r.failedLately()
	streak := 0
	for {
		// Ask past the books this pass tried and those waiting out a failure, which
		// stay due: they can't fill the batch and keep the rest from their turn.
		due, err := r.cat.DueChapterChecks(ctx, r.now().Add(-staleAfter).UTC().Format(time.RFC3339),
			batch+len(tried)+len(waiting))
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("community chapters: list due books failed", "err", err)
			}
			return false
		}
		var todo []catalog.Ref
		for _, b := range due {
			if !tried[b] && !waiting[b] {
				tried[b] = true
				todo = append(todo, b)
			}
		}
		if len(todo) == 0 {
			return false
		}
		var mu sync.Mutex
		more := func() bool {
			mu.Lock()
			defer mu.Unlock()
			return streak < maxFailStreak && r.enabled()
		}
		pool.Each(ctx, workers, todo, more, func(b *catalog.Ref) {
			_, err := r.Check(ctx, *b)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil, errors.Is(err, catalog.ErrNotFound), errors.Is(err, ErrBusy):
				streak = 0
				r.noteFailure(*b, false)
			case ctx.Err() != nil:
			default:
				streak++
				r.noteFailure(*b, true)
				r.log.Warn("community chapters: check failed", "library", b.LibraryID, "path", b.Path, "err", err)
			}
		})
		if ctx.Err() != nil {
			return false
		}
		if !more() {
			mu.Lock()
			defer mu.Unlock()
			return streak >= maxFailStreak
		}
	}
}

// failedLately is the books a check failed for within retryAfter (forgetting
// older failures).
func (r *Runner) failedLately() map[catalog.Ref]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[catalog.Ref]bool{}
	for ref, at := range r.failed {
		if r.now().Sub(at) < retryAfter {
			out[ref] = true
		} else {
			delete(r.failed, ref)
		}
	}
	return out
}

// Failed reports whether the last check of the book failed (the service didn't
// answer, or the check ran out of time), so the book page can say so: its
// recorded check is then the one before.
func (r *Runner) Failed(ref catalog.Ref) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.failed[ref]
	return ok
}

// noteFailure records (or, failed false, forgets) that a check failed for ref.
func (r *Runner) noteFailure(ref catalog.Ref, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if failed {
		r.failed[ref] = r.now()
	} else {
		delete(r.failed, ref)
	}
}

// Start checks one book in the background, under ctx (the server's lifetime):
// false when it is being checked already. Checking reports it from the moment
// Start returns. Asked for, the book is fitted again even when the list and the
// audio are what the last check saw (that one may have had no ffmpeg, or the
// library offline, and so no pauses to snap to).
func (r *Runner) Start(ctx context.Context, ref catalog.Ref) bool {
	ref.Path = catalog.CleanRelPath(ref.Path)
	if !r.claim(ref) {
		return false
	}
	r.wg.Go(func() {
		defer r.release(ref)
		_, err := r.check(ctx, ref, true)
		failed := err != nil && ctx.Err() == nil && !errors.Is(err, catalog.ErrNotFound)
		if failed {
			r.log.Warn("community chapters: check failed", "library", ref.LibraryID, "path", ref.Path, "err", err)
		}
		if ctx.Err() == nil {
			r.noteFailure(ref, failed)
		}
	})
	return true
}

// Check checks one book now and records the outcome: its check, or the
// service's failure (nothing is recorded then). ErrNotFound when no book is
// indexed at the path, ErrBusy while it is being checked already.
func (r *Runner) Check(ctx context.Context, ref catalog.Ref) (*catalog.CommunityChapters, error) {
	ref.Path = catalog.CleanRelPath(ref.Path)
	if !r.claim(ref) {
		return nil, ErrBusy
	}
	defer r.release(ref)
	return r.check(ctx, ref, false)
}

func (r *Runner) claim(ref catalog.Ref) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busy[ref] {
		return false
	}
	r.busy[ref] = true
	return true
}

func (r *Runner) release(ref catalog.Ref) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.busy, ref)
}

// check is one book's check (see Check), the book claimed; refit fits it again
// even when the last check saw the same list for the same audio.
func (r *Runner) check(ctx context.Context, ref catalog.Ref, refit bool) (*catalog.CommunityChapters, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	in, err := r.cat.ChapterInputs(ctx, ref.LibraryID, ref.Path)
	if err != nil {
		return nil, err
	}
	cc := catalog.CommunityChapters{LibraryID: ref.LibraryID, Path: ref.Path, Basis: in.Basis}
	rc, err := r.src.RecordingChapters(ctx, in.ASIN, in.ISBN)
	switch {
	case errors.Is(err, meta.ErrNotFound):
		cc.Status = catalog.CommunityNoMatch
	case err != nil:
		return nil, err
	case len(rc.Chapters) < 2:
		cc.Status, cc.WorkID, cc.RecordingID = catalog.CommunityUnavailable, rc.WorkID, rc.RecordingID
	default:
		cc.WorkID, cc.RecordingID, cc.ListHash = rc.WorkID, rc.RecordingID, listHash(rc.Chapters)
		// The same list for the same audio fits as it did: renew the check, don't
		// find every pause again.
		if prev, err := r.cat.GetCommunityChapters(ctx, ref.LibraryID, ref.Path); err != nil {
			return nil, err
		} else if !refit && prev != nil && prev.Basis == cc.Basis && prev.ListHash == cc.ListHash {
			return prev, r.cat.TouchCommunityChapters(ctx, ref.LibraryID, ref.Path)
		}
		probe, err := r.prober(ctx, ref.LibraryID)
		if err != nil {
			return nil, err
		}
		if probe != nil {
			// The pauses run on a budget of their own: a window past it fails, and
			// chapteralign leaves it unsnapped, rather than the check's deadline
			// failing the whole fit.
			budget, cancelBudget := context.WithTimeout(ctx, probeBudget)
			defer cancelBudget()
			inner := probe
			probe = func(_ context.Context, file string, from, to float64) ([]chapteralign.Silence, error) {
				return inner(budget, file, from, to)
			}
		}
		res, err := chapteralign.Align(ctx, files(in.Files), in.Chapters, remotes(rc.Chapters), probe)
		if err != nil {
			return nil, err
		}
		cc.Status, cc.Detail, cc.Chapters = string(res.Status), &res.Detail, res.Chapters
	}
	if err := r.cat.SaveCommunityChapters(ctx, cc); err != nil {
		return nil, err
	}
	return &cc, nil
}

// prober finds pauses in the library's files with ffmpeg, through SafeJoin like
// every read of the library.
func (r *Runner) prober(ctx context.Context, libraryID int64) (chapteralign.Prober, error) {
	if r.ffmpeg == "" {
		return nil, nil
	}
	lib, err := r.cat.GetLibrary(ctx, libraryID)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, file string, from, to float64) ([]chapteralign.Silence, error) {
		abs, err := library.SafeJoin(lib.Root, file)
		if err != nil {
			return nil, err
		}
		pauses, err := media.DetectSilences(ctx, r.ffmpeg, abs, from, to)
		if err != nil {
			return nil, err
		}
		out := make([]chapteralign.Silence, len(pauses))
		for i, p := range pauses {
			out[i] = chapteralign.Silence(p)
		}
		return out, nil
	}, nil
}

func files(in []catalog.BookFile) []chapteralign.File {
	out := make([]chapteralign.File, len(in))
	for i, f := range in {
		out[i] = chapteralign.File{Path: f.RelPath, Duration: f.Duration}
	}
	return out
}

// listHash names a community list, so a recheck can tell it unchanged.
func listHash(chs []meta.Chapter) string {
	h := sha256.New()
	for _, c := range chs {
		fmt.Fprintf(h, "%d\x00%d\x00%s\x00", c.StartMS, c.LengthMS, c.Title)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func remotes(in []meta.Chapter) []chapteralign.Remote {
	out := make([]chapteralign.Remote, len(in))
	for i, c := range in {
		out[i] = chapteralign.Remote(c)
	}
	return out
}

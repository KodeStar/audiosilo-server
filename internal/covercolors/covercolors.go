// Package covercolors reads books' cover colours in the background. A colour is
// otherwise read only when a thumbnail of the art is made for someone (the
// player's cover, the console's grids), so a library nobody has browsed through
// has none; the pass reads the colour of every book that may have art and holds
// none for it (catalog.CoverColorsDue), one book at a time, so things drawn in a
// cover's colours (the console's Series spines, the player's themed screens) have
// them soon after a book is indexed.
//
// The reading itself (the art, its thumbnail, the palette) is the cover
// endpoints' own (api), handed in as a Colorer.
package covercolors

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Bounds on the background pass: it shares the disk with the scan and the art
// reads and decodes with the cover endpoints, so it reads one book at a time.
const (
	// interval is how often the background looks for books due a colour without
	// a Kick: a read that failed is worth trying again.
	interval = time.Hour
	// settleKicks is how long a pass waits with no Kick before it starts (a scan
	// kicks once per book indexed, so the pass waits for the scan to end), and
	// how long the first waits after the start; maxSettle bounds the wait.
	settleKicks = 30 * time.Second
	maxSettle   = 10 * time.Minute
	// batch is how many books are read from the index at a time.
	batch = 100
	// maxFailStreak consecutive books whose art couldn't be read (a library on a
	// mount that has gone away) end the pass until the next interval.
	maxFailStreak = 10
	// recordTimeout bounds recording a batch's colours: the single writer may be
	// held by a scan, and the colours are read again next pass if it is.
	recordTimeout = 30 * time.Second
)

// Colorer reads the colour of a due book's cover, in lib, from a thumbnail of its
// art: a record with a zero Color when it has no art, or none that decodes;
// ErrArtMissing when the files its art is read from aren't there; another error
// when the art couldn't be read and is worth trying again.
type Colorer func(ctx context.Context, lib *catalog.Library, due catalog.CoverColorDue) (catalog.CoverColorRecord, error)

// ErrArtMissing is a Colorer's answer for a book whose art files aren't there: an
// unmounted share (the scan keeps its books indexed) or a book not pruned yet.
// Recording "no colour" would stick once the share is back (its books aren't
// re-indexed), so the book is left due and read again next pass; it isn't a
// failure either, so a library of them never stops the pass for the others.
var ErrArtMissing = errors.New("the book's art files are missing")

// Runner reads cover colours in the background.
type Runner struct {
	cat   *catalog.Catalog
	color Colorer
	log   *slog.Logger

	kick   chan struct{}
	settle time.Duration
}

// New returns a Runner; Run starts its background pass.
func New(cat *catalog.Catalog, color Colorer, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{cat: cat, color: color, log: log, kick: make(chan struct{}, 1), settle: settleKicks}
}

// Kick asks the background to look for books due a colour once the kicks settle
// (a scan or an edit may have made some).
func (r *Runner) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run reads the colours of the books due one shortly after it starts, then every
// interval and after a Kick, until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if !r.settled(ctx) {
			return
		}
		kick := r.kick
		if r.pass(ctx) {
			kick = nil // the art keeps failing to read: wait for the tick
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
	}
}

// settled waits until no Kick has come for r.settle, at most maxSettle; false
// when ctx ended first.
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

// pass reads the colour of every due book once and records them a batch at a
// time; true when it stopped because the art kept failing to read.
func (r *Runner) pass(ctx context.Context) bool {
	libs, err := r.cat.ListLibraries(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("cover colours: list libraries failed", "err", err)
		}
		return false
	}
	byID := make(map[int64]*catalog.Library, len(libs))
	for i := range libs {
		byID[libs[i].ID] = &libs[i]
	}
	var after int64
	read, streak := 0, 0
	defer func() {
		if read > 0 {
			r.log.Info("cover colours read", "books", read)
		}
	}()
	for {
		due, next, err := r.cat.CoverColorsDue(ctx, after, batch)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("cover colours: list due books failed", "err", err)
			}
			return false
		}
		var recs []catalog.CoverColorRecord
		for _, d := range due {
			if ctx.Err() != nil || streak >= maxFailStreak {
				break
			}
			lib := byID[d.LibraryID]
			if lib == nil {
				continue // a library added since the pass began waits for the next
			}
			rec, err := r.color(ctx, lib, d)
			switch {
			case errors.Is(err, ErrArtMissing):
				continue // left due: read again next pass
			case err != nil:
				if ctx.Err() == nil {
					streak++
					r.log.Debug("cover colours: read art failed", "library", d.LibraryID, "path", d.Path, "err", err)
				}
				continue
			}
			streak = 0
			recs = append(recs, rec)
			if rec.Color.Bg != "" {
				read++
			}
		}
		r.record(ctx, recs)
		switch {
		case ctx.Err() != nil:
			return false
		case streak >= maxFailStreak:
			r.log.Warn("cover colours: the art keeps failing to read; trying again later", "failures", streak)
			return true
		case next == 0:
			return false
		}
		after = next
	}
}

// record stores a batch's colours. A failure costs only the colours, read again
// next pass, so it is logged: at debug when the writer stayed busy past
// recordTimeout (a scan) or the server is stopping, which are expected.
func (r *Runner) record(ctx context.Context, recs []catalog.CoverColorRecord) {
	if len(recs) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	switch err := r.cat.RecordCoverColors(wctx, recs); {
	case err == nil:
	case wctx.Err() != nil || ctx.Err() != nil:
		r.log.Debug("cover colours: record failed (the writer is busy, or the server is stopping)", "err", err)
	default:
		r.log.Warn("cover colours: record failed", "err", err)
	}
}

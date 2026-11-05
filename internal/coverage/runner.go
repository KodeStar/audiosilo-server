// Package coverage is the opt-in daily report of unknown Audible ASINs
// (LibraForge C3, metadata.report_unknown): once a day it looks up the ASINs of
// this server's books (catalog.UnknownASINCandidates) in the community
// metadata database and sends the metadata service the ones it doesn't know,
// as {"asins": [...]} and nothing else, so the community can add them.
//
// A run:
//  1. nothing at all while the switch or metadata is off (whileOn: a run in
//     flight stops within a second), on a demo server (never built), within a
//     day of the last run, or within the week after the service said it takes
//     no reports (not_offered);
//  2. a base_url the report may not go to (plain http off this machine:
//     meta.Service.CanReport) is not_offered with no call;
//  3. in mirror mode, nothing until the local copy is ready, and the lookups
//     stay local: the copy answers them, and one it fails (or can't take, the
//     copy dropped mid-run) is a failed lookup, never a question to the remote
//     service (meta.Service.KnownASIN); the POST goes remote (and a Ping
//     the copy fails, which carries no ASIN);
//  4. Ping, then up to maxChecks lookups of the candidates never looked up or
//     looked up over recheckAge ago, in a shuffled order; only a 404 is
//     "unknown", any other failure stops the lookups (the rest wait);
//  5. the unknown ones not sent within resendAge, shuffled, in chunks of
//     chunkSize, at most maxPosts POSTs, and only when nothing was sent in the
//     last minGap (MAX(sent_at), across restarts); a 202 marks its chunk sent,
//     a 404/405/3xx/odd 2xx is not_offered (a week's quiet), anything else
//     marks nothing and waits for the next day.
//
// Nothing stored names a book: rows are ASINs, and one no book holds any more
// is dropped at the start of the next run.
package coverage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// Metadata is what the job asks the community metadata service
// (*meta.Service).
type Metadata interface {
	Ping() meta.Health
	CanReport() bool
	KnownASIN(ctx context.Context, asin string) (bool, error)
	ReportUnknownASINs(ctx context.Context, asins []string) error
}

// Options configure a Runner.
type Options struct {
	// On reports whether the job may run now: metadata on AND
	// metadata.report_unknown. Required.
	On func() bool
	// Ready reports whether lookups may be made: in mirror mode, the local
	// copy is loaded. nil is always ready (remote mode).
	Ready func() bool
	Now   func() time.Time
	Log   *slog.Logger
}

// The run's states (the console's status line).
const (
	StateNever      = "never"
	StateOK         = "ok"
	StateNotOffered = "not_offered"
	StateFailed     = "failed"
)

// Schedule and bounds.
const (
	firstDelay = time.Hour      // after the start
	interval   = 24 * time.Hour // between runs
	jitter     = time.Hour      // +- on interval
	// minGap: a run within it of the last one is skipped, and nothing is
	// POSTed within it of the last send (MAX(sent_at)), whatever restarted.
	minGap     = 23 * time.Hour
	offPoll    = time.Minute // while off, how often the switch is looked at
	notReady   = time.Hour   // mirror mode: how soon a run waiting for the copy looks again
	quietFor   = 7 * 24 * time.Hour
	recheckAge = 30 * 24 * time.Hour
	resendAge  = 30 * 24 * time.Hour
	maxChecks  = 500
	chunkSize  = 200
	maxPosts   = 5
)

// errSwitchedOff ends a run the report (or metadata) was switched off during:
// a cancellation, no status recorded.
var errSwitchedOff = fmt.Errorf("unknown ASIN report switched off: %w", context.Canceled)

// errNotReady is a run held back until the mirror's local copy is ready.
var errNotReady = errors.New("unknown ASIN report: the local copy of the metadata is not ready")

// Runner is the daily report job.
type Runner struct {
	cat  *catalog.Catalog
	meta Metadata
	opt  Options
	log  *slog.Logger

	wake chan struct{}
	// mu makes one run at a time.
	mu sync.Mutex
	// offCheck is how often a run looks at the switch (whileOn).
	offCheck time.Duration
	// httpsLogged: the "needs an https URL" line is logged once a process.
	httpsLogged bool
}

// New returns a Runner; Run starts its schedule.
func New(cat *catalog.Catalog, md Metadata, opt Options) *Runner {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Runner{cat: cat, meta: md, opt: opt, log: opt.Log, wake: make(chan struct{}, 1), offCheck: time.Second}
}

// Wake makes the schedule look again at once (the report or metadata was
// switched on): it runs now unless it ran within the day.
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run keeps the schedule until ctx ends: the first run an hour after the start
// (or a day after the last run, when that is later), then daily with jitter.
func (r *Runner) Run(ctx context.Context) {
	next := r.firstRun(ctx)
	for {
		wait := next.Sub(r.opt.Now())
		if !r.opt.On() {
			// Looked at again in a minute (Wake runs it at once when it is
			// switched on), never sooner: once next has passed, a wait capped
			// with min would be negative and the loop would spin.
			wait = offPoll
		}
		t := time.NewTimer(max(wait, 0))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-r.wake:
			t.Stop()
			if r.opt.On() && r.due(ctx) {
				next = r.opt.Now()
			}
			continue
		case <-t.C:
		}
		if !r.opt.On() || r.opt.Now().Before(next) {
			continue
		}
		switch err := r.RunOnce(ctx); {
		case errors.Is(err, errNotReady):
			next = r.opt.Now().Add(notReady)
		case errors.Is(err, errSwitchedOff):
			// looked at again once it is back on
		default:
			next = r.opt.Now().Add(interval + time.Duration(rand.Int64N(int64(2*jitter))) - jitter) //nolint:gosec // schedule jitter, not a secret
		}
	}
}

// firstRun is when the first run after the start is: an hour on, or a day
// after the last run when that is later.
func (r *Runner) firstRun(ctx context.Context) time.Time {
	at := r.opt.Now().Add(firstDelay)
	if st, err := r.cat.UnknownASINReportState(ctx); err == nil {
		if last, ok := parseStamp(st.LastRunAt); ok && last.Add(interval).After(at) {
			at = last.Add(interval)
		}
	}
	return at
}

// due reports whether no run has been made within minGap.
func (r *Runner) due(ctx context.Context) bool {
	st, err := r.cat.UnknownASINReportState(ctx)
	if err != nil {
		return false
	}
	last, ok := parseStamp(st.LastRunAt)
	return !ok || r.opt.Now().Sub(last) >= minGap
}

// RunOnce runs the report once, unless it is off (errSwitchedOff), waiting for
// the mirror's copy (errNotReady), ran within minGap, or is in its quiet week
// (both nil, no call). The error is the run's failure, also recorded in the
// status.
func (r *Runner) RunOnce(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.opt.On() {
		return errSwitchedOff
	}
	now := r.opt.Now()
	st, err := r.cat.UnknownASINReportState(ctx)
	if err != nil {
		return err
	}
	if last, ok := parseStamp(st.LastRunAt); ok && now.Sub(last) < minGap {
		return nil
	}
	if until, ok := parseStamp(st.QuietUntil); ok && now.Before(until) {
		return nil
	}
	if !r.meta.CanReport() {
		if !r.httpsLogged {
			r.httpsLogged = true
			r.log.Info("unknown ASIN report: the report needs an https metadata URL; nothing is sent")
		}
		r.record(ctx, catalog.UnknownASINReport{State: StateNotOffered, LastRunAt: stamp(now)})
		return nil
	}
	if r.opt.Ready != nil && !r.opt.Ready() {
		return errNotReady
	}
	rctx, stop := r.whileOn(ctx)
	defer stop()
	res, err := r.run(rctx, now)
	if rctx.Err() != nil {
		return context.Cause(rctx)
	}
	r.record(ctx, res)
	if err != nil {
		r.log.Warn("unknown ASIN report: run failed", "err", err, "sent", res.LastSent)
	}
	return err
}

// whileOn is ctx, cancelled with errSwitchedOff once the report is switched
// off (looked at every offCheck); stop releases it.
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

// run is one run's work: the status to record, and the failure when its state
// is failed.
func (r *Runner) run(ctx context.Context, now time.Time) (catalog.UnknownASINReport, error) {
	res := catalog.UnknownASINReport{State: StateOK, LastRunAt: stamp(now)}
	failed := func(err error) (catalog.UnknownASINReport, error) {
		res.State = StateFailed
		return res, err
	}
	if h := r.meta.Ping(); !h.Reachable {
		return failed(fmt.Errorf("metadata service unreachable: %s", h.Error))
	}
	candidates, err := r.cat.UnknownASINCandidates(ctx)
	if err != nil {
		return failed(fmt.Errorf("reading the books: %w", err))
	}
	rows, err := r.cat.UnknownASINRows(ctx)
	if err != nil {
		return failed(fmt.Errorf("reading the report's rows: %w", err))
	}
	// Forget what no book holds any more.
	held := make(map[string]bool, len(candidates))
	for _, a := range candidates {
		held[a] = true
	}
	var gone []string
	for a := range rows {
		if !held[a] {
			gone = append(gone, a)
			delete(rows, a)
		}
	}
	if err := r.cat.DropUnknownASINs(ctx, gone); err != nil {
		return failed(fmt.Errorf("dropping old rows: %w", err))
	}

	lookupErr := r.check(ctx, candidates, rows, now)
	if lookupErr != nil {
		res.State = StateFailed
	}
	if ctx.Err() != nil {
		return res, lookupErr
	}

	// The unknown ones, freshly confirmed and not sent within resendAge.
	var send []string
	for _, a := range candidates {
		row, ok := rows[a]
		if !ok || !row.Unknown || !within(row.CheckedAt, now, recheckAge) || within(row.SentAt, now, resendAge) {
			continue
		}
		send = append(send, a)
	}
	if len(send) == 0 {
		return res, lookupErr
	}
	last, err := r.cat.LastASINSentAt(ctx)
	if err != nil {
		return failed(fmt.Errorf("reading the last report: %w", err))
	}
	if within(last, now, minGap) {
		return res, lookupErr // once a day, whatever restarted
	}
	rand.Shuffle(len(send), func(i, j int) { send[i], send[j] = send[j], send[i] })
	for i := 0; i < len(send) && i/chunkSize < maxPosts; i += chunkSize {
		chunk := send[i:min(i+chunkSize, len(send))]
		err := r.meta.ReportUnknownASINs(ctx, chunk)
		switch {
		case errors.Is(err, meta.ErrReportNotOffered):
			r.log.Info("unknown ASIN report: the metadata service takes no reports; trying again in a week")
			res.State = StateNotOffered
			res.QuietUntil = stamp(now.Add(quietFor))
			return res, nil
		case err != nil:
			return failed(fmt.Errorf("sending the report: %w", err))
		}
		// Accepted: marked even if the run is being stopped, or the next run
		// would send them again.
		if err := r.cat.MarkASINsSent(context.WithoutCancel(ctx), chunk, stamp(r.opt.Now())); err != nil {
			return failed(fmt.Errorf("recording the report: %w", err))
		}
		res.LastSent += len(chunk)
	}
	return res, lookupErr
}

// check looks up the candidates due (never looked up, or over recheckAge ago),
// at most maxChecks in a shuffled order, records the answers and updates rows
// with them. The error is the first failed lookup, which ends the lookups.
func (r *Runner) check(ctx context.Context, candidates []string, rows map[string]catalog.UnknownASINRow, now time.Time) error {
	at := stamp(now)
	var due []string
	for _, a := range candidates {
		if row, ok := rows[a]; !ok || !within(row.CheckedAt, now, recheckAge) {
			due = append(due, a)
		}
	}
	rand.Shuffle(len(due), func(i, j int) { due[i], due[j] = due[j], due[i] })
	if len(due) > maxChecks {
		due = due[:maxChecks]
	}
	var checks []catalog.ASINCheck
	var lookupErr error
	for _, a := range due {
		known, err := r.meta.KnownASIN(ctx, a)
		if err != nil {
			lookupErr = fmt.Errorf("looking up an ASIN: %w", err)
			break
		}
		checks = append(checks, catalog.ASINCheck{ASIN: a, Unknown: !known})
	}
	// What was asked is kept even when the run is being stopped.
	if err := r.cat.SaveASINChecks(context.WithoutCancel(ctx), checks, at); err != nil {
		return fmt.Errorf("recording the lookups: %w", err)
	}
	for _, c := range checks {
		row := rows[c.ASIN]
		row.CheckedAt, row.Unknown = at, c.Unknown
		if !c.Unknown {
			row.SentAt = ""
		}
		rows[c.ASIN] = row
	}
	return lookupErr
}

// record stores a run's status.
func (r *Runner) record(ctx context.Context, st catalog.UnknownASINReport) {
	if err := r.cat.SaveUnknownASINReportState(context.WithoutCancel(ctx), st); err != nil {
		r.log.Warn("unknown ASIN report: saving the run status failed", "err", err)
	}
}

// Status is the report's state for the console (metadata.report_status).
type Status struct {
	State     string `json:"state"`
	LastRunAt string `json:"last_run_at,omitempty"`
	LastSent  int    `json:"last_sent"`
}

// Status reads the report's last run.
func (r *Runner) Status(ctx context.Context) (Status, error) {
	st, err := r.cat.UnknownASINReportState(ctx)
	if err != nil {
		return Status{}, err
	}
	if st.State == "" {
		return Status{State: StateNever}, nil
	}
	return Status{State: st.State, LastRunAt: st.LastRunAt, LastSent: st.LastSent}, nil
}

// within reports whether the stamp at is less than age before now ("" never is).
func within(at string, now time.Time, age time.Duration) bool {
	t, ok := parseStamp(at)
	return ok && now.Sub(t) < age
}

// stamp and parseStamp are the catalog's fixed-width UTC millisecond time.
var (
	stamp      = catalog.FormatStamp
	parseStamp = catalog.ParseStamp
)

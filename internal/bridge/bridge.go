// Package bridge is the server's side of the Bridge (BRIDGE-PLAN.md): the
// optional companion that writes to the libraries and runs the heavy jobs, so
// the server can stay read-only. The server never connects to the Bridge. It
// keeps the Bridge's last heartbeat (status.go) and the jobs admins ask for
// (jobs.go); the Bridge calls in to report, claim and update. Pairing and the
// Bridge's token are auth's (auth/bridge.go).
package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// Service is the Bridge's status and job queue over the store.
type Service struct {
	db  *store.DB
	now func() time.Time
	// wake is closed (and replaced) when a job is queued, so a claim waiting in
	// waitClaim looks again at once.
	wake         *signal
	ended        *signal
	fetchWaitMu  sync.Mutex
	fetchWait    map[fetchKey]*fetchWatch
	secretFlowMu sync.Mutex
	secretMu     sync.Mutex
	privateURLs  map[int64]string
	// beforeSecretHeld, tests only, runs in Subscribe between the job's
	// commit and storing its private URL.
	beforeSecretHeld func(*Job)
	// OnEnded hears every committed terminal transition, on a context with a
	// budget of its own (foldContext). It must return at once; wake background
	// work rather than re-entering the queue here.
	OnEnded func(ctx context.Context, job Job)

	// OnTransition, when set (at wiring, before the first request), hears about
	// every move of a job into a state an admin is told about (Job.Notable,
	// a park once per outage: parkNews): the Bridge's update, or a lease that
	// ran out on the last attempt. It runs once the move has committed, on the
	// caller's goroutine.
	OnTransition func(ctx context.Context, job Job, from string)

	// ModuleOn, when set (at wiring, before the first request), says whether a
	// managed module is on. A queued job that needs one that is off (ModuleFor)
	// is held: no claim hands it out until the module is on again, so turning
	// one off leaves its queued jobs at rest, and it refuses no new job that
	// cancels it (givesWay: a generate job replaces a held share). A job the
	// Bridge holds (claimed, running or parked) carries on, but for one that
	// only files to the community, asked to stop at its next report as claimed
	// or running (stopsShare, CancelModuleOff), a parked one once it reports
	// working again. It is the toggle alone, never the capability flag, which
	// follows the Bridge's heartbeat.
	ModuleOn func(id string) bool

	// ClaimAudibleSync, when set (at wiring, before the first request), is
	// the params a claim of an account's audible.sync answers: the account and
	// its titles to sync as they are now (AudibleSyncClaim). The queued job
	// stores the account alone (server#303); with none wired a claim answers
	// the params as queued.
	ClaimAudibleSync func(ctx context.Context, account string) (json.RawMessage, error)
	// Log, when set (at wiring, before the first request), hears what a claim
	// settles without answering an error (endUnfillableClaim ending a job it
	// can't fill); New leaves it discarding.
	Log *slog.Logger
	// syncFills counts, per Audible account, the claims in a row that failed
	// to read its audible.sync's titles (fillAudibleSync), under syncFillMu.
	syncFillMu sync.Mutex
	syncFills  map[string]syncFill

	// hb caches the last heartbeat as read (status.go), so GET /server, which
	// every client calls often, reads the database at most every heartbeatTTL.
	// hbGen counts the cache's writes (a heartbeat saved, Forget): a read that
	// began before one never replaces its newer answer.
	hbMu   sync.Mutex
	hb     *Heartbeat
	hbRead time.Time
	hbGen  uint64

	// offlineSince is when expireOfflineSubscribes first saw no online Bridge
	// for podcast.subscribe (its clock), zero while it last saw one: the
	// silence must last OnlineWindow on this clock before a request fails, so
	// a server restart, a host waking from sleep or a wall clock that jumped
	// fails none before the Bridge's next heartbeat.
	offlineMu    sync.Mutex
	offlineSince time.Time

	// announced is the park last announced (parkNews) per open job, under
	// announcedMu: a Bridge that resumes a parked job on a timer and parks it
	// again for the same reason (GitHub down, a credential still refused) is
	// told to admins once per outage, not on every retry. In memory only: a
	// restart forgets it, so a park still on is announced once more.
	announcedMu sync.Mutex
	announced   map[int64]announcedPark
}

// announcedPark is the park code announced for a job and the claim (attempt)
// it was announced on.
type announcedPark struct {
	attempt int
	code    string
}

// New returns a Service. now may be nil to use time.Now.
func New(db *store.DB, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, now: now, wake: newSignal(), ended: newSignal(), privateURLs: map[int64]string{},
		syncFills: map[string]syncFill{}, announced: map[int64]announcedPark{}, Log: slog.New(slog.DiscardHandler)}
}

// ts is now as the bridge tables store times: RFC3339 UTC, whole seconds, so
// stored times compare as text.
func (s *Service) ts() string { return s.now().UTC().Format(time.RFC3339) }

// at is t as the bridge tables store it.
func at(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// committedContext ignores a disconnected caller while retaining the operation's
// absolute deadline, so a notification (OnTransition) cannot extend a bounded
// media request.
func committedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	detached := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(detached, deadline)
	}
	return detached, func() {}
}

// minFoldBudget is the least time each OnEnded fold gets of its own
// (foldContext), whatever the caller's deadline has left, and maxFoldOverrun how
// far past that deadline the folds of one caller may run in all.
const (
	minFoldBudget  = 5 * time.Second
	maxFoldOverrun = 2 * minFoldBudget
)

// foldContext is one OnEnded fold's context: the caller's deadline as
// committedContext keeps it, but never less than minFoldBudget from now, so a
// slow fold, or a stream's short budget spent before an unrelated job's end was
// settled, can't leave the next fold no time (server#219). The floor stops at
// maxFoldOverrun past the caller's deadline, so a settle that ends many jobs
// holds its request that much longer at most; a fold that misses it is
// recovered by the scheduler's sweep (EndedPodcasts). Without a caller deadline
// it is unbounded, as committedContext's.
func foldContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		floor := time.Now().Add(minFoldBudget)
		if limit := deadline.Add(maxFoldOverrun); limit.Before(floor) {
			floor = limit
		}
		if deadline.Before(floor) {
			return context.WithDeadline(context.WithoutCancel(ctx), floor)
		}
	}
	return committedContext(ctx)
}

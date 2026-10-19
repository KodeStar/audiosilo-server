// Package bridge is the server's side of the Bridge (BRIDGE-PLAN.md): the
// optional companion that writes to the libraries and runs the heavy jobs, so
// the server can stay read-only. The server never connects to the Bridge. It
// keeps the Bridge's last heartbeat (status.go) and the jobs admins ask for
// (jobs.go); the Bridge calls in to report, claim and update. Pairing and the
// Bridge's token are auth's (auth/bridge.go).
package bridge

import (
	"context"
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
	wake *signal

	// OnTransition, when set (at wiring, before the first request), hears about
	// every move of a job into a state an admin is told about (Job.Notable): the
	// Bridge's update, or a lease that ran out on the last attempt. It runs once
	// the move has committed, on the caller's goroutine.
	OnTransition func(ctx context.Context, job Job, from string)

	// ModuleOn, when set (at wiring, before the first request), says whether a
	// managed module is on. A queued job that needs one that is off (ModuleFor)
	// is held: no claim hands it out until the module is on again, so turning
	// one off leaves its queued jobs at rest (a job the Bridge holds, claimed,
	// running or parked, carries on).
	ModuleOn func(id string) bool

	// hb caches the last heartbeat as read (status.go), so GET /server, which
	// every client calls often, reads the database at most every heartbeatTTL.
	// hbGen counts the cache's writes (a heartbeat saved, Forget): a read that
	// began before one never replaces its newer answer.
	hbMu   sync.Mutex
	hb     *Heartbeat
	hbRead time.Time
	hbGen  uint64
}

// New returns a Service. now may be nil to use time.Now.
func New(db *store.DB, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, now: now, wake: newSignal()}
}

// ts is now as the bridge tables store times: RFC3339 UTC, whole seconds, so
// stored times compare as text.
func (s *Service) ts() string { return s.now().UTC().Format(time.RFC3339) }

// at is t as the bridge tables store it.
func at(t time.Time) string { return t.UTC().Format(time.RFC3339) }

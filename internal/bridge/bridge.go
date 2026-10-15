// Package bridge is the server's side of the Bridge (BRIDGE-PLAN.md): the
// optional companion that writes to the libraries and runs the heavy jobs, so
// the server can stay read-only. The server never connects to the Bridge. It
// keeps the Bridge's last heartbeat (status.go) and the jobs admins ask for
// (jobs.go); the Bridge calls in to report, claim and update. Pairing and the
// Bridge's token are auth's (auth/bridge.go).
package bridge

import (
	"time"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// Service is the Bridge's status and job queue over the store.
type Service struct {
	db  *store.DB
	now func() time.Time
	// wake is closed (and replaced) when a job is queued, so a claim waiting in
	// WaitClaim looks again at once.
	wake *signal
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

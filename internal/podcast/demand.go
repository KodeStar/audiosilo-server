package podcast

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

const StreamWait = 2 * time.Second

// Demand keeps the absolute media budget around enqueue, observation and wait.
// The clock and wake timer can be replaced by deterministic tests, and so can
// the budget: it is wall-clock time (a context deadline), which a loaded
// machine can spend before the request is even queued (server#189).
type Demand struct {
	Catalog *catalog.Catalog
	Bridge  *bridge.Service
	Now     func() time.Time
	After   func(time.Duration) (<-chan time.Time, func())
	// Budget is the whole wait, StreamWait when zero.
	Budget time.Duration
}

func (d Demand) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}
func (d Demand) timer(t time.Duration) (<-chan time.Time, func()) {
	if d.After != nil {
		return d.After(t)
	}
	timer := time.NewTimer(t)
	return timer.C, func() { timer.Stop() }
}

// EnsureFetch queues or adopts the fetch of the episode at rel in lib under the
// identity pin names (bridge.EnsurePodcastFetch; nj carries the priority and
// the requester). An identity the feed replaced between the pin's read and the
// queue's check of it (bridge.ErrIdentityChanged) is asked for once more as the
// episode is now: one rule for the stream and the fetch routes (server#179).
// The same identity isn't asked again (its previous one's fetch is still
// stopping, and the writer transaction would only refuse it again): that
// refusal, or a second change, is the caller's to answer. An episode that left
// the index meanwhile is bridge.ErrNotFound, as the queue's own check says it.
func EnsureFetch(ctx context.Context, cat *catalog.Catalog, q *bridge.Service, lib int64, rel string, pin catalog.PodcastPin, nj bridge.NewJob, opt bridge.FetchOptions) (*bridge.Job, bool, error) {
	ensure := func(pin catalog.PodcastPin) (*bridge.Job, bool, error) {
		nj.LibraryID, nj.Path = &lib, &rel
		nj.Params, _ = json.Marshal(bridge.EpisodeParams{ShowID: pin.ShowID, EpisodeID: pin.EpisodeID})
		return q.EnsurePodcastFetch(ctx, nj, opt)
	}
	job, created, err := ensure(pin)
	if !errors.Is(err, bridge.ErrIdentityChanged) {
		return job, created, err
	}
	fresh, perr := cat.PodcastPin(ctx, lib, rel)
	switch {
	case errors.Is(perr, catalog.ErrNotFound):
		return nil, false, bridge.ErrNotFound
	case perr != nil:
		return nil, false, perr
	case fresh.ShowID == pin.ShowID && fresh.EpisodeID == pin.EpisodeID:
		return nil, false, err
	}
	return ensure(fresh)
}

// Fetch queues authorized work and returns an opened committed generation if it
// becomes ready. The caller has already checked the exact path and read scope.
func (d Demand) Fetch(ctx context.Context, lib catalog.Library, rel string, by int64, noNew bool) (*os.File, *feed.Fetch, bridge.FetchStatus, error) {
	wait := cmp.Or(d.Budget, StreamWait)
	deadline := d.now().Add(wait)
	// Leave a small part of the same budget for the final presence/status check.
	budget, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
	defer cancel()
	status := bridge.FetchStatus{State: "absent"}
	pin, err := d.Catalog.PodcastPin(budget, lib.ID, rel)
	if err != nil {
		if budget.Err() != nil {
			return nil, nil, bridge.UnavailableFetchStatus(bridge.FetchStatus{}, bridge.ErrKindUnavailable), nil
		}
		return nil, nil, status, err
	}
	// An identity the feed changed since the pin was read is asked for once
	// more as it is now (EnsureFetch). What is left of ErrIdentityChanged (the
	// previous identity's fetch is still stopping, or it changed again) is
	// returned as it is, no fetch status: the stream answers it by its own
	// code (server#179).
	job, _, err := EnsureFetch(budget, d.Catalog, d.Bridge, lib.ID, rel, pin,
		bridge.NewJob{Priority: bridge.PriorityPlaying, RequestedBy: by}, bridge.FetchOptions{NoNew: noNew})
	if job != nil {
		// What the job is doing now: a fetch the Bridge already holds says
		// fetching, even when the budget runs out before the next read.
		status = bridge.OpenFetchStatus(*job)
	}
	refusal := err
	if err != nil && !bridge.IsFetchRefusal(err) {
		if budget.Err() == nil {
			return nil, nil, status, err
		}
		return nil, nil, bridge.UnavailableFetchStatus(bridge.FetchStatus{}, bridge.ErrKindUnavailable), nil
	}
	for {
		wake, release := d.Bridge.WatchPodcast(lib.ID, rel)
		f, fetch, openErr := library.OpenPodcastBounded(budget, d.Catalog, lib, rel, wait)
		if openErr == nil {
			release()
			return f, fetch, bridge.ReadyFetchStatus(fetch), nil
		}
		if !errors.Is(openErr, library.ErrNotIndexable) && !errors.Is(openErr, catalog.ErrNotFound) && budget.Err() == nil {
			release()
			return nil, nil, status, openErr
		}
		// An indexed pin whose committed descriptor cannot be opened is unready.
		st, readErr := d.Bridge.UnreadyFetchStatus(budget, lib.ID, rel)
		if readErr != nil && budget.Err() == nil {
			release()
			return nil, nil, status, readErr
		}
		if readErr == nil {
			status = st
		}
		if noNew {
			status = bridge.UnavailableFetchStatus(status, bridge.ErrFetchDemo)
		}
		if refusal != nil && !errors.Is(refusal, bridge.ErrFetchCooldown) && (status.State == "absent" || (errors.Is(refusal, bridge.ErrFetchLimit) && status.State != "queued" && status.State != "fetching")) {
			status = bridge.UnavailableFetchStatus(status, refusal)
		}
		left := deadline.Sub(d.now())
		if status.State == "failed" || status.State == "unavailable" || left <= 25*time.Millisecond || wake == nil || budget.Err() != nil || ctx.Err() != nil {
			release()
			// Presence always gets one final check, including timeout and terminal wakes.
			f, fetch, err = library.OpenPodcastBounded(budget, d.Catalog, lib, rel, wait)
			if err == nil {
				return f, fetch, bridge.ReadyFetchStatus(fetch), nil
			}
			return nil, nil, status, nil
		}
		timer, stop := d.timer(left - 25*time.Millisecond)
		select {
		case <-wake:
		case <-timer:
		case <-budget.Done():
		case <-ctx.Done():
		}
		stop()
		release()
	}
}

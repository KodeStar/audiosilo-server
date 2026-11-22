// Package audible is the Audible module's poll scheduler (Bridge Phase 4a):
// while the module is on it queues each mapped account's audible.poll every
// poll_hours, and (Phase 4c) each synced account's audible.sync every 15
// minutes. The Bridge holds the sign-ins and does the polling; the
// accounts and titles are catalog's (catalog/audible.go).
package audible

// no-tests: exercised by internal/api's audible tests (SchedulerTick and the
// imports run over the API's module deps, a Bridge and its jobs).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/modules"
)

// tick is how often the scheduler looks for due polls.
const tick = time.Minute

// maxPerTick bounds the polls one pass queues.
const maxPerTick = 20

// RunScheduler is the module's Start: a pass at once, then every minute, until
// ctx ends (the module turned off, or the server stopping).
func RunScheduler(ctx context.Context, d modules.Deps) {
	if d.Bridge == nil || d.Catalog == nil {
		return
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if err := SchedulerTick(ctx, d, time.Now()); err != nil && ctx.Err() == nil {
			d.Log.Warn("audible scheduling failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SchedulerTick first cancels orphan jobs, even with the Bridge offline.
// When an online Bridge runs audible.poll,
// each due account's poll is queued (and when it runs audible.sync, each due
// account's sync: queueSyncs) (catalog.DueAudiblePolls: mapped to a
// library and not queued within its poll_hours), deduped per account by the
// queue (an open poll is returned, not a second one), and its queue time
// recorded. With no Bridge online, or one without Audible, nothing is queued:
// the polls fall due again once it is back.
func SchedulerTick(ctx context.Context, d modules.Deps, now time.Time) error {
	_, sweepErr := d.Catalog.CancelOrphanAudibleJobs(ctx)
	h, err := d.Bridge.Online(ctx)
	if err != nil || h == nil {
		return errors.Join(sweepErr, err)
	}
	errs := []error{sweepErr}
	if h.Offers(bridge.KindAudiblePoll) {
		errs = append(errs, queuePolls(ctx, d, now))
	}
	if h.Offers(bridge.KindAudibleSync) {
		errs = append(errs, queueSyncs(ctx, d, now))
	}
	return errors.Join(errs...)
}

// queuePolls queues each due account's poll.
func queuePolls(ctx context.Context, d modules.Deps, now time.Time) error {
	due, err := d.Catalog.DueAudiblePolls(ctx, now, maxPerTick)
	if err != nil {
		return err
	}
	var errs []error
	for _, poll := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, _, err := d.Bridge.Enqueue(ctx, bridge.AudiblePoll(poll.Account, poll.Template, bridge.PriorityScheduled)); err != nil {
			errs = append(errs, fmt.Errorf("poll audible account %s: %w", poll.Account, err))
			continue
		}
		if err := d.Catalog.MarkAudibleQueued(ctx, poll.Account); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// queueSyncs queues each due account's audible.sync (Phase 4c:
// catalog.DueAudibleSyncs, every bridge.AudibleSyncEvery per account with sync
// on, not paused or refused); an account with no synced title queues nothing
// until its next turn. The job names the account alone: the Bridge's claim
// reads its titles as they are then (bridge.Service.ClaimAudibleSync).
func queueSyncs(ctx context.Context, d modules.Deps, now time.Time) error {
	due, err := d.Catalog.DueAudibleSyncs(ctx, now, maxPerTick)
	if err != nil {
		return err
	}
	q := Queues{Catalog: d.Catalog, Bridge: d.Bridge}
	var errs []error
	for _, account := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		errs = append(errs, q.queueSync(ctx, account, false))
	}
	return errors.Join(errs...)
}

// queueSync queues account's audible.sync when it has a synced title, or
// always when empty (the Bridge's claim then tells it the set is empty), and
// records its queue time.
func (q Queues) queueSync(ctx context.Context, account string, empty bool) error {
	queue := empty
	if !queue {
		var err error
		if queue, err = q.Catalog.HasAudibleSyncTitles(ctx, account); err != nil {
			return fmt.Errorf("sync audible account %s: %w", account, err)
		}
	}
	if queue {
		if _, _, err := q.Bridge.Enqueue(ctx, bridge.AudibleSync(account, bridge.PriorityScheduled)); err != nil {
			return fmt.Errorf("sync audible account %s: %w", account, err)
		}
	}
	return q.Catalog.MarkAudibleSyncQueued(ctx, account)
}

// SyncNow queues account's audible.sync at once (Phase 4c) when the titles
// that take part in its sync may have changed (a title's "Not this book" or
// "Don't sync this book") or its sync was turned on, so the Bridge stops
// pushing a title the console just left out without waiting for the next
// turn: only while an online Bridge runs audible.sync and the account would be
// due but for its time (catalog.AudibleSyncQueueable), even with no synced
// title when titlesChanged (the Bridge is told the set is empty). The claim
// reads the titles, so an open sync the Bridge hasn't claimed yet syncs the
// new set; one it is running keeps the titles its claim gave it.
func (q Queues) SyncNow(ctx context.Context, account string, titlesChanged bool) error {
	h, err := q.Bridge.Online(ctx)
	if err != nil || h == nil || !h.Offers(bridge.KindAudibleSync) {
		return err
	}
	ok, err := q.Catalog.AudibleSyncQueueable(ctx, account)
	if err != nil || !ok {
		return err
	}
	return q.queueSync(ctx, account, titlesChanged)
}

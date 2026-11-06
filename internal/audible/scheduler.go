// Package audible is the Audible module's poll scheduler (Bridge Phase 4a):
// while the module is on it queues each mapped account's audible.poll every
// poll_hours, and (Phase 4c) each synced account's audible.sync every 15
// minutes. The Bridge holds the sign-ins and does the polling; the
// accounts and titles are catalog's (catalog/audible.go).
package audible

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/modules"
)

// tick is how often the scheduler looks for due polls.
const tick = time.Minute

// maxPerTick bounds the polls one pass queues.
const maxPerTick = 20

// schedulerPriority is a scheduled poll's job priority: under anything a
// person asked for.
const schedulerPriority = -10

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

// SchedulerTick is one pass at now: when an online Bridge runs audible.poll,
// each due account's poll is queued (and when it runs audible.sync, each due
// account's sync: queueSyncs) (catalog.DueAudiblePolls: mapped to a
// library and not queued within its poll_hours), deduped per account by the
// queue (an open poll is returned, not a second one), and its queue time
// recorded. With no Bridge online, or one without Audible, nothing is queued:
// the polls fall due again once it is back.
func SchedulerTick(ctx context.Context, d modules.Deps, now time.Time) error {
	h, err := d.Bridge.Online(ctx)
	if err != nil {
		return err
	}
	if h == nil {
		return nil
	}
	var errs []error
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
		if _, _, err := d.Bridge.Enqueue(ctx, bridge.AudiblePoll(poll.Account, poll.Template, schedulerPriority)); err != nil {
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
// on, not paused or refused), its params the account's synced titles at queue
// time; an account with none queues nothing until its next turn. A queued
// sync the Bridge hasn't claimed yet takes the newest titles (the kind
// promotes).
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
		errs = append(errs, q.queueSync(ctx, d.Log, account, false))
	}
	return errors.Join(errs...)
}

// queueSync queues account's audible.sync with its synced titles as params
// (none queues nothing unless empty, the Bridge then told the set is empty)
// and records its queue time.
func (q Queues) queueSync(ctx context.Context, log *slog.Logger, account string, empty bool) error {
	titles, err := q.Catalog.AudibleSyncTitles(ctx, account)
	if err != nil {
		return fmt.Errorf("sync audible account %s: %w", account, err)
	}
	if len(titles) > 0 || empty {
		job, dropped := bridge.AudibleSync(account, titles, schedulerPriority)
		if dropped > 0 && log != nil {
			log.Warn("audible: an account's sync leaves out titles past the params' bound", "account", account,
				"dropped", dropped)
		}
		if _, _, err := q.Bridge.Enqueue(ctx, job); err != nil {
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
// due but for its time (catalog.AudibleSyncQueueable). Its params are the
// titles now, even none when titlesChanged (the Bridge is told the set is
// empty); an open sync the Bridge hasn't claimed yet takes them (the kind
// promotes), one it is running keeps the ones it was given.
func (q Queues) SyncNow(ctx context.Context, log *slog.Logger, account string, titlesChanged bool) error {
	h, err := q.Bridge.Online(ctx)
	if err != nil || h == nil || !h.Offers(bridge.KindAudibleSync) {
		return err
	}
	ok, err := q.Catalog.AudibleSyncQueueable(ctx, account)
	if err != nil || !ok {
		return err
	}
	return q.queueSync(ctx, log, account, titlesChanged)
}

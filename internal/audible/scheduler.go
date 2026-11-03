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
	var errs []error
	for _, account := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		titles, err := d.Catalog.AudibleSyncTitles(ctx, account)
		if err != nil {
			errs = append(errs, fmt.Errorf("sync audible account %s: %w", account, err))
			continue
		}
		if len(titles) > 0 {
			job, dropped := bridge.AudibleSync(account, titles, schedulerPriority)
			if dropped > 0 && d.Log != nil {
				d.Log.Warn("audible: an account's sync leaves out titles past the params' bound", "account", account,
					"dropped", dropped)
			}
			if _, _, err := d.Bridge.Enqueue(ctx, job); err != nil {
				errs = append(errs, fmt.Errorf("sync audible account %s: %w", account, err))
				continue
			}
		}
		if err := d.Catalog.MarkAudibleSyncQueued(ctx, account); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

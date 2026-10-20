// Package podcast is the podcasts module's poll scheduler; the feed.json v1
// format and URL helpers are its feed package, the index and folding catalog's.
package podcast

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/modules"
)

// RunScheduler folds committed outcomes even while the Bridge is offline.
// Turning the module off stops both scheduling and this recovery sweep.
func RunScheduler(ctx context.Context, d modules.Deps) {
	if d.Bridge == nil {
		return
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		wake := d.Bridge.Ended()
		if err := SchedulerTick(ctx, d); err != nil && ctx.Err() == nil {
			d.Log.Warn("podcast scheduling failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-wake:
		}
	}
}

// SchedulerTick is one bounded pass, also used by deterministic scheduler tests.
// A job or show that fails is reported and passed over: the next ones still
// fold and poll, as the same one comes first again on every tick.
func SchedulerTick(ctx context.Context, d modules.Deps) error {
	jobs, err := d.Bridge.EndedPodcasts(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.Catalog.FoldPodcastJob(ctx, j); err != nil {
			errs = append(errs, fmt.Errorf("fold podcast job %d: %w", j.ID, err))
		}
	}
	h, err := d.Bridge.Online(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if h == nil || !h.Offers(bridge.KindPodcastPoll) {
		return errors.Join(errs...)
	}
	refs, err := d.Catalog.DuePodcastPolls(ctx, 10)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, r := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := queuePoll(ctx, d, r); err != nil {
			errs = append(errs, fmt.Errorf("poll library %d show %q: %w", r.LibraryID, r.Path, err))
		}
	}
	return errors.Join(errs...)
}

// queuePoll queues one due show's scheduled poll; a library the Bridge can't
// write to is retried later instead (PodcastNotWritable).
func queuePoll(ctx context.Context, d modules.Deps, r catalog.Ref) error {
	st, err := d.Catalog.PodcastState(ctx, r.LibraryID, r.Path)
	if err != nil {
		return err
	}
	_, _, err = d.Bridge.Enqueue(ctx, bridge.NewJob{Kind: bridge.KindPodcastPoll, LibraryID: &r.LibraryID, Path: &r.Path, Params: st.PollParams(), Priority: -10})
	if errors.Is(err, bridge.ErrLibraryNotWritable) {
		err = d.Catalog.PodcastNotWritable(ctx, r)
	}
	return err
}

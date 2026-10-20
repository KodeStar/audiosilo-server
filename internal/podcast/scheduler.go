package podcast

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
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
func SchedulerTick(ctx context.Context, d modules.Deps) error {
	jobs, err := d.Bridge.EndedPodcasts(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if err := d.Catalog.FoldPodcastJob(ctx, j); err != nil {
			return err
		}
	}
	h, err := d.Bridge.Online(ctx)
	if err != nil {
		return err
	}
	if h == nil || !h.Offers(bridge.KindPodcastPoll) {
		return nil
	}
	refs, err := d.Catalog.DuePodcastPolls(ctx, 10)
	if err != nil {
		return err
	}
	for _, r := range refs {
		st, err := d.Catalog.PodcastState(ctx, r.LibraryID, r.Path)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(bridge.PollParams{ETag: st.ETag, LastModified: st.LastModified})
		if err != nil {
			return err
		}
		_, _, err = d.Bridge.Enqueue(ctx, bridge.NewJob{Kind: bridge.KindPodcastPoll, LibraryID: &r.LibraryID, Path: &r.Path, Params: raw, Priority: -10})
		if errors.Is(err, bridge.ErrLibraryNotWritable) {
			err = d.Catalog.PodcastNotWritable(ctx, r)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

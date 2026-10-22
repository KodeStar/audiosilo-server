// Package podcast is the podcasts module's poll scheduler; the feed.json v1
// format and URL helpers are its feed package, the index and folding catalog's.
package podcast

import (
	"context"
	"encoding/json"
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
	if h == nil {
		return errors.Join(errs...)
	}
	var refs []catalog.Ref
	if h.Offers(bridge.KindPodcastPoll) {
		if refs, err = d.Catalog.DuePodcastPolls(ctx, 10); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	for _, r := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := queuePoll(ctx, d, r); err != nil {
			errs = append(errs, fmt.Errorf("poll library %d show %q: %w", r.LibraryID, r.Path, err))
		}
	}
	if err := scheduleEpisodes(ctx, d, h); err != nil {
		errs = append(errs, err)
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

func scheduleEpisodes(ctx context.Context, d modules.Deps, h *bridge.Heartbeat) error {
	if !h.Offers(bridge.KindPodcastFetch) && !h.Offers(bridge.KindPodcastRetain) {
		return nil
	}
	shows, err := d.Catalog.PodcastWorkShows(ctx)
	if err != nil {
		return err
	}

	fetches, retains := 0, 0
	var errs []error
	for _, show := range shows {
		if err := ctx.Err(); err != nil {
			return err
		}

		if !h.Writable(show.LibraryID) {
			continue
		}
		if h.Offers(bridge.KindPodcastFetch) && fetches < 10 {
			d.Catalog.PodcastVisited(show.Ref)
			candidates, err := d.Catalog.PodcastFetchCandidates(ctx, show)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			added := 0
			for _, ep := range candidates {
				raw, _ := json.Marshal(bridge.EpisodeParams{ShowID: show.ShowID, EpisodeID: ep.EpisodeID})
				_, created, err := d.Bridge.EnsurePodcastFetch(ctx, bridge.NewJob{Kind: bridge.KindPodcastFetch, LibraryID: &show.LibraryID, Path: &ep.Path, Params: raw, Priority: -10}, bridge.FetchOptions{})
				if err != nil {
					errs = append(errs, err)
					continue
				}
				if created {
					fetches++
					added++
				}
				if fetches == 10 || added == 2 {
					break
				}
			}
		}
		if h.Offers(bridge.KindPodcastRetain) && show.Present && show.RetainDue && retains < 10 {
			raw, _ := json.Marshal(bridge.RetainParams{ShowID: show.ShowID})
			_, created, err := d.Bridge.Enqueue(ctx, bridge.NewJob{Kind: bridge.KindPodcastRetain, LibraryID: &show.LibraryID, Path: &show.Path, Params: raw, Priority: -20})
			if err != nil {
				errs = append(errs, err)
			} else if created {
				retains++
			}
		}
		if fetches == 10 && retains == 10 {
			break
		}
	}
	return errors.Join(errs...)
}

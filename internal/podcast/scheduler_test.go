package podcast

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/modules"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func TestSchedulerDueAndWritableBackoff(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cat := catalog.New(db, func() time.Time { return now })
	queue := bridge.New(db, func() time.Time { return now })
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(1,'admin','','admin','t','t'); INSERT INTO tokens(id,user_id,token_hash,kind,created_at) VALUES(1,1,'hash','bridge','t'); INSERT INTO bridge_status(id,paired_at,paired_by) VALUES(1,'t',1)`); err != nil {
		t.Fatal(err)
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Podcasts", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.IndexPodcastShow(ctx, lib.ID, "Example Show", "s", "", "", f, nil, false); err != nil {
		t.Fatal(err)
	}
	deps := modules.Deps{Log: slog.Default(), Catalog: cat, Bridge: queue}
	if err := cat.FollowPodcast(ctx, 1, lib.ID, "Example Show", f.ShowID); err != nil {
		t.Fatal(err)
	}
	// Offline means no scheduled work, even though the feed is due.
	if err := SchedulerTick(ctx, deps); err != nil {
		t.Fatal(err)
	}
	beat := func(writable bool) {
		t.Helper()
		if err := queue.SaveHeartbeat(ctx, 1, bridge.Status{Version: "0.9.0", Kinds: []string{bridge.KindPodcastPoll}, Libraries: []bridge.LibraryStatus{{LibraryID: lib.ID, MountPath: "/podcasts", Mapped: true, Writable: writable}}}); err != nil {
			t.Fatal(err)
		}
	}
	beat(false)
	if err := SchedulerTick(ctx, deps); err != nil {
		t.Fatal(err)
	}
	show, err := cat.PodcastShow(ctx, lib.ID, "Example Show", 1)
	if err != nil || show.Status.Error == nil || show.Status.Error.Code != "library_not_writable" || show.Status.NextCheck == nil || *show.Status.NextCheck != "2026-10-20T12:30:00Z" {
		t.Fatalf("writable retry %+v %v", show, err)
	}
	beat(true)
	if err := SchedulerTick(ctx, deps); err != nil {
		t.Fatal(err)
	}
	jobs, _, err := queue.List(ctx, bridge.Filter{Kind: bridge.KindPodcastPoll})
	if err != nil || len(jobs) != 0 {
		t.Fatalf("not yet due %+v %v", jobs, err)
	}
	now = now.Add(30 * time.Minute)
	beat(true)
	if err := SchedulerTick(ctx, deps); err != nil {
		t.Fatal(err)
	}
	jobs, _, err = queue.List(ctx, bridge.Filter{Kind: bridge.KindPodcastPoll})
	if err != nil || len(jobs) != 1 || jobs[0].Priority != -10 || jobs[0].RequestedBy != nil {
		t.Fatalf("scheduled %+v %v", jobs, err)
	}
	if err := SchedulerTick(ctx, deps); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.ClaimFor(ctx, 1, nil, 0)
	if err != nil || claim == nil {
		t.Fatalf("claim %+v %v", claim, err)
	}
	done := bridge.StateDone
	_, err = queue.UpdateHeld(ctx, claim.ID, bridge.Update{Attempt: claim.Attempts, State: &done, Result: json.RawMessage(`{"status":200,"etag":"v1","next_poll_seconds":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	// The sweep folds the outcome and the following tick cannot requeue it.
	if err := SchedulerTick(ctx, deps); err != nil {
		t.Fatal(err)
	}
	show, err = cat.PodcastShow(ctx, lib.ID, "Example Show", 1)
	if err != nil || show.Status.NextCheck == nil || *show.Status.NextCheck != "2026-10-20T12:45:00Z" {
		t.Fatalf("fold clamp %+v %v", show, err)
	}
	if err := cat.UnfollowPodcast(ctx, 1, lib.ID, "Example Show"); err != nil {
		t.Fatal(err)
	}
	if err := SchedulerTick(ctx, deps); err != nil {
		t.Fatal(err)
	}
	show, _ = cat.PodcastShow(ctx, lib.ID, "Example Show", 1)
	if show.Following {
		t.Fatal("recovery sweep followed again")
	}
}

// One show whose poll can't be queued must not hold back the shows due after
// it (it comes first again on every tick).
func TestSchedulerPassesOverAFailingShow(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cat := catalog.New(db, func() time.Time { return now })
	queue := bridge.New(db, func() time.Time { return now })
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(1,'admin','','admin','t','t'); INSERT INTO tokens(id,user_id,token_hash,kind,created_at) VALUES(1,1,'hash','bridge','t'); INSERT INTO bridge_status(id,paired_at,paired_by) VALUES(1,'t',1)`); err != nil {
		t.Fatal(err)
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Podcasts", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	for _, show := range []string{"A Show", "B Show"} {
		// The fixture's episodes name another show, so these index none.
		f, err := feed.ReadFile("testdata/feed-v1.json", show, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cat.IndexPodcastShow(ctx, lib.ID, show, "s", "", "", f, nil, false); err != nil {
			t.Fatal(err)
		}
		if err := cat.FollowPodcast(ctx, 1, lib.ID, show, f.ShowID); err != nil {
			t.Fatal(err)
		}
	}
	// "A Show" is due first and holds a validator no poll may carry.
	if _, err := db.ExecContext(ctx, `INSERT INTO podcast_feed_state(library_id,show_path,etag,next_poll_at) VALUES(?1,'A Show',?2,'2026-10-20T11:00:00Z'),(?1,'B Show','','2026-10-20T11:30:00Z')`, lib.ID, strings.Repeat("x", 600)); err != nil {
		t.Fatal(err)
	}
	if err := queue.SaveHeartbeat(ctx, 1, bridge.Status{Version: "0.9.0", Kinds: []string{bridge.KindPodcastPoll}, Libraries: []bridge.LibraryStatus{{LibraryID: lib.ID, MountPath: "/podcasts", Mapped: true, Writable: true}}}); err != nil {
		t.Fatal(err)
	}
	deps := modules.Deps{Log: slog.Default(), Catalog: cat, Bridge: queue}
	if err := SchedulerTick(ctx, deps); err == nil || !strings.Contains(err.Error(), `"A Show"`) {
		t.Fatalf("refused poll not reported: %v", err)
	}
	jobs, _, err := queue.List(ctx, bridge.Filter{Kind: bridge.KindPodcastPoll})
	if err != nil || len(jobs) != 1 || jobs[0].Path == nil || *jobs[0].Path != "B Show" {
		t.Fatalf("polls %+v %v", jobs, err)
	}
}

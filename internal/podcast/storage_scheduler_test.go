package podcast

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/modules"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func TestPodcastSchedulerBoundsFairnessAndPolicies(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	cat := catalog.New(db, func() time.Time { return now })
	queue := bridge.New(db, func() time.Time { return now })
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(1,'admin','','admin','t','t'); INSERT INTO tokens(id,user_id,token_hash,kind,created_at) VALUES(1,1,'hash','bridge','t'); INSERT INTO bridge_status(id,paired_at,paired_by) VALUES(1,'t',1)`); err != nil {
		t.Fatal(err)
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Pods", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if err = queue.SaveHeartbeat(ctx, 1, bridge.Status{Version: "0.9.0", Kinds: []string{bridge.KindPodcastFetch, bridge.KindPodcastRetain}, Libraries: []bridge.LibraryStatus{{LibraryID: lib.ID, MountPath: "/podcasts", Mapped: true, Writable: true}}}); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		show := fmt.Sprintf("Show %d", i)
		f, err := feed.ReadFile("testdata/phase2c2/feed-fetch-v1.json", "Fixture Show", nil)
		if err != nil {
			t.Fatal(err)
		}
		f.ShowID = fmt.Sprintf("%016x", i+1)
		for j := range f.Episodes {
			f.Episodes[j].Path = path.Join(show, path.Base(f.Episodes[j].Path))
			f.Episodes[j].Fetch = nil
		}
		if _, err = cat.IndexPodcastShow(ctx, lib.ID, show, "s", "", "", f, nil, true); err != nil {
			t.Fatal(err)
		}
		if i != 7 {
			if err = cat.FollowPodcast(ctx, 1, lib.ID, show, f.ShowID); err != nil {
				t.Fatal(err)
			}
		}
		policy := catalog.Policy{Mode: "archive", Keep: 3, Days: 7}
		if i == 6 {
			policy.Mode = "on_demand"
		}
		if err = cat.SetPodcastPolicy(ctx, lib.ID, show, f.ShowID, &policy); err != nil {
			t.Fatal(err)
		}
	}
	deps := modules.Deps{Log: slog.Default(), Catalog: cat, Bridge: queue}
	if err = SchedulerTick(ctx, deps); err != nil {
		t.Fatal(err)
	}
	var total, maxPerShow int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch'`).Scan(&total); err != nil || total != 10 {
		t.Fatalf("pass cap: %d %v", total, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT max(n) FROM (SELECT count(*) n FROM bridge_jobs GROUP BY substr(rel_path,1,6))`).Scan(&maxPerShow); err != nil || maxPerShow != 2 {
		t.Fatalf("show cap: %d %v", maxPerShow, err)
	}
	// Archive chooses oldest first. The second pass starts with the previously
	// unvisited show, and existing open rows consume no quota.
	var first string
	if err = db.QueryRowContext(ctx, `SELECT rel_path FROM bridge_jobs ORDER BY id LIMIT 1`).Scan(&first); err != nil || first != "Show 0/2026-10-17-c0d697452a.mp3" {
		t.Fatalf("archive order: %s %v", first, err)
	}
	if err = SchedulerTick(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE rel_path LIKE 'Show 5/%'`).Scan(&total); err != nil || total != 2 {
		t.Fatalf("rotation: %d %v", total, err)
	}
	for _, show := range []string{"Show 6", "Show 7"} {
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE substr(rel_path,1,6)=?`, show).Scan(&total); err != nil || total != 0 {
			t.Fatalf("no automatic work: %s %d %v", show, total, err)
		}
	}
}

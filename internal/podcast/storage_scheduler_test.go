package podcast

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/modules"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store"
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
		f, err := feed.ReadFile("testdata/phase2c2", "testdata/phase2c2/feed-fetch-v1.json", "Fixture Show", nil)
		if err != nil {
			t.Fatal(err)
		}
		f.ShowID = fmt.Sprintf("%016x", i+1)
		for j := range f.Episodes {
			f.Episodes[j].Path = path.Join(show, path.Base(f.Episodes[j].Path))
			f.Episodes[j].Fetch = nil
		}
		if _, err = cat.IndexPodcastShow(ctx, lib.ID, show, "s", "", "", f, nil, true, false); err != nil {
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

// refusedShowEnv is a podcasts library whose indexed show "Fixture Show" has
// no episode downloaded and a folder the share refuses the server for good
// (dir, made chmod 000, made readable at cleanup): an admin (1) and a
// listener (2), a Bridge online that fetches and writes the library. A test
// that uses it is not parallel, for the chmod; root reads a folder whatever
// its mode, so it skips there.
type refusedShowEnv struct {
	ctx   context.Context
	db    *store.DB
	cat   *catalog.Catalog
	queue *bridge.Service
	lib   *catalog.Library
	f     *feed.File
	dir   string
}

func refusedShow(t *testing.T) refusedShowEnv {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder whatever its mode")
	}
	e := refusedShowEnv{ctx: context.Background(), db: storetest.Open(t)}
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	e.cat = catalog.New(e.db, func() time.Time { return now })
	e.queue = bridge.New(e.db, func() time.Time { return now })
	if _, err := e.db.ExecContext(e.ctx, `INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(1,'admin','','admin','t','t'),(2,'listener','','user','t','t'); INSERT INTO tokens(id,user_id,token_hash,kind,created_at) VALUES(1,1,'hash','bridge','t'); INSERT INTO bridge_status(id,paired_at,paired_by) VALUES(1,'t',1)`); err != nil {
		t.Fatal(err)
	}
	var err error
	if e.lib, err = e.cat.CreateLibrary(e.ctx, catalog.Library{Name: "Pods", Root: t.TempDir(), Kind: catalog.KindPodcasts}); err != nil {
		t.Fatal(err)
	}
	if err = e.queue.SaveHeartbeat(e.ctx, 1, bridge.Status{Version: "0.9.0", Kinds: []string{bridge.KindPodcastFetch}, Libraries: []bridge.LibraryStatus{{LibraryID: e.lib.ID, MountPath: "/podcasts", Mapped: true, Writable: true}}}); err != nil {
		t.Fatal(err)
	}
	if e.f, err = feed.ReadFile("testdata/phase2c2", "testdata/phase2c2/feed-fetch-v1.json", "Fixture Show", nil); err != nil {
		t.Fatal(err)
	}
	for j := range e.f.Episodes {
		e.f.Episodes[j].Fetch = nil
	}
	if _, err = e.cat.IndexPodcastShow(e.ctx, e.lib.ID, "Fixture Show", "s", "", "", e.f, nil, true, false); err != nil {
		t.Fatal(err)
	}
	e.dir = filepath.Join(e.lib.Root, "Fixture Show")
	if err = os.Mkdir(e.dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.dir, 0o755) })
	return e
}

// fetches is how many episode fetches are queued, for path alone when given.
func (e refusedShowEnv) fetches(t *testing.T, path string) (n int) {
	t.Helper()
	if err := e.db.QueryRowContext(e.ctx, `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch' AND (?1='' OR rel_path=?1)`, path).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSchedulerPassesOverARefusedShow: a followed show whose folder the share
// refuses the server for good (chmod 000) gets no scheduled fetch
// (library.ShowRefused, the fetch routes' check): the Bridge's file would sit
// behind the same refusal, the episode never indexed as there, so every tick
// would download it again. Allowed: the folder readable again, its episodes
// are fetched.
func TestSchedulerPassesOverARefusedShow(t *testing.T) {
	e := refusedShow(t)
	if err := e.cat.FollowPodcast(e.ctx, 1, e.lib.ID, "Fixture Show", e.f.ShowID); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.SetPodcastPolicy(e.ctx, e.lib.ID, "Fixture Show", e.f.ShowID, &catalog.Policy{Mode: "archive", Keep: 3, Days: 7}); err != nil {
		t.Fatal(err)
	}
	deps := modules.Deps{Log: slog.Default(), Catalog: e.cat, Bridge: e.queue}
	if err := SchedulerTick(e.ctx, deps); err != nil {
		t.Fatal(err)
	}
	if n := e.fetches(t, ""); n != 0 {
		t.Fatalf("%d fetches scheduled in a show the share refuses, want none", n)
	}
	if err := os.Chmod(e.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SchedulerTick(e.ctx, deps); err != nil {
		t.Fatal(err)
	}
	if n := e.fetches(t, ""); n == 0 {
		t.Fatal("no fetch scheduled once the show's folder reads")
	}
}

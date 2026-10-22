package podcast

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func TestNextPodcastScopedQueue(t *testing.T) {
	current := catalog.Ref{LibraryID: 1, Path: "Show/current.mp3"}
	next := catalog.Ref{LibraryID: 1, Path: "Show/next.mp3"}
	book := &catalog.Book{}
	queue := []catalog.ListItem{{Ref: catalog.Ref{LibraryID: 2, Path: "Book"}, Book: book}, {Ref: current, Book: book}, {Ref: catalog.Ref{LibraryID: 2, Path: "Other"}, Book: book}, {Ref: next, Book: book}}
	podcasts := map[int64]bool{1: true}
	if got, ok := NextPodcast(queue, current, podcasts); !ok || got != next {
		t.Fatal(got, ok)
	}
	if got, ok := NextPodcast(queue, catalog.Ref{}, podcasts); !ok || got != current {
		t.Fatal(got, ok)
	}
	if _, ok := NextPodcast(queue, next, podcasts); ok {
		t.Fatal("wrapped queue")
	}
}

func TestPrefetchSkipsFetchLimitAndRetriesAfterSlotFreed(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	jobs := bridge.New(db, time.Now)
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(1,'admin','','admin','t','t'),(2,'listener','','user','t','t'); INSERT INTO tokens(id,user_id,token_hash,kind,created_at) VALUES(1,1,'bridgehash','bridge','t'); INSERT INTO bridge_status(id,paired_at,paired_by) VALUES(1,'t',1)`); err != nil {
		t.Fatal(err)
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Pods", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if err = jobs.SaveHeartbeat(ctx, 1, bridge.Status{Kinds: []string{bridge.KindPodcastFetch}, Libraries: []bridge.LibraryStatus{{LibraryID: lib.ID, Mapped: true, Writable: true}}}); err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("testdata/phase2c2/feed-fetch-v1.json", "Fixture Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	template := f.Episodes[0]
	f.Episodes = nil
	for i := range 6 {
		ep := template
		ep.ID, ep.Path, ep.Fetch = fmt.Sprintf("%016x", i+1), fmt.Sprintf("Fixture Show/limit-%d.mp3", i), nil
		f.Episodes = append(f.Episodes, ep)
	}
	if _, err = cat.IndexPodcastShow(ctx, lib.ID, "Fixture Show", "s", "", "", f, nil, true); err != nil {
		t.Fatal(err)
	}
	var first int64
	for i := range 5 {
		ep := f.Episodes[i]
		raw, _ := json.Marshal(bridge.EpisodeParams{ShowID: f.ShowID, EpisodeID: ep.ID})
		j, _, err := jobs.EnsurePodcastFetch(ctx, bridge.NewJob{LibraryID: &lib.ID, Path: &ep.Path, Params: raw, Priority: 100, RequestedBy: 2}, bridge.FetchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = j.ID
		}
	}
	if err = cat.GrantWholeLibrary(ctx, 2, lib.ID); err != nil {
		t.Fatal(err)
	}
	who := catalog.Caller{ID: 2, Podcasts: true}
	scopes, err := cat.CallerScopes(ctx, who)
	if err != nil {
		t.Fatal(err)
	}
	current := catalog.Ref{LibraryID: lib.ID, Path: f.Episodes[0].Path}
	next := catalog.Ref{LibraryID: lib.ID, Path: f.Episodes[5].Path}
	if err = cat.SetQueue(ctx, 2, []catalog.Ref{current, next}, scopes); err != nil {
		t.Fatal(err)
	}
	p := &Prefetcher{Catalog: cat, Bridge: jobs, Context: ctx}
	done := p.Start(who, current, true)
	defer done()
	p.work.Wait()
	var n int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE rel_path=?`, next.Path).Scan(&n); err != nil || n != 0 {
		t.Fatalf("prefetch exceeded cap: %d, %v", n, err)
	}
	if _, err = jobs.Cancel(ctx, first); err != nil {
		t.Fatal(err)
	}
	p.Reconsider(who)
	p.work.Wait()
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE rel_path=? AND priority=10 AND requested_by=2`, next.Path).Scan(&n); err != nil || n != 1 {
		t.Fatalf("free slot not prefetched: %d, %v", n, err)
	}
}

func TestPrefetchScopesPromotionAndMutationCap(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cat := catalog.New(db, func() time.Time { return now })
	jobs := bridge.New(db, func() time.Time { return now })
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(1,'admin','','admin','t','t'),(2,'listener','','user','t','t'); INSERT INTO tokens(id,user_id,token_hash,kind,created_at) VALUES(1,1,'bridgehash','bridge','t'); INSERT INTO bridge_status(id,paired_at,paired_by) VALUES(1,'t',1)`); err != nil {
		t.Fatal(err)
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Pods", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if err = jobs.SaveHeartbeat(ctx, 1, bridge.Status{Kinds: []string{bridge.KindPodcastFetch}, Libraries: []bridge.LibraryStatus{{LibraryID: lib.ID, Mapped: true, Writable: true}}}); err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("testdata/phase2c2/feed-fetch-v1.json", "Fixture Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range f.Episodes {
		f.Episodes[i].Fetch = nil
	}
	if _, err = cat.IndexPodcastShow(ctx, lib.ID, "Fixture Show", "s", "", "", f, nil, true); err != nil {
		t.Fatal(err)
	}
	private := *f
	private.Private = true
	private.ShowID = "1111111111111111"
	private.Episodes = append([]feed.Episode(nil), f.Episodes...)
	for i := range private.Episodes {
		private.Episodes[i].Path = path.Join("Private", path.Base(private.Episodes[i].Path))
	}
	if _, err = cat.IndexPodcastShow(ctx, lib.ID, "Private", "s", "", "", &private, nil, true); err != nil {
		t.Fatal(err)
	}
	if err = cat.GrantWholeLibrary(ctx, 2, lib.ID); err != nil {
		t.Fatal(err)
	}
	who := catalog.Caller{ID: 2, Podcasts: true}
	scopes, err := cat.CallerScopes(ctx, who)
	if err != nil {
		t.Fatal(err)
	}
	current := catalog.Ref{LibraryID: lib.ID, Path: f.Episodes[0].Path}
	candidate := catalog.Ref{LibraryID: lib.ID, Path: f.Episodes[1].Path}
	third := catalog.Ref{LibraryID: lib.ID, Path: f.Episodes[2].Path}
	hidden := catalog.Ref{LibraryID: lib.ID, Path: private.Episodes[0].Path}
	// An old stored private entry is filtered with this caller's current proof.
	if _, err = db.ExecContext(ctx, `INSERT INTO up_next(user_id,library_id,rel_path,position,added_at) VALUES(2,?,?,0,'t'),(2,?,?,1,'t'),(2,?,?,2,'t')`, lib.ID, current.Path, lib.ID, hidden.Path, lib.ID, candidate.Path); err != nil {
		t.Fatal(err)
	}
	p := &Prefetcher{Catalog: cat, Bridge: jobs, Context: ctx}
	done := p.Start(who, current, true)
	defer done()
	p.work.Wait()
	var id int64
	var priority int
	var requester int64
	if err = db.QueryRowContext(ctx, `SELECT id,priority,requested_by FROM bridge_jobs WHERE kind='podcast.fetch' AND rel_path=?`, candidate.Path).Scan(&id, &priority, &requester); err != nil || priority != 10 || requester != 2 {
		t.Fatal(id, priority, requester, err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE rel_path=?`, hidden.Path).Scan(&n); err != nil || n != 0 {
		t.Fatal("private prefetched", n, err)
	}
	pin, err := cat.PodcastPin(ctx, lib.ID, candidate.Path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(bridge.EpisodeParams{ShowID: pin.ShowID, EpisodeID: pin.EpisodeID})
	promoted, created, err := jobs.EnsurePodcastFetch(ctx, bridge.NewJob{LibraryID: &lib.ID, Path: &candidate.Path, Params: raw, Priority: 100}, bridge.FetchOptions{})
	if err != nil || created || promoted.ID != id || promoted.Priority != 100 {
		t.Fatalf("promote %+v %v", promoted, err)
	}
	if err = cat.SetQueue(ctx, 2, []catalog.Ref{current, third}, scopes); err != nil {
		t.Fatal(err)
	}
	p.Reconsider(who)
	p.work.Wait()
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("cap", n, err)
	}
	j, err := jobs.Job(ctx, id)
	if err != nil || j.CancelRequested {
		t.Fatal("queue removal cancelled work", err)
	}
	// A different current with an empty queue is reconsidered after mutation.
	if err = cat.SetQueue(ctx, 2, nil, scopes); err != nil {
		t.Fatal(err)
	}
	done2 := p.Start(who, third, true)
	defer done2()
	p.work.Wait()
	if err = cat.SetQueue(ctx, 2, []catalog.Ref{third, current}, scopes); err != nil {
		t.Fatal(err)
	}
	p.Reconsider(who)
	p.work.Wait()
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch' AND rel_path=? AND priority=10`, current.Path).Scan(&n); err != nil || n != 1 {
		t.Fatal("mutation", n, err)
	}
	// Old clients and revoked scope create no preparation work.
	if err = cat.SetQueue(ctx, 2, []catalog.Ref{candidate, third}, scopes); err != nil {
		t.Fatal(err)
	}
	who.Podcasts = false
	done3 := p.Start(who, candidate, true)
	defer done3()
	p.work.Wait()
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch' AND rel_path=?`, third.Path).Scan(&n); err != nil || n != 0 {
		t.Fatal("old-client prefetched", n, err)
	}
	// A finished look is not repeated for each Range request of the same item;
	// a queue mutation looks again.
	if _, err = db.ExecContext(ctx, `DELETE FROM bridge_jobs`); err != nil {
		t.Fatal(err)
	}
	who.Podcasts = true
	if err = cat.SetQueue(ctx, 2, []catalog.Ref{third}, scopes); err != nil {
		t.Fatal(err)
	}
	p2 := &Prefetcher{Catalog: cat, Bridge: jobs, Context: ctx}
	done4 := p2.Start(who, third, true)
	defer done4()
	p2.work.Wait()
	if err = cat.SetQueue(ctx, 2, []catalog.Ref{third, current}, scopes); err != nil {
		t.Fatal(err)
	}
	done5 := p2.Start(who, third, true)
	defer done5()
	p2.work.Wait()
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE rel_path=?`, current.Path).Scan(&n); err != nil || n != 0 {
		t.Fatal("settled item looked again", n, err)
	}
	p2.Reconsider(who)
	p2.work.Wait()
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE rel_path=? AND priority=10`, current.Path).Scan(&n); err != nil || n != 1 {
		t.Fatal("mutation after settle", n, err)
	}
	// An open scheduled intent for the next episode is raised to the prefetch
	// priority, never duplicated.
	if _, err = db.ExecContext(ctx, `DELETE FROM bridge_jobs`); err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := jobs.EnsurePodcastFetch(ctx, bridge.NewJob{LibraryID: &lib.ID, Path: &candidate.Path, Params: raw, Priority: -10}, bridge.FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = cat.SetQueue(ctx, 2, []catalog.Ref{third, candidate}, scopes); err != nil {
		t.Fatal(err)
	}
	p3 := &Prefetcher{Catalog: cat, Bridge: jobs, Context: ctx}
	done6 := p3.Start(who, third, true)
	defer done6()
	p3.work.Wait()
	if j, err = jobs.Job(ctx, scheduled.ID); err != nil || j.Priority != 10 {
		t.Fatal("scheduled candidate not raised", j, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE rel_path=?`, candidate.Path).Scan(&n); err != nil || n != 1 {
		t.Fatal("scheduled candidate duplicated", n, err)
	}
	// A removed feed entry next in the queue is not prepared automatically.
	if _, err = db.ExecContext(ctx, `DELETE FROM bridge_jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE podcast_episodes SET removed_at='2026-10-01T00:00:00Z' WHERE book_id=(SELECT id FROM books WHERE library_id=? AND rel_path=?)`, lib.ID, candidate.Path); err != nil {
		t.Fatal(err)
	}
	p4 := &Prefetcher{Catalog: cat, Bridge: jobs, Context: ctx}
	done7 := p4.Start(who, third, true)
	defer done7()
	p4.work.Wait()
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE rel_path=?`, candidate.Path).Scan(&n); err != nil || n != 0 {
		t.Fatal("removed entry prefetched", n, err)
	}
}

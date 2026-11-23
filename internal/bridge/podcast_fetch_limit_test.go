package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func fetchLimitJobs(t *testing.T, q *testQueue, n int) []NewJob {
	t.Helper()
	ctx := context.Background()
	jobs := make([]NewJob, n)
	for i := range jobs {
		path := fmt.Sprintf("Show/limit-%d.mp3", i)
		id := fmt.Sprintf("%016x", i+1)
		for _, stmt := range []string{ // one statement an Exec (see fetchQueue)
			`INSERT INTO books(library_id,rel_path,title,indexed_at) VALUES(?1,?2,'Episode','t')`,
			`INSERT INTO podcast_episodes(book_id,library_id,show_path,episode_id,guid,episode_type,explicit,feed_duration,present,generation,removed_at,duration_source)
			 SELECT id,library_id,'Show',?3,'','full',0,0,0,0,'','none' FROM books WHERE library_id=?1 AND rel_path=?2`,
		} {
			if _, err := q.db.ExecContext(ctx, stmt, q.lib, path, id); err != nil {
				t.Fatal(err)
			}
		}
		raw, _ := json.Marshal(EpisodeParams{ShowID: "0123456789abcdef", EpisodeID: id})
		jobs[i] = NewJob{LibraryID: &q.lib, Path: &path, Params: raw, Priority: 10, RequestedBy: q.admin}
	}
	return jobs
}

func fetchLimitListener(t *testing.T, q *testQueue, name string, kid bool) int64 {
	t.Helper()
	var member any
	if kid {
		member = "kid"
	}
	r, err := q.db.ExecContext(context.Background(), `INSERT INTO users(username,password_hash,role,member_kind,created_at,updated_at) VALUES(?,'','user',?,'t','t')`, name, member)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := r.LastInsertId()
	return id
}

// Both capped counts seek their kind's partial index by requester, not the
// retained history of every job.
func TestOpenRequestsPlan(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	for _, tc := range []struct{ kind, states, index string }{
		{KindPodcastFetch, workingStatesSQL, "idx_bridge_jobs_fetch_requester (requested_by=?)"},
		{KindPodcastSubscribe, OpenStatesSQL, "idx_bridge_jobs_subscribe_user (requested_by=?)"},
	} {
		plan := strings.Join(queryPlan(t, q, openRequestsSQL(tc.kind, tc.states), q.admin), "\n")
		if !strings.Contains(plan, tc.index) {
			t.Errorf("%s cap did not seek %s:\n%s", tc.kind, tc.index, plan)
		}
	}
}

func TestPodcastFetchLimitCountsCreatorOpenIntents(t *testing.T) {
	for _, kid := range []bool{false, true} {
		t.Run(fmt.Sprint("kid=", kid), func(t *testing.T) {
			q := fetchQueue(t)
			ctx := context.Background()
			by := fetchLimitListener(t, q, "listener", kid)
			jobs := fetchLimitJobs(t, q, 19)
			spare := jobs[18] // the listener's, after the freed slot
			jobs = jobs[:18]
			var owned, parked *Job
			for i := range 5 {
				jobs[i].RequestedBy = by
				j, created, err := q.EnsurePodcastFetch(ctx, jobs[i], FetchOptions{})
				if err != nil || !created {
					t.Fatalf("allowed %d: %+v, %v", i, j, err)
				}
				if i == 0 {
					owned = j
				}
				if i == 3 {
					parked = j
				}
				// Every working state counts, stopping work included (a parked
				// one doesn't: below).
				state := []string{StateQueued, StateClaimed, StateRunning, StateRunning, StateRunning}[i]
				if _, err = q.db.ExecContext(ctx, `UPDATE bridge_jobs SET state=?,cancel_requested=? WHERE id=?`, state, i == 4, j.ID); err != nil {
					t.Fatal(err)
				}
			}
			jobs[5].RequestedBy = by
			for _, retry := range []bool{false, true} {
				j, created, err := q.EnsurePodcastFetch(ctx, jobs[5], FetchOptions{Retry: retry})
				if !errors.Is(err, ErrFetchLimit) || created || j != nil {
					t.Fatalf("denied retry=%v: %+v, %v", retry, j, err)
				}
			}
			jobs[0].Priority = 100
			j, created, err := q.EnsurePodcastFetch(ctx, jobs[0], FetchOptions{Retry: true})
			if err != nil || created || j.ID != owned.ID || j.Priority != 100 || j.RequestedBy.ID != by {
				t.Fatalf("own dedupe at cap %+v, %v", j, err)
			}
			// Admin and scheduler work are exempt, including new intents while
			// this listener is capped. Their work never becomes listener-owned.
			for i := 6; i < len(jobs); i++ {
				if i%2 == 0 {
					jobs[i].RequestedBy = 0
				}
				j, created, err = q.EnsurePodcastFetch(ctx, jobs[i], FetchOptions{})
				if err != nil || !created {
					t.Fatalf("exempt %+v, %v", j, err)
				}
			}
			jobs[6].RequestedBy, jobs[6].Priority = by, 100
			j, created, err = q.EnsurePodcastFetch(ctx, jobs[6], FetchOptions{})
			if err != nil || created || j.Priority != 100 || j.RequestedBy != nil {
				t.Fatalf("household promotion at cap %+v, %v", j, err)
			}
			// A terminal row frees a slot, regardless of retained history.
			if _, err = q.db.ExecContext(ctx, `UPDATE bridge_jobs SET state='done' WHERE id=?`, owned.ID); err != nil {
				t.Fatal(err)
			}
			if j, created, err = q.EnsurePodcastFetch(ctx, jobs[5], FetchOptions{}); err != nil || !created {
				t.Fatalf("freed slot %+v, %v", j, err)
			}
			// At the cap again; a parked fetch (the Bridge waits on something
			// the listener can't change) frees its slot (server#179).
			spare.RequestedBy = by
			if j, _, err = q.EnsurePodcastFetch(ctx, spare, FetchOptions{}); !errors.Is(err, ErrFetchLimit) {
				t.Fatalf("at the cap again %+v, %v", j, err)
			}
			if _, err = q.db.ExecContext(ctx, `UPDATE bridge_jobs SET state=? WHERE id=?`, StateNeedsAttention, parked.ID); err != nil {
				t.Fatal(err)
			}
			if j, created, err = q.EnsurePodcastFetch(ctx, spare, FetchOptions{}); err != nil || !created {
				t.Fatalf("parked slot %+v, %v", j, err)
			}
		})
	}
}

func TestPodcastFetchLimitConcurrentDistinctPaths(t *testing.T) {
	q := fetchQueue(t)
	ctx := context.Background()
	by := fetchLimitListener(t, q, "listener", false)
	jobs := fetchLimitJobs(t, q, 20)
	var wg sync.WaitGroup
	results := make(chan error, len(jobs))
	start := make(chan struct{})
	for _, job := range jobs {
		job.RequestedBy = by
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := q.EnsurePodcastFetch(ctx, job, FetchOptions{Retry: true})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	allowed, denied := 0, 0
	for err := range results {
		switch {
		case err == nil:
			allowed++
		case errors.Is(err, ErrFetchLimit):
			denied++
		default:
			t.Fatal(err)
		}
	}
	var n int
	if err := q.db.QueryRowContext(ctx, `SELECT count(*) FROM bridge_jobs WHERE kind='podcast.fetch' AND requested_by=?`, by).Scan(&n); err != nil || n != 5 || allowed != 5 || denied != 15 {
		t.Fatalf("cap: stored=%d allowed=%d denied=%d, %v", n, allowed, denied, err)
	}
	// The cap belongs to each creator, not the household or library.
	other := fetchLimitListener(t, q, "other", false)
	job := demandJob(q, 100)
	job.RequestedBy = other
	if j, created, err := q.EnsurePodcastFetch(ctx, job, FetchOptions{}); err != nil || !created || j.RequestedBy.ID != other {
		t.Fatalf("other creator %+v, %v", j, err)
	}
}

func TestPodcastFetchLimitAcrossLibraries(t *testing.T) {
	q := fetchQueue(t)
	ctx := context.Background()
	by := fetchLimitListener(t, q, "listener", false)
	for _, job := range fetchLimitJobs(t, q, 5) {
		job.RequestedBy = by
		if _, _, err := q.EnsurePodcastFetch(ctx, job, FetchOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := q.db.ExecContext(ctx, `INSERT INTO libraries(name,root,kind,created_at) VALUES('Other','/other','podcasts','t')`)
	if err != nil {
		t.Fatal(err)
	}
	other := *q
	other.lib, _ = r.LastInsertId()
	if _, err = q.db.ExecContext(ctx, `INSERT INTO podcast_shows(library_id,show_path,show_id,title,author,description,link,language,show_type,explicit,private,feed_url,feed_host,feed_url_sha256,podcast_guid,cover_path,episode_count,latest_published,feed_sig,indexed_at) VALUES(?1,'Show','0123456789abcdef','Title','','','','','episodic',0,0,'','','','','',1,NULL,'s','t')`, other.lib); err != nil {
		t.Fatal(err)
	}
	if err = q.SaveHeartbeat(ctx, q.token, Status{Kinds: []string{KindPodcastFetch}, Libraries: []LibraryStatus{{LibraryID: q.lib, Mapped: true, Writable: true}, {LibraryID: other.lib, Mapped: true, Writable: true}}}); err != nil {
		t.Fatal(err)
	}
	job := fetchLimitJobs(t, &other, 1)[0]
	job.RequestedBy = by
	if _, created, err := q.EnsurePodcastFetch(ctx, job, FetchOptions{Retry: true}); !errors.Is(err, ErrFetchLimit) || created {
		t.Fatalf("other library escaped cap: created=%v, %v", created, err)
	}
}

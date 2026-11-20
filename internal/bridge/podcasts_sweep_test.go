package bridge

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"
)

// BenchmarkEndedPodcasts times the podcast scheduler's sweep for ended jobs
// not yet folded (EndedPodcasts, every minute and at every job's end) in a
// library whose Bridge has fetched 20,000 episodes, each fetch folded but
// the last ten (server#180).
//
//	go test ./internal/bridge -run '^$' -bench BenchmarkEndedPodcasts
func BenchmarkEndedPodcasts(b *testing.B) {
	ctx := context.Background()
	q := newTestQueue(b)
	const fetched, unfolded = 20000, 10
	finished := q.now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	err := q.db.WithTx(ctx, "seed", func(tx *sql.Tx) error {
		for i := range fetched {
			at := finished
			if i >= fetched-unfolded {
				at = q.now.Format(time.RFC3339)
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO bridge_jobs(kind,library_id,rel_path,state,dedupe_key,created_at,updated_at,finished_at) VALUES('podcast.fetch',?,?,'done',?,?,?,?)`,
				q.lib, fmt.Sprintf("Show/%05d.mp3", i), fmt.Sprintf("fetch-%d", i), at, at, at)
			if err != nil {
				return err
			}
			if i < fetched-unfolded {
				id, _ := res.LastInsertId()
				if _, err := tx.ExecContext(ctx, `INSERT INTO podcast_folded(job_id,at) VALUES(?,?)`, id, at); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		jobs, err := q.EndedPodcasts(ctx)
		if err != nil || len(jobs) != unfolded {
			b.Fatalf("swept %d: %v", len(jobs), err)
		}
	}
}

// An ended fetch not yet folded is swept at the first sweep however long ago
// it ended, and again at each sweep FoldSweepEvery after the last full one;
// between them only one that ended within FoldSweepEvery is (server#180).
// A poll is swept while it ended within PollFoldWindow, at every sweep.
func TestEndedPodcastsSweepsOldFetchesHourly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	end := func(kind, path string, ago time.Duration) int64 {
		t.Helper()
		at := q.now.Add(-ago).Format(time.RFC3339)
		res, err := q.db.ExecContext(ctx, `INSERT INTO bridge_jobs(kind,library_id,rel_path,state,dedupe_key,created_at,updated_at,finished_at) VALUES(?,?,?,'done',?,?,?,?)`,
			kind, q.lib, path, kind+path, at, at, at)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	swept := func() []int64 {
		t.Helper()
		jobs, err := q.EndedPodcasts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ids := []int64{}
		for _, j := range jobs {
			ids = append(ids, j.ID)
		}
		return ids
	}
	old := end(KindPodcastFetch, "Show/old.mp3", 3*24*time.Hour)
	poll := end(KindPodcastPoll, "Show", 2*time.Hour)
	if got := swept(); !slices.Equal(got, []int64{old, poll}) {
		t.Fatalf("first sweep %v, want the old fetch and the poll", got)
	}
	recent := end(KindPodcastFetch, "Show/recent.mp3", time.Minute)
	q.advance(30 * time.Minute)
	if got := swept(); !slices.Equal(got, []int64{poll, recent}) {
		t.Fatalf("sweep within the hour %v, want the poll and the recent fetch", got)
	}
	q.advance(FoldSweepEvery)
	if got := swept(); !slices.Equal(got, []int64{old, poll, recent}) {
		t.Fatalf("sweep an hour on %v, want every unfolded job", got)
	}
}

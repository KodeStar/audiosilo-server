package bridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

// An idle claim (nothing queued for the asked kinds, no lease run out) answers
// from the reader pool: it neither takes the writer nor moves the write
// generation, the long poll's wait included. A queued job of another kind, a
// held one (its module off) and a revoked token are idle too.
func TestIdleClaimLeavesGenerationAlone(t *testing.T) {
	t.Parallel()
	q := podcastQueue(t)
	ctx := context.Background()
	idle := func(what string, kinds []string, wantErr error) {
		t.Helper()
		gen := q.db.Generation()
		j, err := q.claim(ctx, q.token, kinds)
		if j != nil || !errors.Is(err, wantErr) {
			t.Fatalf("%s: claim = %+v, %v; want nil, %v", what, j, err, wantErr)
		}
		if got := q.db.Generation(); got != gen {
			t.Fatalf("%s: generation %d -> %d; an idle claim wrote", what, gen, got)
		}
	}
	idle("empty queue", []string{KindVerify, KindPodcastPoll}, nil)

	gen := q.db.Generation()
	if j, err := q.waitClaim(ctx, q.token, []string{KindVerify}, 50*time.Millisecond); j != nil || err != nil {
		t.Fatalf("waiting claim = %+v, %v", j, err)
	}
	if got := q.db.Generation(); got != gen {
		t.Fatalf("a waiting claim moved the generation %d -> %d", gen, got)
	}

	if _, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/feed.xml", false, ""); err != nil {
		t.Fatal(err)
	}
	idle("a job of another kind queued", []string{KindPodcastPoll}, nil)

	if _, _, err := q.Enqueue(ctx, NewJob{Kind: KindPodcastPoll, LibraryID: &q.lib, Path: ptr("Show"), Params: []byte(`{}`), RequestedBy: q.admin}); err != nil {
		t.Fatal(err)
	}
	q.ModuleOn = func(string) bool { return false }
	idle("a held job", []string{KindPodcastPoll}, nil)
	q.ModuleOn = nil

	if _, err := q.db.ExecContext(ctx, `UPDATE tokens SET revoked = 1 WHERE id = ?`, q.token); err != nil {
		t.Fatal(err)
	}
	idle("a revoked token", []string{KindVerify}, ErrNotPaired)
}

// claimDue lets every claim with work through to the writer: a queued job of
// an asked kind is claimed, and a lease that ran out is ended by any claim (one
// asking other kinds too), its job queued again for the next.
func TestClaimDueLetsWorkThrough(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	job := q.verify(t, "A", 0)
	gen, durable := q.db.Generation(), q.db.DurableCommits()
	j, err := q.claim(ctx, q.token, []string{KindVerify})
	if err != nil || j == nil || j.ID != job.ID || j.State != StateClaimed || j.Attempts != 1 {
		t.Fatalf("claim of a queued job = %+v, %v", j, err)
	}
	if q.db.Generation() == gen {
		t.Fatal("a claim that leased a job did not move the generation")
	}
	// Its attempt number must survive a power cut (never handed out twice).
	if got := q.db.DurableCommits() - durable; got != 1 {
		t.Fatalf("a claim that leased a job made %d durable commits, want 1", got)
	}
	idleGen, durable := q.db.Generation(), q.db.DurableCommits()
	if j, err := q.claim(ctx, q.token, []string{KindVerify}); j != nil || err != nil || q.db.Generation() != idleGen || q.db.DurableCommits() != durable {
		t.Fatalf("claim with the only job held = %+v, %v (generation %d -> %d)", j, err, idleGen, q.db.Generation())
	}

	q.advance(Lease + time.Second)
	gen = q.db.Generation()
	if j, err := q.claim(ctx, q.token, []string{KindDescribe}); j != nil || err != nil {
		t.Fatalf("claim of another kind = %+v, %v", j, err)
	}
	if q.db.Generation() == gen {
		t.Fatal("a claim with a lapsed lease due did not write")
	}
	if got, err := q.Job(ctx, job.ID); err != nil || got.State != StateQueued || got.LeaseUntil != nil {
		t.Fatalf("lapsed job after another kind's claim = %+v, %v; want queued again", got, err)
	}
	j, err = q.claim(ctx, q.token, []string{KindVerify})
	if err != nil || j == nil || j.ID != job.ID || j.Attempts != 2 {
		t.Fatalf("claim of the requeued job = %+v, %v", j, err)
	}
}

// A private follow request whose feed URL a restart lost is failed by the next
// claim, even one asking other kinds: claimDue finds it due.
func TestClaimDueFailsLostPrivateRequest(t *testing.T) {
	t.Parallel()
	q := podcastQueue(t)
	ctx := context.Background()
	j, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/rss?secret=SECRET123", true, "")
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(q.db, func() time.Time { return *q.now })
	if claim, err := restarted.claim(ctx, q.token, []string{KindPodcastPoll}); claim != nil || err != nil {
		t.Fatalf("restarted claim = %+v, %v", claim, err)
	}
	got, err := restarted.Job(ctx, j.ID)
	if err != nil || got.State != StateFailed {
		t.Fatalf("lost private request = %+v, %v; want failed", got, err)
	}
}

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestReleaseUnsentClaimRestoresAttemptAndWakes(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()
	queued := q.verify(t, "Author/Book", 0)
	for range MaxAttempts + 1 {
		claim, err := q.ClaimFor(ctx, q.token, nil, 0)
		if err != nil || claim == nil || claim.ID != queued.ID || claim.Attempts != 1 {
			t.Fatalf("claim %+v, %v", claim, err)
		}
		wake := q.wake.wait()
		if err = q.ReleaseUnsentClaim(ctx, claim); err != nil {
			t.Fatal(err)
		}
		job, err := q.Job(ctx, queued.ID)
		if err != nil || job.State != StateQueued || job.Attempts != 0 || job.LeaseUntil != nil || job.ClaimedAt != nil {
			t.Fatalf("released %+v, %v", job, err)
		}
		select {
		case <-wake:
		default:
			t.Fatal("waiting claims were not woken")
		}
	}
}

func TestReleaseUnsentClaimPreservesSpentAttempts(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()
	queued := q.verify(t, "Author/Book", 0)
	if _, err := q.db.ExecContext(ctx, `UPDATE bridge_jobs SET attempts=2 WHERE id=?`, queued.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := q.ClaimFor(ctx, q.token, nil, 0)
	if err != nil || claim == nil || claim.Attempts != MaxAttempts {
		t.Fatalf("claim %+v, %v", claim, err)
	}
	if err = q.ReleaseUnsentClaim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	job, err := q.Job(ctx, queued.ID)
	if err != nil || job.State != StateQueued || job.Attempts != 2 || job.LeaseUntil != nil {
		t.Fatalf("spent attempts changed: %+v, %v", job, err)
	}
}

func TestReleaseUnsentClaimPreservesReportedLease(t *testing.T) {
	for _, state := range []string{StateRunning, StateDone, StateFailed, StateNeedsAttention, "acknowledged"} {
		t.Run(state, func(t *testing.T) {
			q := newTestQueue(t)
			ctx := context.Background()
			queued := q.verify(t, "Author/Book", 0)
			claim, err := q.ClaimFor(ctx, q.token, nil, 0)
			if err != nil || claim == nil {
				t.Fatalf("claim %+v, %v", claim, err)
			}
			u := Update{Attempt: claim.Attempts}
			if state != "acknowledged" {
				u.State = &state
			}
			if state == StateNeedsAttention {
				u.Progress = json.RawMessage(`{"park":{"code":"sign_in"}}`)
			}
			before, err := q.UpdateHeld(ctx, queued.ID, u)
			if err != nil {
				t.Fatal(err)
			}
			if err = q.ReleaseUnsentClaim(ctx, claim); err != nil {
				t.Fatal(err)
			}
			after, err := q.Job(ctx, queued.ID)
			if err != nil || after.State != before.State || after.Attempts != before.Attempts || after.UpdatedAt != before.UpdatedAt {
				t.Fatalf("reported lease changed: %+v, %v", after, err)
			}
		})
	}
}

func TestReleaseUnsentClaimPreservesNewAttempt(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()
	queued := q.verify(t, "Author/Book", 0)
	old, err := q.ClaimFor(ctx, q.token, nil, 0)
	if err != nil || old == nil {
		t.Fatal(err)
	}
	q.advance(Lease + time.Second)
	current, err := q.ClaimFor(ctx, q.token, nil, 0)
	if err != nil || current == nil || current.Attempts != 2 {
		t.Fatalf("new claim %+v, %v", current, err)
	}
	if err = q.ReleaseUnsentClaim(ctx, old); err != nil {
		t.Fatal(err)
	}
	got, err := q.Job(ctx, queued.ID)
	if err != nil || got.State != StateClaimed || got.Attempts != 2 {
		t.Fatalf("new attempt changed: %+v, %v", got, err)
	}
}

func TestReleaseUnsentClaimHonorsCancellation(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()
	queued := q.verify(t, "Author/Book", 0)
	claim, err := q.ClaimFor(ctx, q.token, nil, 0)
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	if _, err = q.Cancel(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	if err = q.ReleaseUnsentClaim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	got, err := q.Job(ctx, queued.ID)
	if err != nil || got.State != StateCancelled || got.Attempts != 0 || got.FinishedAt == nil {
		t.Fatalf("cancelled claim %+v, %v", got, err)
	}
}

// A claim the server released as undelivered may still have reached the
// Bridge (it read the reply, then the connection dropped before the server saw
// it flushed): cancelled then, or while queued again, its report hears reason
// cancelled, not a lost job (bridge#35), though the release gave the attempt
// back. A job queued again and not cancelled, or claimed again and cancelled
// under the later claim, is plain not held for an older claim.
func TestReleasedClaimHearsCancel(t *testing.T) {
	cancel := func(q *testQueue, ctx context.Context, id int64) error {
		_, err := q.Cancel(ctx, id)
		return err
	}
	for _, tc := range []struct {
		name          string
		before, after func(q *testQueue, ctx context.Context, id int64) error
		cancelled     bool
	}{
		{name: "cancelled while held", before: cancel, cancelled: true},
		{name: "cancelled once queued again", after: cancel, cancelled: true},
		{name: "queued again"},
		{name: "claimed again and cancelled", after: func(q *testQueue, ctx context.Context, id int64) error {
			// Claimed again twice (the first lease lapsing): the released
			// claim is now two attempts behind, then cancelled under the
			// later claim.
			for range 2 {
				if c, err := q.ClaimFor(ctx, q.token, nil, 0); err != nil || c == nil {
					return fmt.Errorf("claim again %+v, %v", c, err)
				}
				q.advance(Lease + time.Second)
			}
			return cancel(q, ctx, id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newTestQueue(t)
			ctx := context.Background()
			queued := q.verify(t, "Author/Book", 0)
			claim, err := q.ClaimFor(ctx, q.token, nil, 0)
			if err != nil || claim == nil {
				t.Fatalf("claim %+v, %v", claim, err)
			}
			for i, step := range []func(q *testQueue, ctx context.Context, id int64) error{
				tc.before, func(q *testQueue, ctx context.Context, _ int64) error { return q.ReleaseUnsentClaim(ctx, claim) }, tc.after,
			} {
				if step == nil {
					continue
				}
				if err := step(q, ctx, queued.ID); err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
			}
			_, err = q.UpdateHeld(ctx, queued.ID, Update{Attempt: claim.Attempts, State: ptr(StateRunning)})
			if tc.cancelled != errors.Is(err, ErrCancelledWhileHeld) || !errors.Is(err, ErrNotHeld) {
				t.Fatalf("the released claim's report: %v, want ErrNotHeld, cancelled %v", err, tc.cancelled)
			}
		})
	}
}

// failUnsentClaim ends an untouched claim failed with its reason; a cancel
// asked for meanwhile (an admin's, or the server's own with its reason) ends it
// cancelled instead, keeping that reason; a claim the Bridge reported on, or
// one a lapsed lease handed out again, is left as it is.
func TestFailUnsentClaim(t *testing.T) {
	const reason = "the claim could not be filled"
	for _, tc := range []struct {
		name      string
		meanwhile func(q *testQueue, ctx context.Context, claim *Job) (*Job, error)
		state     string
		attempts  int
		err       string
	}{
		{name: "untouched", state: StateFailed, attempts: 1, err: reason},
		{name: "cancel asked", state: StateCancelled, attempts: 1,
			meanwhile: func(q *testQueue, ctx context.Context, claim *Job) (*Job, error) {
				_, err := q.Cancel(ctx, claim.ID)
				return claim, err
			}},
		{name: "reported", state: StateRunning, attempts: 1,
			meanwhile: func(q *testQueue, ctx context.Context, claim *Job) (*Job, error) {
				_, err := q.UpdateHeld(ctx, claim.ID, Update{Attempt: claim.Attempts, State: ptr(StateRunning)})
				return claim, err
			}},
		{name: "claimed again", state: StateClaimed, attempts: 2,
			meanwhile: func(q *testQueue, ctx context.Context, claim *Job) (*Job, error) {
				q.advance(Lease + time.Second)
				_, err := q.ClaimFor(ctx, q.token, nil, 0)
				return claim, err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newTestQueue(t)
			ctx := context.Background()
			queued := q.verify(t, "Author/Book", 0)
			claim, err := q.ClaimFor(ctx, q.token, nil, 0)
			if err != nil || claim == nil {
				t.Fatalf("claim %+v, %v", claim, err)
			}
			if tc.meanwhile != nil {
				if claim, err = tc.meanwhile(q, ctx, claim); err != nil {
					t.Fatal(err)
				}
			}
			if err := q.failUnsentClaim(ctx, claim, reason); err != nil {
				t.Fatal(err)
			}
			got, err := q.Job(ctx, queued.ID)
			if err != nil || got.State != tc.state || got.Attempts != tc.attempts || got.Error != tc.err {
				t.Fatalf("after = %+v, %v; want %s after %d attempts, error %q", got, err, tc.state, tc.attempts, tc.err)
			}
		})
	}
}

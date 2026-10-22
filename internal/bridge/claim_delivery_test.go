package bridge

import (
	"context"
	"encoding/json"
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

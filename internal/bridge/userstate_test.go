package bridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// An online Bridge's heartbeat with its last status is user state (server#258,
// PERF-SPIKE B2b): it leaves the content generation while still recording when
// the Bridge was heard. A new status, or coming back after the online window,
// moves it as any write.
func TestHeartbeatContentGeneration(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	q.beat(t, KindVerify)
	stamp := func() time.Time {
		t.Helper()
		h, err := readHeartbeat(ctx, q.db)
		if err != nil || h == nil {
			t.Fatalf("heartbeat = %+v, %v", h, err)
		}
		return h.At
	}
	check := func(what string, moved bool, beat func()) {
		t.Helper()
		gen, at := q.db.ContentGeneration(), stamp()
		beat()
		if got := q.db.ContentGeneration() != gen; got != moved {
			t.Fatalf("%s: content generation moved %v, want %v", what, got, moved)
		}
		if stamp().Equal(at) {
			t.Fatalf("%s: heartbeat_at not updated", what)
		}
	}
	q.advance(30 * time.Second)
	check("unchanged heartbeat", false, func() { q.beat(t, KindVerify) })
	q.advance(30 * time.Second)
	check("another unchanged heartbeat", false, func() { q.beat(t, KindVerify) })
	q.advance(30 * time.Second)
	check("new kinds", true, func() { q.beat(t, KindVerify, KindDescribe) })
	q.advance(30 * time.Second)
	beatV2 := func() {
		if err := q.SaveHeartbeat(ctx, q.token, Status{Version: "0.2.0", Kinds: []string{KindVerify, KindDescribe}}); err != nil {
			t.Fatal(err)
		}
	}
	check("a new version", true, beatV2)
	q.advance(OnlineWindow + time.Minute)
	check("back after the online window", true, beatV2)
	if h, err := q.Online(ctx); err != nil || h == nil || h.Version != "0.2.0" {
		t.Fatalf("online heartbeat = %+v, %v", h, err)
	}
}

// A job report that only renews the lease is user state; one that changes the
// job's progress, state or result moves the content generation (B2b).
func TestLeaseRenewalContentGeneration(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	job := q.verify(t, "A", 0)
	j, err := q.claim(ctx, q.token, []string{KindVerify})
	if err != nil || j == nil || j.ID != job.ID {
		t.Fatalf("claim = %+v, %v", j, err)
	}
	report := func(what string, moved bool, u Update) *Job {
		t.Helper()
		gen := q.db.ContentGeneration()
		u.Attempt = j.Attempts
		got, err := q.UpdateHeld(ctx, job.ID, u)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if m := q.db.ContentGeneration() != gen; m != moved {
			t.Fatalf("%s: content generation moved %v, want %v", what, m, moved)
		}
		return got
	}
	running := StateRunning
	step1 := json.RawMessage(`{"step":1}`)
	report("start running", true, Update{State: &running, Progress: step1})
	q.advance(10 * time.Second)
	before, _ := q.Job(ctx, job.ID)
	after := report("keepalive", false, Update{})
	if before.LeaseUntil == nil || after.LeaseUntil == nil || *after.LeaseUntil <= *before.LeaseUntil {
		t.Fatalf("lease %v -> %v, want it renewed", before.LeaseUntil, after.LeaseUntil)
	}
	q.advance(10 * time.Second)
	report("the same progress again", false, Update{State: &running, Progress: step1})
	q.advance(10 * time.Second)
	got := report("new progress", true, Update{Progress: json.RawMessage(`{"step":2}`)})
	if string(got.Progress) != `{"step":2}` {
		t.Fatalf("progress = %s", got.Progress)
	}
	done := StateDone
	report("done", true, Update{State: &done, Result: json.RawMessage(`{}`)})
}

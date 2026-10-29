package bridge

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// A Bridge's heartbeat is user state (server#258, PERF-SPIKE B2b), whatever
// its status says: no content cache reads bridge_status, and while an ASR
// worker is attached every beat's status differs (workers[].last_seen). It
// still records when the Bridge was heard and what it sent, and GET /server's
// view of it (Online) follows at once.
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
	check := func(what string, st Status) {
		t.Helper()
		q.advance(30 * time.Second)
		gen, at := q.db.ContentGeneration(), stamp()
		if err := q.SaveHeartbeat(ctx, q.token, st); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := q.db.ContentGeneration(); got != gen {
			t.Fatalf("%s: content generation moved %d -> %d", what, gen, got)
		}
		if stamp().Equal(at) {
			t.Fatalf("%s: heartbeat_at not updated", what)
		}
		stored, err := readHeartbeat(ctx, q.db)
		if err != nil || stored == nil || !reflect.DeepEqual(stored.Status, st) {
			t.Fatalf("%s: stored %+v, %v, want %+v", what, stored, err, st)
		}
		if h, err := q.Online(ctx); err != nil || h == nil || !reflect.DeepEqual(h.Status, st) {
			t.Fatalf("%s: online heartbeat = %+v, %v, want %+v", what, h, err, st)
		}
	}
	verify := Status{Version: "0.1.0", Kinds: []string{KindVerify}}
	check("unchanged heartbeat", verify)
	worker := func(seen string) Status {
		st := verify
		st.Workers = []Worker{{Name: "mac", Version: "0.1.0", Platform: "darwin", LastSeen: seen}}
		return st
	}
	check("a worker attached", worker("2026-10-15T12:01:00Z"))
	check("the worker's last_seen", worker("2026-10-15T12:01:30Z"))
	check("new kinds", Status{Version: "0.1.0", Kinds: []string{KindVerify, KindDescribe}})
	check("a new version", Status{Version: "0.2.0", Kinds: []string{KindVerify, KindDescribe}})
	q.advance(OnlineWindow + time.Minute)
	if h, err := q.Online(ctx); err != nil || h != nil {
		t.Fatalf("after the online window: online = %+v, %v, want offline", h, err)
	}
	check("back after the online window", Status{Version: "0.2.0", Kinds: []string{KindVerify, KindDescribe}})
}

// A job report that renews the lease or reports progress (its message and
// park included) is user state; one that changes the job's state, result or
// error moves the content generation (B2b).
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
	got := report("new progress", false, Update{Progress: json.RawMessage(`{"step":2,"message":"Transcribing"}`)})
	if string(got.Progress) != `{"step":2,"message":"Transcribing"}` {
		t.Fatalf("progress = %s", got.Progress)
	}
	q.advance(10 * time.Second)
	report("progress with the state it has", false, Update{State: &running, Progress: json.RawMessage(`{"step":3}`)})
	q.advance(10 * time.Second)
	parked := StateNeedsAttention
	report("a park", true, Update{State: &parked, Progress: json.RawMessage(`{"park":{"code":"agent_rate_limited"}}`)})
	q.advance(10 * time.Second)
	report("a parked job's progress", false, Update{State: &parked, Progress: json.RawMessage(`{"step":4}`)})
	q.advance(10 * time.Second)
	report("back to running", true, Update{State: &running, Progress: json.RawMessage(`{"step":5}`)})
	q.advance(10 * time.Second)
	errText := "a step failed, retrying"
	report("an error", true, Update{Error: &errText})
	done := StateDone
	report("done", true, Update{State: &done, Result: json.RawMessage(`{}`)})
}

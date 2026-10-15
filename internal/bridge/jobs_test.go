package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// testQueue is a Service on a clock the test moves, with one admin and one
// library to point jobs at.
type testQueue struct {
	*Service
	db    *store.DB
	now   *time.Time
	admin int64
	lib   int64
}

func newTestQueue(t *testing.T) *testQueue {
	t.Helper()
	ctx := context.Background()
	db := storetest.Open(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	q := &testQueue{Service: New(db, func() time.Time { return now }), db: db, now: &now}
	res, err := db.ExecContext(ctx, `INSERT INTO users(username, password_hash, role, created_at, updated_at) VALUES('boss', '', 'admin', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	q.admin, _ = res.LastInsertId()
	res, err = db.ExecContext(ctx, `INSERT INTO libraries(name, root, created_at) VALUES('Books', '/books', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	q.lib, _ = res.LastInsertId()
	return q
}

func (q *testQueue) advance(d time.Duration) { *q.now = q.now.Add(d) }

func (q *testQueue) verify(t *testing.T, path string, priority int) *Job {
	t.Helper()
	j := NewJob{Kind: KindVerify, LibraryID: &q.lib, RequestedBy: q.admin, Priority: priority}
	if path != "" {
		j.Path = &path
	}
	if err := CheckNew(&j); err != nil {
		t.Fatal(err)
	}
	job, created, err := q.Enqueue(context.Background(), j)
	if err != nil || !created {
		t.Fatalf("Enqueue = %v, %v, %v", job, created, err)
	}
	return job
}

func ptr[T any](v T) *T { return &v }

func TestEnqueueDedupesOpenJobs(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	first := q.verify(t, "Author/Book", 0)
	if first.State != StateQueued || first.RequestedBy == nil || first.RequestedBy.Username != "boss" || string(first.Params) != `{"hold_seconds":0}` {
		t.Fatalf("queued = %+v", first)
	}
	again, created, err := q.Enqueue(ctx, NewJob{Kind: KindVerify, LibraryID: &q.lib, Path: ptr("Author/Book"), Params: first.Params})
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("second press = %v, %v, %v; want the first job", again, created, err)
	}
	// Another book, or the library itself, is a job of its own.
	other := q.verify(t, "Author/Other", 0)
	whole := q.verify(t, "", 0)
	if other.ID == first.ID || whole.ID == first.ID || whole.Path != nil {
		t.Fatalf("distinct jobs = %d %d %d", first.ID, other.ID, whole.ID)
	}
	// Once the first is finished, a new press queues a new job.
	if _, err := q.Cancel(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if again := q.verify(t, "Author/Book", 0); again.ID == first.ID {
		t.Fatal("a finished job was returned as open")
	}
}

func TestCheckNew(t *testing.T) {
	t.Parallel()
	lib := int64(1)
	for name, j := range map[string]NewJob{
		"unknown kind":         {Kind: "sidecars.generate", LibraryID: &lib},
		"no library":           {Kind: KindVerify},
		"unknown param":        {Kind: KindVerify, LibraryID: &lib, Params: json.RawMessage(`{"rm":"-rf"}`)},
		"hold out of range":    {Kind: KindVerify, LibraryID: &lib, Params: json.RawMessage(`{"hold_seconds":601}`)},
		"params not object":    {Kind: KindVerify, LibraryID: &lib, Params: json.RawMessage(`[1]`)},
		"params trailing junk": {Kind: KindVerify, LibraryID: &lib, Params: json.RawMessage(`{} {}`)},
	} {
		if err := CheckNew(&j); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: CheckNew = %v, want ErrInvalid", name, err)
		}
	}
	ok := NewJob{Kind: KindVerify, LibraryID: &lib, Params: json.RawMessage(`{"hold_seconds":5}`)}
	if err := CheckNew(&ok); err != nil || string(ok.Params) != `{"hold_seconds":5}` {
		t.Errorf("valid job: %v, %s", err, ok.Params)
	}
}

func TestClaimOrderAndKinds(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	low := q.verify(t, "A", 0)
	high := q.verify(t, "B", 5)
	if j, err := q.Claim(ctx, []string{"podcast.fetch"}); err != nil || j != nil {
		t.Fatalf("claim of a kind with no jobs = %v, %v", j, err)
	}
	j, err := q.Claim(ctx, []string{KindVerify})
	if err != nil || j.ID != high.ID || j.State != StateClaimed || j.Attempts != 1 || j.LeaseUntil == nil {
		t.Fatalf("first claim = %+v, %v; want the high priority job, claimed", j, err)
	}
	j, err = q.Claim(ctx, []string{KindVerify})
	if err != nil || j.ID != low.ID {
		t.Fatalf("second claim = %+v, %v", j, err)
	}
	if j, err := q.Claim(ctx, []string{KindVerify}); err != nil || j != nil {
		t.Fatalf("empty queue claim = %v, %v", j, err)
	}
}

func TestWaitClaimWakesOnEnqueue(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	got := make(chan *Job, 1)
	go func() {
		j, _ := q.WaitClaim(context.Background(), []string{KindVerify}, MaxWait)
		got <- j
	}()
	time.Sleep(50 * time.Millisecond)
	queued := q.verify(t, "A", 0)
	select {
	case j := <-got:
		if j == nil || j.ID != queued.ID {
			t.Fatalf("woken claim = %+v", j)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting claim never woke")
	}
}

// A Bridge that dies mid-job loses nothing: the lease runs out and the job is
// queued again, until its last attempt fails it.
func TestLeaseExpiry(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	job := q.verify(t, "A", 0)
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		j, err := q.Claim(ctx, []string{KindVerify})
		if err != nil || j == nil || j.Attempts != attempt {
			t.Fatalf("claim %d = %+v, %v", attempt, j, err)
		}
		// Progress renews the lease.
		q.advance(Lease - time.Second)
		if _, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: attempt, State: ptr(StateRunning)}); err != nil {
			t.Fatalf("progress %d: %v", attempt, err)
		}
		q.advance(Lease - time.Second)
		if err := q.ExpireLeases(ctx); err != nil {
			t.Fatal(err)
		}
		if j, _ := q.Get(ctx, job.ID); j.State != StateRunning {
			t.Fatalf("renewed lease expired: %s", j.State)
		}
		// Then the Bridge goes quiet.
		q.advance(2 * time.Second)
		jobs, err := q.List(ctx, Filter{})
		if err != nil {
			t.Fatal(err)
		}
		want := StateQueued
		if attempt == MaxAttempts {
			want = StateFailed
		}
		if jobs[0].State != want {
			t.Fatalf("after lease %d ran out: %s, want %s", attempt, jobs[0].State, want)
		}
		// The dead claim's late report is refused.
		if _, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: attempt, State: ptr(StateDone)}); !errors.Is(err, ErrNotHeld) {
			t.Fatalf("late report %d: %v, want ErrNotHeld", attempt, err)
		}
	}
	if j, _ := q.Get(ctx, job.ID); j.FinishedAt == nil || j.Error == "" {
		t.Fatalf("failed job = %+v", j)
	}
}

func TestUpdateHeld(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	job := q.verify(t, "A", 0)
	if _, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: 0, State: ptr(StateRunning)}); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("report on a queued job: %v, want ErrNotHeld", err)
	}
	if _, err := q.Claim(ctx, []string{KindVerify}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: 2, State: ptr(StateRunning)}); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("wrong attempt: %v, want ErrNotHeld", err)
	}
	for name, u := range map[string]Update{
		"back to queued":   {Attempt: 1, State: ptr(StateQueued)},
		"progress array":   {Attempt: 1, Progress: json.RawMessage(`[1]`)},
		"result not json":  {Attempt: 1, Result: json.RawMessage(`nope`)},
		"error with a nul": {Attempt: 1, Error: ptr("bad\x00")},
	} {
		if _, err := q.UpdateHeld(ctx, job.ID, u); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	j, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: 1, State: ptr(StateRunning), Progress: json.RawMessage(`{"stage":"stat","percent":50}`)})
	if err != nil || j.State != StateRunning || string(j.Progress) != `{"stage":"stat","percent":50}` {
		t.Fatalf("progress = %+v, %v", j, err)
	}
	// Parking drops the lease; the job stays the Bridge's.
	j, err = q.UpdateHeld(ctx, job.ID, Update{Attempt: 1, State: ptr(StateNeedsAttention), Error: ptr("needs a look")})
	if err != nil || j.LeaseUntil != nil || j.State != StateNeedsAttention {
		t.Fatalf("park = %+v, %v", j, err)
	}
	q.advance(time.Hour)
	if err := q.ExpireLeases(ctx); err != nil {
		t.Fatal(err)
	}
	j, err = q.UpdateHeld(ctx, job.ID, Update{Attempt: 1, State: ptr(StateDone), Result: json.RawMessage(`{"ok":true}`)})
	if err != nil || j.State != StateDone || j.FinishedAt == nil || j.LeaseUntil != nil || string(j.Progress) != `{"stage":"stat","percent":50}` {
		t.Fatalf("done = %+v, %v", j, err)
	}
	if _, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: 1, State: ptr(StateRunning)}); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("report after done: %v, want ErrNotHeld", err)
	}
}

func TestCancel(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	queued := q.verify(t, "A", 0)
	j, err := q.Cancel(ctx, queued.ID)
	if err != nil || j.State != StateCancelled || j.FinishedAt == nil {
		t.Fatalf("cancel queued = %+v, %v", j, err)
	}
	if _, err := q.Cancel(ctx, queued.ID); !errors.Is(err, ErrFinished) {
		t.Fatalf("cancel twice: %v, want ErrFinished", err)
	}
	if _, err := q.Cancel(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown: %v, want ErrNotFound", err)
	}

	running := q.verify(t, "B", 0)
	if _, err := q.Claim(ctx, []string{KindVerify}); err != nil {
		t.Fatal(err)
	}
	j, err = q.Cancel(ctx, running.ID)
	if err != nil || j.State != StateClaimed || !j.CancelRequested {
		t.Fatalf("cancel claimed = %+v, %v; want a request", j, err)
	}
	// The Bridge hears it on its next report and stops.
	j, err = q.UpdateHeld(ctx, running.ID, Update{Attempt: 1, State: ptr(StateRunning)})
	if err != nil || !j.CancelRequested {
		t.Fatalf("report after cancel = %+v, %v", j, err)
	}
	if j, err = q.UpdateHeld(ctx, running.ID, Update{Attempt: 1, State: ptr(StateCancelled)}); err != nil || j.State != StateCancelled {
		t.Fatalf("bridge stops = %+v, %v", j, err)
	}

	// A cancelled job whose Bridge died is cancelled when its lease runs out,
	// not queued again.
	orphan := q.verify(t, "C", 0)
	if _, err := q.Claim(ctx, []string{KindVerify}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Cancel(ctx, orphan.ID); err != nil {
		t.Fatal(err)
	}
	q.advance(Lease + time.Second)
	if err := q.ExpireLeases(ctx); err != nil {
		t.Fatal(err)
	}
	if j, _ := q.Get(ctx, orphan.ID); j.State != StateCancelled {
		t.Fatalf("orphaned cancel = %s, want cancelled", j.State)
	}
}

func TestListFilters(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	a := q.verify(t, "A", 0)
	b := q.verify(t, "B", 0)
	if _, err := q.Cancel(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err := q.List(ctx, Filter{Path: ptr("A")})
	if err != nil || len(jobs) != 1 || jobs[0].ID != a.ID {
		t.Fatalf("by path = %+v, %v", jobs, err)
	}
	jobs, err = q.List(ctx, Filter{State: StateQueued})
	if err != nil || len(jobs) != 1 || jobs[0].ID != b.ID {
		t.Fatalf("by state = %+v, %v", jobs, err)
	}
	jobs, err = q.List(ctx, Filter{Limit: 1})
	if err != nil || len(jobs) != 1 || jobs[0].ID != b.ID {
		t.Fatalf("first page = %+v, %v", jobs, err)
	}
	jobs, err = q.List(ctx, Filter{Limit: 1, Before: b.ID})
	if err != nil || len(jobs) != 1 || jobs[0].ID != a.ID {
		t.Fatalf("next page = %+v, %v", jobs, err)
	}
}

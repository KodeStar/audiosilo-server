package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
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
	// token is the paired Bridge's token (its tokens row id).
	token int64
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
	q.pair(t)
	q.beat(t, KindVerify)
	return q
}

// pair stands in for auth's pairing: a live bridge token of the admin and the
// bridge_status row.
func (q *testQueue) pair(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	res, err := q.db.ExecContext(ctx, `INSERT INTO tokens(user_id, token_hash, kind, created_at) VALUES(?, 'h', 'bridge', '2026-01-01T00:00:00Z')`, q.admin)
	if err != nil {
		t.Fatal(err)
	}
	q.token, _ = res.LastInsertId()
	if _, err := q.db.ExecContext(ctx, `INSERT INTO bridge_status(id, paired_at, paired_by) VALUES(1, '2026-01-01T00:00:00Z', ?)`, q.admin); err != nil {
		t.Fatal(err)
	}
}

// beat records a heartbeat naming kinds.
func (q *testQueue) beat(t *testing.T, kinds ...string) {
	t.Helper()
	st := Status{Version: "0.1.0", Kinds: kinds}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveHeartbeat(context.Background(), q.token, st); err != nil {
		t.Fatal(err)
	}
}

func (q *testQueue) advance(d time.Duration) { *q.now = q.now.Add(d) }

func (q *testQueue) verify(t *testing.T, path string, priority int) *Job {
	t.Helper()
	j := NewJob{Kind: KindVerify, LibraryID: &q.lib, RequestedBy: q.admin, Priority: priority}
	if path != "" {
		j.Path = &path
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
	if first.State != StateQueued || first.RequestedBy == nil || first.RequestedBy.Username != "boss" || string(first.Params) != `{}` {
		t.Fatalf("queued = %+v", first)
	}
	again, created, err := q.Enqueue(ctx, NewJob{Kind: KindVerify, LibraryID: &q.lib, Path: ptr("Author/Book"), Params: first.Params, RequestedBy: q.admin})
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

func TestEnqueueChecks(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	lib := q.lib
	for name, j := range map[string]NewJob{
		"unknown kind":         {Kind: "files.delete", LibraryID: &lib},
		"no library":           {Kind: KindVerify},
		"unknown param":        {Kind: KindVerify, LibraryID: &lib, Params: json.RawMessage(`{"rm":"-rf"}`)},
		"hold out of range":    {Kind: KindVerify, LibraryID: &lib, Params: json.RawMessage(`{"hold_seconds":601}`)},
		"params not object":    {Kind: KindVerify, LibraryID: &lib, Params: json.RawMessage(`[1]`)},
		"params trailing junk": {Kind: KindVerify, LibraryID: &lib, Params: json.RawMessage(`{} {}`)},
	} {
		if _, _, err := q.Enqueue(ctx, j); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Enqueue = %v, want ErrInvalid", name, err)
		}
	}
	job, _, err := q.Enqueue(ctx, NewJob{Kind: KindVerify, LibraryID: &lib, RequestedBy: q.admin, Params: json.RawMessage(`{"hold_seconds":5}`)})
	if err != nil || string(job.Params) != `{"hold_seconds":5}` {
		t.Errorf("valid job: %v, %+v", err, job)
	}
	// A kind the server knows but the Bridge doesn't run, or no Bridge at all.
	q.beat(t)
	q.advance(heartbeatTTL)
	if _, _, err := q.Enqueue(ctx, NewJob{Kind: KindVerify, LibraryID: &lib}); !errors.Is(err, ErrKindUnavailable) {
		t.Errorf("unoffered kind: %v, want ErrKindUnavailable", err)
	}
}

// The heartbeat is the paired Bridge's only while its token is live: revoking
// it (or demoting its admin) ends it, within the cache's few seconds.
func TestHeartbeatNeedsALiveToken(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	if h, err := q.LastHeartbeat(ctx); err != nil || h == nil || !h.Offers(KindVerify) {
		t.Fatalf("allowed: heartbeat = %+v, %v", h, err)
	}
	if _, err := q.db.ExecContext(ctx, `UPDATE users SET role = 'user'`); err != nil {
		t.Fatal(err)
	}
	if h, _ := q.LastHeartbeat(ctx); h == nil {
		t.Fatal("the cache should still answer within its TTL")
	}
	q.advance(heartbeatTTL)
	if h, err := q.LastHeartbeat(ctx); err != nil || h != nil {
		t.Fatalf("denied: a demoted admin's Bridge = %+v, %v", h, err)
	}
	if h, _ := q.Online(ctx); h != nil {
		t.Fatal("a demoted admin's Bridge is online")
	}
	if _, err := q.db.ExecContext(ctx, `UPDATE users SET role = 'admin'`); err != nil {
		t.Fatal(err)
	}
	q.Forget()
	if h, _ := q.LastHeartbeat(ctx); h == nil {
		t.Fatal("Forget didn't drop the cached miss")
	}
}

// ClaimFor takes the heartbeat's known kinds, narrowed to the ones asked for.
func TestClaimFor(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	job := q.verify(t, "A", 0)
	if j, err := q.ClaimFor(ctx, q.token, []string{"sidecars.generate"}, MaxWait); err != nil || j != nil {
		t.Fatalf("a kind not asked for = %+v, %v", j, err)
	}
	j, err := q.ClaimFor(ctx, q.token, nil, 0)
	if err != nil || j == nil || j.ID != job.ID {
		t.Fatalf("no narrowing = %+v, %v", j, err)
	}
	if _, err := q.db.ExecContext(ctx, `UPDATE bridge_status SET heartbeat_at = NULL`); err != nil {
		t.Fatal(err)
	}
	q.Forget()
	if _, err := q.ClaimFor(ctx, q.token, nil, 0); !errors.Is(err, ErrNoHeartbeat) {
		t.Fatalf("before a heartbeat: %v, want ErrNoHeartbeat", err)
	}
}

func TestClaimOrderAndKinds(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	low := q.verify(t, "A", 0)
	high := q.verify(t, "B", 5)
	if j, err := q.claim(ctx, q.token, []string{"podcast.fetch"}); err != nil || j != nil {
		t.Fatalf("claim of a kind with no jobs = %v, %v", j, err)
	}
	j, err := q.claim(ctx, q.token, []string{KindVerify})
	if err != nil || j.ID != high.ID || j.State != StateClaimed || j.Attempts != 1 || j.LeaseUntil == nil {
		t.Fatalf("first claim = %+v, %v; want the high priority job, claimed", j, err)
	}
	j, err = q.claim(ctx, q.token, []string{KindVerify})
	if err != nil || j.ID != low.ID {
		t.Fatalf("second claim = %+v, %v", j, err)
	}
	if j, err := q.claim(ctx, q.token, []string{KindVerify}); err != nil || j != nil {
		t.Fatalf("empty queue claim = %v, %v", j, err)
	}
}

func TestWaitClaimWakesOnEnqueue(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	got := make(chan *Job, 1)
	go func() {
		j, _ := q.waitClaim(context.Background(), q.token, []string{KindVerify}, MaxWait)
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
		j, err := q.claim(ctx, q.token, []string{KindVerify})
		if err != nil || j == nil || j.Attempts != attempt {
			t.Fatalf("claim %d = %+v, %v", attempt, j, err)
		}
		// Progress renews the lease.
		q.advance(Lease - time.Second)
		if _, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: attempt, State: ptr(StateRunning)}); err != nil {
			t.Fatalf("progress %d: %v", attempt, err)
		}
		q.advance(Lease - time.Second)
		if j, _ := q.Job(ctx, job.ID); j.State != StateRunning {
			t.Fatalf("renewed lease expired: %s", j.State)
		}
		// Then the Bridge goes quiet, having said how far it got.
		if _, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: attempt, Progress: json.RawMessage(`{"step":"hold"}`)}); err != nil {
			t.Fatalf("progress %d: %v", attempt, err)
		}
		q.advance(Lease + time.Second)
		jobs, _, err := q.List(ctx, Filter{})
		if err != nil {
			t.Fatal(err)
		}
		want, progress := StateQueued, `{}` // a waiting job doesn't read as under way
		if attempt == MaxAttempts {
			want, progress = StateFailed, `{"step":"hold"}` // how far the last attempt got
		}
		if jobs[0].State != want || string(jobs[0].Progress) != progress {
			t.Fatalf("after lease %d ran out: %s %s, want %s %s", attempt, jobs[0].State, jobs[0].Progress, want, progress)
		}
		// The dead claim's late report is refused.
		if _, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: attempt, State: ptr(StateDone)}); !errors.Is(err, ErrNotHeld) {
			t.Fatalf("late report %d: %v, want ErrNotHeld", attempt, err)
		}
	}
	if j, _ := q.Job(ctx, job.ID); j.FinishedAt == nil || j.Error == "" {
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
	if _, err := q.claim(ctx, q.token, []string{KindVerify}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: 2, State: ptr(StateRunning)}); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("wrong attempt: %v, want ErrNotHeld", err)
	}
	for name, u := range map[string]Update{
		"back to queued":  {Attempt: 1, State: ptr(StateQueued)},
		"progress array":  {Attempt: 1, Progress: json.RawMessage(`[1]`)},
		"result not json": {Attempt: 1, Result: json.RawMessage(`nope`)},
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
	if _, err := q.claim(ctx, q.token, []string{KindVerify}); err != nil {
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
	if _, err := q.claim(ctx, q.token, []string{KindVerify}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Cancel(ctx, orphan.ID); err != nil {
		t.Fatal(err)
	}
	q.advance(Lease + time.Second)
	if j, _ := q.Job(ctx, orphan.ID); j.State != StateCancelled {
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
	jobs, next, err := q.List(ctx, Filter{Path: ptr("A")})
	if err != nil || len(jobs) != 1 || jobs[0].ID != a.ID || next != 0 {
		t.Fatalf("by path = %+v, %d, %v", jobs, next, err)
	}
	jobs, _, err = q.List(ctx, Filter{State: StateQueued})
	if err != nil || len(jobs) != 1 || jobs[0].ID != b.ID {
		t.Fatalf("by state = %+v, %v", jobs, err)
	}
	jobs, next, err = q.List(ctx, Filter{Limit: 1})
	if err != nil || len(jobs) != 1 || jobs[0].ID != b.ID || next != b.ID {
		t.Fatalf("first page = %+v, %d, %v", jobs, next, err)
	}
	jobs, next, err = q.List(ctx, Filter{Limit: 1, Before: next})
	if err != nil || len(jobs) != 1 || jobs[0].ID != a.ID || next != 0 {
		t.Fatalf("last page = %+v, %d, %v; want no next page", jobs, next, err)
	}
	// An out-of-range limit is the default, and an exact last page offers no
	// next one.
	jobs, next, _ = q.List(ctx, Filter{Limit: 500})
	if len(jobs) != 2 || next != 0 {
		t.Fatalf("limit 500 = %d jobs, next %d", len(jobs), next)
	}
	jobs, next, _ = q.List(ctx, Filter{Limit: 2})
	if len(jobs) != 2 || next != 0 {
		t.Fatalf("an exact page = %d jobs, next %d", len(jobs), next)
	}
}

// A read takes the writer only when a lease has run out.
func TestReadsExpireOnlyWhenDue(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	job := q.verify(t, "A", 0)
	if _, err := q.claim(ctx, q.token, []string{KindVerify}); err != nil {
		t.Fatal(err)
	}
	before, _ := q.Job(ctx, job.ID)
	q.advance(Lease - time.Second)
	if j, _ := q.Job(ctx, job.ID); j.State != StateClaimed || j.UpdatedAt != before.UpdatedAt {
		t.Fatalf("a live lease was touched: %+v", j)
	}
	q.advance(2 * time.Second)
	if j, _ := q.Job(ctx, job.ID); j.State != StateQueued {
		t.Fatalf("a lapsed lease stayed: %s", j.State)
	}
}

// MoveJobs carries a book's jobs, rebuilding their dedupe key, and cancels an
// open one that collides with an open job of its kind at the new path.
func TestMoveJobs(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	old := q.verify(t, "Old", 0)
	clash := q.verify(t, "Taken", 0)
	move := func(from, to string) {
		t.Helper()
		if err := q.db.WithTx(ctx, "move", func(tx *sql.Tx) error {
			return MoveJobs(ctx, tx, q.lib, from, to, *q.now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	move("Old", "New")
	j, _ := q.Job(ctx, old.ID)
	if j.Path == nil || *j.Path != "New" || j.State != StateQueued {
		t.Fatalf("moved = %+v", j)
	}
	// The second press at the new path finds the moved job (the key moved too).
	again, created, err := q.Enqueue(ctx, NewJob{Kind: KindVerify, LibraryID: &q.lib, Path: ptr("New"), RequestedBy: q.admin})
	if err != nil || created || again.ID != old.ID {
		t.Fatalf("the key didn't move: %+v %v %v", again, created, err)
	}
	move("New", "Taken")
	if j, _ := q.Job(ctx, old.ID); j.State != StateCancelled || *j.Path != "Taken" {
		t.Fatalf("the colliding job = %+v, want cancelled and moved", j)
	}
	if j, _ := q.Job(ctx, clash.ID); j.State != StateQueued {
		t.Fatalf("the job already there = %s", j.State)
	}
}

// A lease that lapsed on its last attempt is failed, and announced, by the next
// Service call after a move, never inside the move's transaction: there, the
// failure it caused would never be told to an admin (the move commits in the
// catalog, which reports nothing).
func TestMoveJobsLeavesLapsedLeasesToTheService(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	heard := q.heard()
	q.verify(t, "Old", 0)
	job := q.verify(t, "Elsewhere", 1) // claimed first
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if j, err := q.claim(ctx, q.token, []string{KindVerify}); err != nil || j == nil || j.ID != job.ID {
			t.Fatalf("claim %d = %+v, %v", attempt, j, err)
		}
		q.advance(Lease + time.Second)
	}
	// Another book moves while the last attempt's lease has lapsed.
	if err := q.db.WithTx(ctx, "move", func(tx *sql.Tx) error {
		return MoveJobs(ctx, tx, q.lib, "Old", "New", *q.now)
	}); err != nil {
		t.Fatal(err)
	}
	if len(*heard) != 0 {
		t.Fatalf("heard %v during the move", *heard)
	}
	if _, _, err := q.List(ctx, Filter{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"claimed>failed:"}; !slices.Equal(*heard, want) {
		t.Fatalf("heard = %v, want %v", *heard, want)
	}
	if j, _ := q.Job(ctx, job.ID); j.State != StateFailed {
		t.Fatalf("job = %+v", j)
	}
}

// Security-critical: a claim or a heartbeat is the paired Bridge's only while its
// token is live. A long poll outlives the request's authentication, so a Bridge
// that another pairing (or an unpair) replaced mid-wait gets no job and lands no
// status.
func TestStaleTokenNeitherClaimsNorBeats(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	job := q.verify(t, "A", 0)
	if _, err := q.db.ExecContext(ctx, `UPDATE tokens SET revoked = 1 WHERE id = ?`, q.token); err != nil {
		t.Fatal(err)
	}
	if j, err := q.claim(ctx, q.token, []string{KindVerify}); !errors.Is(err, ErrNotPaired) || j != nil {
		t.Fatalf("denied: a revoked token's claim = %+v, %v; want ErrNotPaired", j, err)
	}
	if err := q.SaveHeartbeat(ctx, q.token, Status{Version: "0.0.1", Kinds: []string{KindVerify}}); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("denied: a revoked token's heartbeat = %v; want ErrNotPaired", err)
	}
	// Allowed: the Bridge that replaced it claims the job, still queued.
	res, err := q.db.ExecContext(ctx, `INSERT INTO tokens(user_id, token_hash, kind, created_at) VALUES(?, 'h2', 'bridge', '2026-01-02T00:00:00Z')`, q.admin)
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := res.LastInsertId()
	j, err := q.claim(ctx, fresh, []string{KindVerify})
	if err != nil || j == nil || j.ID != job.ID || j.Attempts != 1 {
		t.Fatalf("allowed: the live token's claim = %+v, %v", j, err)
	}
}

// A job gone under its claim (its library deleted) is not held any more: the
// Bridge drops it (409 not_held), rather than retrying a 404 forever.
func TestUpdateOfAGoneJob(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	if _, err := q.UpdateHeld(context.Background(), 999, Update{Attempt: 1, State: ptr(StateRunning)}); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("report on a gone job: %v, want ErrNotHeld", err)
	}
}

// The heartbeat cache never goes back: a read that began before a heartbeat was
// saved (or before Forget) doesn't replace the newer answer.
func TestHeartbeatCacheNeverGoesBack(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	readStarts := func() uint64 {
		q.hbMu.Lock()
		defer q.hbMu.Unlock()
		return q.hbGen
	}
	// A read that found no heartbeat yet ends after the first one was saved.
	gen := readStarts()
	q.beat(t, KindVerify)
	q.keep(gen, nil)
	if h, err := q.LastHeartbeat(ctx); err != nil || h == nil {
		t.Fatalf("a stale read replaced the saved heartbeat: %+v, %v", h, err)
	}
	// A read that found the heartbeat ends after an unpair's Forget.
	old, _ := q.LastHeartbeat(ctx)
	q.advance(heartbeatTTL)
	gen = readStarts()
	if _, err := q.db.ExecContext(ctx, `DELETE FROM bridge_status`); err != nil {
		t.Fatal(err)
	}
	q.Forget()
	q.keep(gen, old)
	if h, err := q.LastHeartbeat(ctx); err != nil || h != nil {
		t.Fatalf("a stale read undid Forget: %+v, %v", h, err)
	}
	// A heartbeat whose write landed before an unpair, cached after its Forget.
	gen = q.heartbeatGen()
	q.Forget()
	q.remember(gen, old)
	if h, err := q.LastHeartbeat(ctx); err != nil || h != nil {
		t.Fatalf("a heartbeat saved before an unpair outlived its Forget: %+v, %v", h, err)
	}
}

// A failed job's report lands whatever its error quotes (a tool's output with a
// carriage return, an escape code): the error is cleaned, line breaks kept, never
// the reason the report is refused (the Bridge would retry it for ever, and the
// job would be re-run until it failed for want of reports).
func TestUpdateCleansTheError(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	job := q.verify(t, "A", 0)
	if _, err := q.claim(ctx, q.token, []string{KindVerify}); err != nil {
		t.Fatal(err)
	}
	j, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: 1, State: ptr(StateFailed), Error: ptr("ffmpeg:\r\n\x1b[31mbad\x00 data\n")})
	if err != nil || j.State != StateFailed || j.Error != "ffmpeg: \n [31mbad  data\n" {
		t.Fatalf("failed with a messy error = %+v, %v", j, err)
	}
}

// A job a lapsed lease puts back in the queue wakes a claim already waiting for
// one (here through the console's read, expireDue), rather than waiting for that
// claim's wait to run out.
func TestWaitClaimWakesOnRequeue(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	// A clock the waiting claim reads while the test moves it.
	var clock atomic.Int64
	clock.Store(q.now.Unix())
	q.Service.now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	job := q.verify(t, "A", 0)
	if _, err := q.claim(ctx, q.token, []string{KindVerify}); err != nil { // the lane that dies
		t.Fatal(err)
	}
	got := make(chan *Job, 1)
	go func() {
		j, _ := q.waitClaim(ctx, q.token, []string{KindVerify}, MaxWait)
		got <- j
	}()
	time.Sleep(50 * time.Millisecond) // the other lane is waiting
	clock.Add(int64((Lease + time.Second) / time.Second))
	if _, _, err := q.List(ctx, Filter{}); err != nil {
		t.Fatal(err)
	}
	select {
	case j := <-got:
		if j == nil || j.ID != job.ID || j.Attempts != 2 {
			t.Fatalf("woken claim = %+v", j)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting claim never woke for the requeued job")
	}
}

// A cancel the server asks for itself (a generate job replacing a working
// describe job) records its reason at once and keeps it through the Bridge's
// reports, which send their own error; an admin's cancel keeps no reason, so
// the Bridge's error stands and the console says an admin cancelled it
// (BRIDGE-02). An admin who asked first stays the one who cancelled it.
func TestServerCancelReasonOutlivesReports(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	q.beatLibrary(t, true, true, KindVerify, KindSidecars, KindDescribe)
	reason := kinds[KindSidecars].cancelsOpen[KindDescribe]
	if !IsCancelReason(reason) || !IsCancelReason(CancelMoved) || !IsCancelReason(CancelReplacedRequest) || IsCancelReason("supervisor: stopped") || IsCancelReason("") {
		t.Fatal("IsCancelReason disagrees with the server's reasons")
	}
	stop := func(id int64, attempt int) *Job {
		t.Helper()
		// A keepalive, then the stop, each with the Bridge's own error.
		if _, err := q.UpdateHeld(ctx, id, Update{Attempt: attempt, State: ptr(StateRunning), Error: ptr("")}); err != nil {
			t.Fatal(err)
		}
		j, err := q.UpdateHeld(ctx, id, Update{Attempt: attempt, State: ptr(StateCancelled), Error: ptr("supervisor: stopped")})
		if err != nil || j.State != StateCancelled {
			t.Fatalf("stop = %+v, %v", j, err)
		}
		return j
	}

	// The server's cancel.
	book := ptr("Author/Book")
	if _, _, err := q.describe("", book); err != nil {
		t.Fatal(err)
	}
	desc := q.work(t, KindDescribe, "")
	if _, created, err := q.sidecars("", book); err != nil || !created {
		t.Fatalf("generate = %v, %v", created, err)
	}
	if j, _ := q.Job(ctx, desc.ID); !j.CancelRequested || j.Error != reason || j.State != StateRunning {
		t.Fatalf("asked to stop = %+v, want the reason recorded on the working job", j)
	}
	if j := stop(desc.ID, desc.Attempts); j.Error != reason {
		t.Fatalf("server cancel ended with %q, want %q", j.Error, reason)
	}

	// An admin's cancel: the Bridge's error stands.
	other := ptr("Author/Other")
	if _, _, err := q.describe("", other); err != nil {
		t.Fatal(err)
	}
	admin := q.work(t, KindDescribe, "")
	if _, err := q.Cancel(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	if j := stop(admin.ID, admin.Attempts); j.Error != "supervisor: stopped" || !j.CancelRequested {
		t.Fatalf("admin cancel ended with %+v", j)
	}

	// An admin first, then a generate job: still the admin's cancel.
	third := ptr("Author/Third")
	if _, _, err := q.describe("", third); err != nil {
		t.Fatal(err)
	}
	first := q.work(t, KindDescribe, "")
	if _, err := q.Cancel(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.sidecars("", third); err != nil {
		t.Fatal(err)
	}
	if j := stop(first.ID, first.Attempts); j.Error != "supervisor: stopped" {
		t.Fatalf("admin-then-server cancel ended with %q, want the Bridge's error", j.Error)
	}
}

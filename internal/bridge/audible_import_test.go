package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func TestCheckAudibleDestPath(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "Author/Series/Book.m4b", "Book.m4b", "A b/C, d (2)/x y.m4b"} {
		if err := CheckAudibleDestPath(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"../x.m4b", "a/../b.m4b", "/abs.m4b", "a//b.m4b", "a/./b.m4b", "a/", ".",
		".hidden/b.m4b", "a/.audiosilo/x", `a\b.m4b`, "a/b\x00.m4b", "a/b\n.m4b", "\xff.m4b", strings.Repeat("a", MaxAudibleDestPath+1)} {
		if err := CheckAudibleDestPath(bad); !errors.Is(err, ErrAudibleDestPath) {
			t.Errorf("%q: %v, want refused", bad, err)
		}
	}
}

// The server checks a template by the Bridge's placement grammar.
func TestCheckAudibleTemplate(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "{Author}/{Series}/{ { PaddedSeq } - }{Title}", "{Author}/{Title:upper,trim}",
		"{Author}/{PaddedSeq:width=3} {Title}{Ext}", `Books/\{literal\}/{ASIN}`, "{SHORTCODE}{Seq} - {TitleShort}",
		"{Narrator}/{Year}/{OriginalFilename}", "{ plain group }", "{Title:upper,,trim}", "{Author}/{Title}.m4b"} {
		if err := CheckAudibleTemplate(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	// A hidden part (the scan skips it) and an empty modifier name with a
	// value (the Bridge's parseMods refuses it) too.
	for _, bad := range []string{"{Nope}", "{Author", "{Author}}", "{ unclosed", "{Title:width}", "{Title:width=-1}",
		"{Title:bold}", "{Title:upper{", "/{Author}", " /x", "{Author}/../{Title}", "a\nb", strings.Repeat("x", MaxAudibleTemplate+1),
		"{Title:=x}", "{Title:upper, =1}", ".hidden/{Title}", "{Author}/.x/{Title}", `\.x/{Title}`, "{Author}/{Title}/.m4b"} {
		if err := CheckAudibleTemplate(bad); !errors.Is(err, ErrAudibleTemplate) {
			t.Errorf("%q: %v, want refused", bad, err)
		}
	}
}

func (q *testQueue) beatWritable(t *testing.T, writable bool, kinds ...string) {
	t.Helper()
	st := Status{Version: "0.4.0", Kinds: kinds, Libraries: []LibraryStatus{{LibraryID: q.lib, MountPath: "/m", Mapped: true, Writable: writable}}}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveHeartbeat(context.Background(), q.token, st); err != nil {
		t.Fatal(err)
	}
}

// audible.import: strict params built by the server, one open per account and
// title, a writing job (a library the Bridge can write to), internal and
// gated by the audible module; an account's removal cancels its open ones.
func TestAudibleImportJobs(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	p := AudibleImportParams{Account: "acct0001", ASIN: "B000000001", LibraryID: q.lib, DestPath: "A/B.m4b", Template: "{Author}/{Title}"}
	if _, _, err := q.Enqueue(ctx, AudibleImport(p, q.admin, 0)); !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("no Bridge offering it: %v", err)
	}
	q.beatWritable(t, false, KindVerify, KindAudibleImport)
	if _, _, err := q.Enqueue(ctx, AudibleImport(p, q.admin, 0)); !errors.Is(err, ErrLibraryNotWritable) {
		t.Fatalf("not writable: %v", err)
	}
	q.beatWritable(t, true, KindVerify, KindAudibleImport)
	first, created, err := q.Enqueue(ctx, AudibleImport(p, q.admin, 0))
	if err != nil || !created || *first.LibraryID != q.lib || first.Path != nil {
		t.Fatalf("queue = %+v, %t, %v", first, created, err)
	}
	if string(first.Params) != `{"account":"acct0001","asin":"B000000001","library_id":`+itoa(q.lib)+`,"dest_path":"A/B.m4b","template":"{Author}/{Title}"}` {
		t.Fatalf("params = %s", first.Params)
	}
	again, created, err := q.Enqueue(ctx, AudibleImport(p, q.admin, 0))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("dedupe = %+v, %t, %v", again, created, err)
	}
	other := p
	other.ASIN = "B000000002"
	second, created, err := q.Enqueue(ctx, AudibleImport(other, q.admin, 0))
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("another title = %+v, %t, %v", second, created, err)
	}
	another := p
	another.Account = "acct0002"
	third, created, err := q.Enqueue(ctx, AudibleImport(another, q.admin, 0))
	if err != nil || !created {
		t.Fatalf("another account = %+v, %t, %v", third, created, err)
	}
	if ModuleFor(KindAudibleImport, nil) != ModuleAudible || !kinds[KindAudibleImport].internal || kinds[KindAudibleImport].quiet {
		t.Fatal("audible.import: internal, the audible module, notified")
	}
	for name, raw := range map[string]string{
		"unknown field": `{"account":"acct0001","asin":"B000000001","library_id":1,"x":1}`,
		"bad account":   `{"account":"ACCT","asin":"B000000001","library_id":1}`,
		"bad asin":      `{"account":"acct0001","asin":"b000000001","library_id":1}`,
		"no library":    `{"account":"acct0001","asin":"B000000001"}`,
		"bad dest":      `{"account":"acct0001","asin":"B000000001","library_id":1,"dest_path":"../x"}`,
		"bad template":  `{"account":"acct0001","asin":"B000000001","library_id":1,"template":"{Nope}"}`,
	} {
		if _, err := audibleImportParams(json.RawMessage(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Which rule a stored value broke stays readable: the console's routes
	// answer invalid_dest_path / invalid_template for it.
	for raw, want := range map[string]error{
		`{"account":"acct0001","asin":"B000000001","library_id":1,"dest_path":"../x"}`:  ErrAudibleDestPath,
		`{"account":"acct0001","asin":"B000000001","library_id":1,"template":"{Nope}"}`: ErrAudibleTemplate,
	} {
		if _, err := audibleImportParams(json.RawMessage(raw)); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", raw, err, want)
		}
	}
	// Removing acct0001 cancels both of its backups, not acct0002's, whose
	// stored params a damaged row has lost (ParamSQL: no failed read).
	q.damageParams(t, third.ID)
	if err := q.db.WithTx(ctx, "t", func(tx *sql.Tx) error {
		return CancelAudibleImports(ctx, tx, "acct0001", CancelAudibleRemoved, *q.now)
	}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{first.ID: StateCancelled, second.ID: StateCancelled, third.ID: StateQueued} {
		if j, err := q.Job(ctx, id); err != nil || j.State != want {
			t.Errorf("job %d = %+v, %v, want %s", id, j, err, want)
		}
	}
}

// Each library, including an account's previous mapping, gets a range seek
// for that account's open backups. A scan of the open index would visit other
// people's jobs on every delete.
func TestCancelAudibleImportsPlan(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	plan := storetest.QueryPlan(t, q.db, openAudibleImportsSQL,
		KindAudibleImport+"|", "||acct0001|", "||acct0001}", "acct0001")
	if !strings.Contains(strings.Join(plan, "\n"), "idx_bridge_jobs_open (dedupe_key>? AND dedupe_key<?)") {
		t.Fatalf("no account range seek: %v", plan)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// A failed backup's code: the result's, else a coded error; a park's code.
func TestAudibleImportCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		j    Job
		want string
	}{
		{Job{State: StateFailed, Result: json.RawMessage(`{"code":"exists","message":"a different file"}`)}, "exists"},
		{Job{State: StateFailed, Error: "disk_full: 2 GB free, 5 GB needed"}, "disk_full"},
		{Job{State: StateFailed, Error: "drm_free: only an MP3"}, "drm_free"},
		{Job{State: StateFailed, Error: "something else"}, ""},
		{Job{State: StateNeedsAttention, Progress: json.RawMessage(`{"park":{"code":"audible_refused"}}`)}, "audible_refused"},
		{Job{State: StateDone, Result: json.RawMessage(`{"code":"exists"}`)}, ""},
		{Job{State: StateRunning, Error: "exists: x"}, ""},
	} {
		if got := AudibleImportCode(tc.j); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.j, got, tc.want)
		}
	}
}

// EnqueueAll queues a batch of backups in one transaction: the same jobs as
// one Enqueue each (the open one found, not queued twice), each passed to
// record in that transaction, so a record that fails queues none. A refusal
// stops it at the job refused, the jobs before it queued; a kind that is no
// batch's is refused whole.
func TestEnqueueAll(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	const n = 750
	batch := make([]NewJob, n)
	for i := range batch {
		p := AudibleImportParams{Account: "acct0001", ASIN: fmt.Sprintf("B%09d", i+1), LibraryID: q.lib}
		batch[i] = AudibleImport(p, q.admin, PriorityBulk)
	}
	none := func(*sql.Tx, int, int64) error { return nil }
	if _, err := q.EnqueueAll(ctx, batch, none); !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("no Bridge offering it: %v", err)
	}
	q.beatWritable(t, false, KindVerify, KindAudibleImport)
	if created, err := q.EnqueueAll(ctx, batch, none); created != 0 || !errors.Is(err, ErrLibraryNotWritable) {
		t.Fatalf("not writable = %d, %v", created, err)
	}
	q.beatWritable(t, true, KindVerify, KindAudibleImport)
	open, _, err := q.Enqueue(ctx, batch[1])
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var c int
		if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bridge_jobs WHERE kind = ?`, KindAudibleImport).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	failing := errors.New("record failed")
	if created, err := q.EnqueueAll(ctx, batch, func(_ *sql.Tx, i int, _ int64) error {
		if i == n-1 {
			return failing
		}
		return nil
	}); created != 0 || !errors.Is(err, failing) || count() != 1 {
		t.Fatalf("a failed record = %d, %v, %d jobs: want none queued", created, err, count())
	}
	ids := make([]int64, n)
	created, err := q.EnqueueAll(ctx, batch, func(tx *sql.Tx, i int, id int64) error {
		// The job is in record's transaction.
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM bridge_jobs WHERE id = ?`, id).Scan(&state); err != nil {
			return err
		}
		ids[i] = id
		return nil
	})
	if err != nil || created != n-1 || count() != n {
		t.Fatalf("batch = %d, %v, %d jobs", created, err, count())
	}
	if ids[1] != open.ID {
		t.Fatalf("the open job = %d, want %d", ids[1], open.ID)
	}
	for i, id := range ids {
		j, err := q.Job(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var p AudibleImportParams
		if err := json.Unmarshal(j.Params, &p); err != nil || p.ASIN != fmt.Sprintf("B%09d", i+1) ||
			j.Priority != PriorityBulk || j.State != StateQueued {
			t.Fatalf("job %d = %+v, %v", i, j, err)
		}
	}
	if again, err := q.EnqueueAll(ctx, batch, none); again != 0 || err != nil || count() != n {
		t.Fatalf("again = %d, %v, %d jobs", again, err, count())
	}
	// A job already asked to stop is not joined: it ends, and its title is
	// queued again (Enqueue's rule).
	stopping, err := q.claim(ctx, q.token, []string{KindAudibleImport})
	if err != nil || stopping == nil {
		t.Fatalf("claim = %+v, %v", stopping, err)
	}
	if _, err := q.Cancel(ctx, stopping.ID); err != nil {
		t.Fatal(err)
	}
	at := slices.Index(ids, stopping.ID)
	created, err = q.EnqueueAll(ctx, batch, func(_ *sql.Tx, i int, id int64) error {
		ids[i] = id
		return nil
	})
	if err != nil || created != 1 || count() != n+1 || ids[at] == stopping.ID {
		t.Fatalf("over a stopping job = %d, %v, %d jobs, id %d (was %d)", created, err, count(), ids[at], stopping.ID)
	}
	if j, err := q.Job(ctx, stopping.ID); err != nil || j.State != StateCancelled || j.FinishedAt == nil {
		t.Fatalf("the stopping job = %+v, %v; want cancelled", j, err)
	}
	if j, err := q.Job(ctx, ids[at]); err != nil || j.State != StateQueued {
		t.Fatalf("its new job = %+v, %v; want queued", j, err)
	}
	// A bad job stops the batch there.
	more := []NewJob{
		AudibleImport(AudibleImportParams{Account: "acct0002", ASIN: "B000000001", LibraryID: q.lib}, q.admin, 0),
		AudibleImport(AudibleImportParams{Account: "acct0002", ASIN: "nope", LibraryID: q.lib}, q.admin, 0),
		AudibleImport(AudibleImportParams{Account: "acct0002", ASIN: "B000000003", LibraryID: q.lib}, q.admin, 0),
	}
	if created, err := q.EnqueueAll(ctx, more, none); created != 1 || !errors.Is(err, ErrInvalid) || count() != n+2 {
		t.Fatalf("a bad job = %d, %v, %d jobs", created, err, count())
	}
	// A library that can't be read is no refusal: none queued.
	unread := []NewJob{
		AudibleImport(AudibleImportParams{Account: "acct0003", ASIN: "B000000001", LibraryID: q.lib}, q.admin, 0),
		AudibleImport(AudibleImportParams{Account: "acct0003", ASIN: "B000000002", LibraryID: q.lib + 1000}, q.admin, 0),
	}
	if created, err := q.EnqueueAll(ctx, unread, none); created != 0 || !errors.Is(err, sql.ErrNoRows) || count() != n+2 {
		t.Fatalf("an unread library = %d, %v, %d jobs", created, err, count())
	}
	if _, err := q.EnqueueAll(ctx, []NewJob{{Kind: KindVerify, LibraryID: &q.lib}}, none); !errors.Is(err, ErrInvalid) {
		t.Fatalf("another kind: %v", err)
	}
}

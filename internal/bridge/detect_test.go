package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func (q *testQueue) detect(params string, path *string) (*Job, bool, error) {
	j := NewJob{Kind: KindDetectChapters, LibraryID: &q.lib, Path: path, RequestedBy: q.admin}
	if params != "" {
		j.Params = json.RawMessage(params)
	}
	return q.Enqueue(context.Background(), j)
}

// chapters.detect takes no params (none or {} is stored as {}; anything else
// is refused), needs a book, a Bridge that runs it and a library it can write
// to; a second press is the open job. It is refused while a generate job is
// open for the book (that run finds the chapters itself), and a generate job is
// refused while it is open (it would throw away the transcription the generate
// run then reuses). Its family is the chapters feature.
func TestDetectQueueRules(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	q.beatLibrary(t, true, true, KindVerify, KindSidecars)
	if _, _, err := q.detect("", ptr("Author/Book")); !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("a Bridge without the kind: %v, want ErrKindUnavailable", err)
	}
	q.beatLibrary(t, true, true, KindVerify, KindSidecars, KindDetectChapters)
	var first *Job
	for _, params := range []string{"", `{}`, `null`} {
		job, _, err := q.detect(params, ptr("Author/Book"))
		if err != nil || string(job.Params) != `{}` || job.Kind != KindDetectChapters {
			t.Fatalf("params %q: %v, %+v", params, err, job)
		}
		if first == nil {
			first = job
		} else if job.ID != first.ID {
			t.Fatalf("a second press queued job %d, want the open job %d", job.ID, first.ID)
		}
	}
	for name, params := range map[string]string{
		"a field":       `{"source":"audio"}`,
		"not an object": `[]`,
		"trailing data": `{} {}`,
	} {
		if _, _, err := q.detect(params, ptr("Author/Other")); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, _, err := q.detect("", nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("no path: %v, want ErrInvalid", err)
	}

	// Denied: a generate job for a book whose detect job is open; once it
	// ends, the generate job is queued.
	if _, _, err := q.sidecars("", ptr("Author/Book")); !errors.Is(err, ErrDetectOpen) {
		t.Fatalf("generate with a detect open: %v, want ErrDetectOpen", err)
	}
	if _, err := q.Cancel(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	gen, created, err := q.sidecars("", ptr("Author/Book"))
	if err != nil || !created {
		t.Fatalf("generate once the detect ended: %v, %v", created, err)
	}
	// Denied: a detect job for a book whose generate job is open; another
	// book's is queued.
	if _, _, err := q.detect("", ptr("Author/Book")); !errors.Is(err, ErrGenerateOpen) {
		t.Fatalf("detect with a generate open: %v, want ErrGenerateOpen", err)
	}
	if _, created, err := q.detect("", ptr("Author/Third")); err != nil || !created {
		t.Fatalf("another book: %v, %v", created, err)
	}
	if _, err := q.Cancel(context.Background(), gen.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := q.detect("", ptr("Author/Book")); err != nil || !created {
		t.Fatalf("detect once the generate ended: %v, %v", created, err)
	}

	// Denied: a library the Bridge can't write to.
	q.beatLibrary(t, true, false, KindVerify, KindSidecars, KindDetectChapters)
	if _, _, err := q.detect("", ptr("Author/Fourth")); !errors.Is(err, ErrLibraryNotWritable) {
		t.Errorf("read-only library: %v, want ErrLibraryNotWritable", err)
	}
	if got := (Status{Kinds: []string{KindDetectChapters, KindSidecars}}).Features(); !slices.Equal(got, []string{"chapters", "sidecars"}) {
		t.Errorf("features = %v", got)
	}
}

// A detect job parked because no chapters were heard waits for a person, so an
// admin hears of it.
func TestDetectParkIsNotable(t *testing.T) {
	t.Parallel()
	j := &Job{State: StateNeedsAttention, Progress: json.RawMessage(`{"park":{"code":"chapters_not_detected"}}`)}
	if !j.Notable(StateRunning, "") {
		t.Errorf("chapters_not_detected: not notable")
	}
}

// work claims the queued job of kind (attempt 1) and reports it running, or,
// with park, parked with code.
func (q *testQueue) work(t *testing.T, kind string, park string) *Job {
	t.Helper()
	ctx := context.Background()
	j, err := q.claim(ctx, q.token, []string{kind})
	if err != nil || j == nil {
		t.Fatalf("claim %s = %+v, %v", kind, j, err)
	}
	u := Update{Attempt: j.Attempts, State: ptr(StateRunning)}
	if park != "" {
		u = Update{Attempt: j.Attempts, State: ptr(StateNeedsAttention),
			Progress: json.RawMessage(`{"park":{"code":"` + park + `"}}`), Error: ptr("no headings heard")}
	}
	if j, err = q.UpdateHeld(ctx, j.ID, u); err != nil {
		t.Fatal(err)
	}
	return j
}

// A generate job is refused only while the book's detect job is queued or
// working (Blocker names that job); a parked detect job (a person's decision,
// chapters_not_detected) gives way: the generate job is queued and cancels it,
// saying why. A second press on a parked detect job is that job.
func TestParkedDetectGivesWayToGenerate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	q.beatLibrary(t, true, true, KindVerify, KindSidecars, KindDetectChapters)
	book := ptr("Author/Book")
	det, _, err := q.detect("", book)
	if err != nil {
		t.Fatal(err)
	}
	// Denied: queued, then running.
	if _, _, err := q.sidecars("", book); !errors.Is(err, ErrDetectOpen) {
		t.Fatalf("generate with a queued detect: %v, want ErrDetectOpen", err)
	}
	q.work(t, KindDetectChapters, "")
	if _, _, err := q.sidecars("", book); !errors.Is(err, ErrDetectOpen) {
		t.Fatalf("generate with a running detect: %v, want ErrDetectOpen", err)
	}
	if b, err := q.Blocker(ctx, KindSidecars, q.lib, *book); err != nil || b == nil || b.ID != det.ID {
		t.Fatalf("Blocker while running = %+v, %v; want the detect job", b, err)
	}
	// Parked: the second press is the parked job, and nothing blocks a generate.
	parked, err := q.UpdateHeld(ctx, det.ID, Update{Attempt: 1, State: ptr(StateNeedsAttention),
		Progress: json.RawMessage(`{"park":{"code":"chapters_not_detected"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if again, created, err := q.detect("", book); err != nil || created || again.ID != det.ID || again.State != StateNeedsAttention {
		t.Fatalf("second press on a parked detect = %+v, %v, %v", again, created, err)
	}
	if b, err := q.Blocker(ctx, KindSidecars, q.lib, *book); err != nil || b != nil {
		t.Fatalf("Blocker while parked = %+v, %v; want none", b, err)
	}
	// Allowed: the generate job is queued and the parked detect job cancelled.
	if _, created, err := q.sidecars("", book); err != nil || !created {
		t.Fatalf("generate with a parked detect: %v, %v", created, err)
	}
	got, err := q.Job(ctx, parked.ID)
	if err != nil || got.State != StateCancelled || got.Error == "" {
		t.Fatalf("the parked detect = %+v, %v; want cancelled with a reason", got, err)
	}
}

// A job being cancelled (cancel_requested) ends without writing, so it refuses
// nothing: a detect job is queued behind a generate job being cancelled, and a
// generate job behind a detect job being cancelled.
func TestCancellingJobsRefuseNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	q.beatLibrary(t, true, true, KindVerify, KindSidecars, KindDetectChapters)
	book := ptr("Author/Book")
	if _, _, err := q.sidecars("", book); err != nil {
		t.Fatal(err)
	}
	gen := q.work(t, KindSidecars, "")
	if _, _, err := q.detect("", book); !errors.Is(err, ErrGenerateOpen) {
		t.Fatalf("detect with a running generate: %v, want ErrGenerateOpen", err)
	}
	if j, err := q.Cancel(ctx, gen.ID); err != nil || !j.CancelRequested || j.State != StateRunning {
		t.Fatalf("cancel = %+v, %v", j, err)
	}
	det, created, err := q.detect("", book)
	if err != nil || !created {
		t.Fatalf("detect with a generate being cancelled: %v, %v", created, err)
	}

	other := ptr("Author/Other")
	if _, _, err := q.detect("", other); err != nil {
		t.Fatal(err)
	}
	// The claim takes the oldest queued detect job: the first book's.
	if j := q.work(t, KindDetectChapters, ""); j.ID != det.ID {
		t.Fatalf("claimed %d, want %d", j.ID, det.ID)
	}
	running := q.work(t, KindDetectChapters, "")
	if _, _, err := q.sidecars("", other); !errors.Is(err, ErrDetectOpen) {
		t.Fatalf("generate with a running detect: %v, want ErrDetectOpen", err)
	}
	if _, err := q.Cancel(ctx, running.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := q.sidecars("", other); err != nil || !created {
		t.Fatalf("generate with a detect being cancelled: %v, %v", created, err)
	}
}

// sidecars.describe and chapters.detect both rewrite the book's bridge.json on
// the Bridge, so each is refused while the other is open (parked included:
// neither gives way to the other), and queued once it ended.
func TestDescribeAndDetectExcludeEachOther(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	q.beatLibrary(t, true, true, KindVerify, KindSidecars, KindDescribe, KindDetectChapters)
	book := ptr("Author/Book")
	desc, _, err := q.describe("", book)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.detect("", book); !errors.Is(err, ErrDescribeOpen) {
		t.Fatalf("detect with a queued describe: %v, want ErrDescribeOpen", err)
	}
	q.work(t, KindDescribe, "agent_credential_refused")
	if _, _, err := q.detect("", book); !errors.Is(err, ErrDescribeOpen) {
		t.Fatalf("detect with a parked describe: %v, want ErrDescribeOpen", err)
	}
	if _, err := q.Cancel(ctx, desc.ID); err != nil {
		t.Fatal(err)
	}
	det, created, err := q.detect("", book)
	if err != nil || !created {
		t.Fatalf("detect once the describe ended: %v, %v", created, err)
	}
	if _, _, err := q.describe("", book); !errors.Is(err, ErrDetectOpen) {
		t.Fatalf("describe with a queued detect: %v, want ErrDetectOpen", err)
	}
	q.work(t, KindDetectChapters, "chapters_not_detected")
	if _, _, err := q.describe("", book); !errors.Is(err, ErrDetectOpen) {
		t.Fatalf("describe with a parked detect: %v, want ErrDetectOpen", err)
	}
	if _, err := q.Cancel(ctx, det.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := q.describe("", book); err != nil || !created {
		t.Fatalf("describe once the detect ended: %v, %v", created, err)
	}
}

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func (q *testQueue) readalong(params string, path *string) (*Job, bool, error) {
	j := NewJob{Kind: KindReadalong, LibraryID: &q.lib, Path: path, RequestedBy: q.admin}
	if params != "" {
		j.Params = json.RawMessage(params)
	}
	return q.Enqueue(context.Background(), j)
}

// readalong.align takes no params, needs a book, a Bridge that runs it and a
// library it can write to; a second press is the open job (a re-align of a
// ready map is just another job). Its family is the readalong feature.
func TestReadalongQueueRules(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	q.beatLibrary(t, true, true, KindVerify, KindSidecars)
	if _, _, err := q.readalong("", ptr("Author/Book")); !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("a Bridge without the kind: %v, want ErrKindUnavailable", err)
	}
	q.beatLibrary(t, true, true, KindVerify, KindSidecars, KindDetectChapters, KindReadalong)
	var first *Job
	for _, params := range []string{"", `{}`, `null`} {
		job, _, err := q.readalong(params, ptr("Author/Book"))
		if err != nil || string(job.Params) != `{}` || job.Kind != KindReadalong {
			t.Fatalf("params %q: %v, %+v", params, err, job)
		}
		if first == nil {
			first = job
		} else if job.ID != first.ID {
			t.Fatalf("a second press queued job %d, want the open job %d", job.ID, first.ID)
		}
	}
	if _, _, err := q.readalong(`{"source":"ebook"}`, ptr("Author/Other")); !errors.Is(err, ErrInvalid) {
		t.Errorf("a field: %v, want ErrInvalid", err)
	}
	if _, _, err := q.readalong("", nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("no path: %v, want ErrInvalid", err)
	}
	q.beatLibrary(t, true, false, KindVerify, KindSidecars, KindReadalong)
	if _, _, err := q.readalong("", ptr("Author/Fourth")); !errors.Is(err, ErrLibraryNotWritable) {
		t.Errorf("read-only library: %v, want ErrLibraryNotWritable", err)
	}
	if got := (Status{Kinds: []string{KindReadalong, KindSidecars}}).Features(); !slices.Equal(got, []string{"readalong", "sidecars"}) {
		t.Errorf("features = %v", got)
	}
}

// Read-along, generate and detect all transcribe the book's audio on the
// Bridge, so they never overlap: a read-along job waits for an open generate
// or detect job, a detect job for an open read-along job, and a generate job
// for a queued or working read-along job (each allowed once the other ends).
func TestReadalongExcludesGenerateAndDetect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	q.beatLibrary(t, true, true, KindVerify, KindSidecars, KindDetectChapters, KindReadalong)
	book := ptr("Author/Book")

	// Denied both ways with generate.
	gen, _, err := q.sidecars("", book)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.readalong("", book); !errors.Is(err, ErrGenerateOpen) {
		t.Fatalf("read-along with a generate open: %v, want ErrGenerateOpen", err)
	}
	if _, err := q.Cancel(ctx, gen.ID); err != nil {
		t.Fatal(err)
	}
	ra, created, err := q.readalong("", book)
	if err != nil || !created {
		t.Fatalf("read-along once the generate ended: %v, %v", created, err)
	}
	if _, _, err := q.sidecars("", book); !errors.Is(err, ErrReadalongOpen) {
		t.Fatalf("generate with a queued read-along: %v, want ErrReadalongOpen", err)
	}
	q.work(t, KindReadalong, "")
	if _, _, err := q.sidecars("", book); !errors.Is(err, ErrReadalongOpen) {
		t.Fatalf("generate with a running read-along: %v, want ErrReadalongOpen", err)
	}
	if b, err := q.Blocker(ctx, KindSidecars, q.lib, *book); err != nil || b == nil || b.ID != ra.ID {
		t.Fatalf("Blocker = %+v, %v; want the read-along job", b, err)
	}

	// Denied both ways with detect.
	if _, _, err := q.detect("", book); !errors.Is(err, ErrReadalongOpen) {
		t.Fatalf("detect with a read-along open: %v, want ErrReadalongOpen", err)
	}
	if _, err := q.Cancel(ctx, ra.ID); err != nil {
		t.Fatal(err)
	}
	if b, err := q.Blocker(ctx, KindDetectChapters, q.lib, *book); err != nil || b != nil {
		t.Fatalf("Blocker of a detect behind a cancelling read-along = %+v, %v; want none", b, err)
	}
	other := ptr("Author/Other")
	if _, created, err := q.detect("", other); err != nil || !created {
		t.Fatalf("detect: %v, %v", created, err)
	}
	if _, _, err := q.readalong("", other); !errors.Is(err, ErrDetectOpen) {
		t.Fatalf("read-along with a detect open: %v, want ErrDetectOpen", err)
	}
	// Another book is never refused.
	if _, created, err := q.readalong("", ptr("Author/Third")); err != nil || !created {
		t.Fatalf("another book: %v, %v", created, err)
	}
}

// A parked read-along job (waiting for an ASR worker, say) gives way to a new
// generate job, which cancels it saying why; it still refuses a detect job.
func TestParkedReadalongGivesWayToGenerate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	q.beatLibrary(t, true, true, KindVerify, KindSidecars, KindDetectChapters, KindReadalong)
	book := ptr("Author/Book")
	ra, _, err := q.readalong("", book)
	if err != nil {
		t.Fatal(err)
	}
	q.work(t, KindReadalong, "asr_worker_waiting")
	if _, _, err := q.detect("", book); !errors.Is(err, ErrReadalongOpen) {
		t.Fatalf("detect with a parked read-along: %v, want ErrReadalongOpen", err)
	}
	if _, created, err := q.sidecars("", book); err != nil || !created {
		t.Fatalf("generate with a parked read-along: %v, %v", created, err)
	}
	got, err := q.Job(ctx, ra.ID)
	if err != nil || got.State != StateCancelled || !strings.Contains(got.Error, "make the read-along again") {
		t.Fatalf("the parked read-along = %+v, %v; want cancelled with a reason", got, err)
	}
}

// ReadalongCoverage reads a finished read-along job's text coverage, and
// nothing from any other job or a result that doesn't say.
func TestReadalongCoverage(t *testing.T) {
	t.Parallel()
	done := func(kind, result string) *Job {
		return &Job{Kind: kind, State: StateDone, Result: json.RawMessage(result)}
	}
	if c, ok := done(KindReadalong, `{"coverage":{"text":0.996,"audio":0.98}}`).ReadalongCoverage(); !ok || c != 0.996 {
		t.Errorf("coverage = %v, %v", c, ok)
	}
	for name, j := range map[string]*Job{
		"another kind":   done(KindDetectChapters, `{"coverage":{"text":0.9}}`),
		"not done":       {Kind: KindReadalong, State: StateFailed, Result: json.RawMessage(`{"coverage":{"text":0.9}}`)},
		"no coverage":    done(KindReadalong, `{}`),
		"out of range":   done(KindReadalong, `{"coverage":{"text":1.5}}`),
		"the old number": done(KindReadalong, `{"coverage":0.9}`),
	} {
		if _, ok := j.ReadalongCoverage(); ok {
			t.Errorf("%s: a coverage", name)
		}
	}
}

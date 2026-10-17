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

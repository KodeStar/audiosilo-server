package coverage

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// fakeMeta is the metadata service as the job sees it, recording every call.
type fakeMeta struct {
	mu       sync.Mutex
	known    map[string]bool
	unreach  bool
	noReport bool
	failAt   int // the lookup (1-based) that fails; 0 for none
	lookups  []string
	pings    int
	posts    [][]string
	postErr  error
	onLookup func(n int) // after each lookup
}

func (f *fakeMeta) Ping() meta.Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pings++
	if f.unreach {
		return meta.Health{Error: "not responding"}
	}
	return meta.Health{Reachable: true}
}

func (f *fakeMeta) CanReport() bool { return !f.noReport }

func (f *fakeMeta) KnownASIN(ctx context.Context, asin string) (bool, error) {
	f.mu.Lock()
	f.lookups = append(f.lookups, asin)
	n := len(f.lookups)
	known := f.known[asin]
	fail := f.failAt != 0 && n >= f.failAt
	hook := f.onLookup
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if fail {
		return false, errors.New("meta: upstream status 503")
	}
	return known, nil
}

func (f *fakeMeta) ReportUnknownASINs(ctx context.Context, asins []string) (meta.ReportAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return meta.ReportAnswer{}, err
	}
	f.posts = append(f.posts, slices.Clone(asins))
	if f.postErr != nil {
		return meta.ReportAnswer{}, f.postErr
	}
	return meta.ReportAnswer{Accepted: len(asins)}, nil
}

func (f *fakeMeta) calls() (pings, lookups, posts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pings, len(f.lookups), len(f.posts)
}

func (f *fakeMeta) posted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.posts {
		out = append(out, p...)
	}
	return out
}

// clock is a settable Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	cat   *catalog.Catalog
	lib   int64
	md    *fakeMeta
	clock *clock
	on    atomic.Bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cat := catalog.New(storetest.Open(t), nil)
	lib, err := cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{cat: cat, lib: lib.ID, md: &fakeMeta{known: map[string]bool{}},
		clock: &clock{t: time.Date(2026, 11, 5, 4, 0, 0, 0, time.UTC)}}
	e.on.Store(true)
	return e
}

func (e *env) runner() *Runner {
	return New(e.cat, e.md, Options{On: e.on.Load, Now: e.clock.now})
}

// book adds a book with asin (its own when tag is false, a tag hint otherwise).
func (e *env) book(t *testing.T, path, asin string, tag bool) {
	t.Helper()
	b := &catalog.Book{LibraryID: e.lib, RelPath: path, Title: path, Author: "A", AddedAt: "2020-01-01"}
	if tag {
		b.TagASIN = asin
	} else {
		b.ASIN = asin
	}
	if _, err := e.cat.UpsertBook(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

// unknowns adds n books with unknown ASINs B0UNK00000.. and returns them.
func (e *env) unknowns(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for i := range n {
		a := fmt.Sprintf("B0UNK%05d", i)
		e.book(t, "u/"+a, a, false)
		out = append(out, a)
	}
	return out
}

func (e *env) status(t *testing.T) catalog.UnknownASINReport {
	t.Helper()
	st, err := e.cat.UnknownASINReportState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func sorted(s []string) []string { return slices.Sorted(slices.Values(s)) }

// A run sends the unknown ASINs, upper-cased and shuffled, and never a known one.
func TestRunSendsTheUnknown(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	want := e.unknowns(t, 50)
	e.book(t, "lower", "b0lower001", false) // its own ASIN, lower case
	want = append(want, "B0LOWER001")
	e.book(t, "tag", "B0TAGGED01", true) // a tag hint on a book with no identifier
	want = append(want, "B0TAGGED01")
	e.book(t, "known", "B0KNOWN001", false)
	e.md.known["B0KNOWN001"] = true

	r := e.runner()
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := e.md.posted()
	if !slices.Equal(sorted(got), sorted(want)) {
		t.Fatalf("posted %v, want %v", got, sorted(want))
	}
	if slices.IsSorted(got) {
		t.Fatal("the ASINs were sent in library order, not shuffled")
	}
	st, _ := r.Status(context.Background())
	if st.State != StateOK || st.LastSent != len(want) || st.LastRunAt != stamp(e.clock.now()) {
		t.Fatalf("status = %+v", st)
	}
	rows, _ := e.cat.UnknownASINRows(context.Background())
	if rows["B0KNOWN001"].Unknown || rows["B0KNOWN001"].SentAt != "" || rows["B0UNK00001"].SentAt == "" {
		t.Fatalf("rows = %+v", rows)
	}
}

// Off (the switch or metadata: On), the job makes no call at all.
func TestRunOffMakesNoCalls(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.unknowns(t, 3)
	e.on.Store(false)
	r := e.runner()
	if err := r.RunOnce(context.Background()); !errors.Is(err, errSwitchedOff) {
		t.Fatalf("err = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r.Run(ctx) // the schedule while off: looks at the switch only
	if p, l, s := e.md.calls(); p+l+s != 0 {
		t.Fatalf("calls while off: %d pings, %d lookups, %d posts", p, l, s)
	}
	if st, _ := r.Status(context.Background()); st.State != StateNever {
		t.Fatalf("status = %+v", st)
	}
}

// Switched off mid-run: the lookups stop, nothing is sent, no status.
func TestRunSwitchedOffMidRun(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.unknowns(t, 20)
	r := e.runner()
	r.offCheck = time.Millisecond
	e.md.onLookup = func(n int) {
		if n == 3 {
			e.on.Store(false)
			time.Sleep(50 * time.Millisecond) // whileOn notices
		}
	}
	if err := r.RunOnce(context.Background()); !errors.Is(err, errSwitchedOff) {
		t.Fatalf("err = %v", err)
	}
	if _, l, s := e.md.calls(); l > 4 || s != 0 {
		t.Fatalf("%d lookups, %d posts after the switch-off", l, s)
	}
	if st := e.status(t); st.State != "" {
		t.Fatalf("a stopped run recorded %+v", st)
	}
}

// Plain http off this machine: no call at all, not_offered.
func TestRunNeedsHTTPS(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.unknowns(t, 3)
	e.md.noReport = true
	r := e.runner()
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, l, s := e.md.calls(); p+l+s != 0 {
		t.Fatalf("calls: %d pings, %d lookups, %d posts", p, l, s)
	}
	if st, _ := r.Status(context.Background()); st.State != StateNotOffered {
		t.Fatalf("status = %+v", st)
	}
}

// Mirror mode before the copy is ready: no call (not even a remote lookup).
func TestRunWaitsForTheMirror(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.unknowns(t, 3)
	r := New(e.cat, e.md, Options{On: e.on.Load, Now: e.clock.now, Ready: func() bool { return false }})
	if err := r.RunOnce(context.Background()); !errors.Is(err, errNotReady) {
		t.Fatalf("err = %v", err)
	}
	if p, l, s := e.md.calls(); p+l+s != 0 {
		t.Fatalf("calls: %d pings, %d lookups, %d posts", p, l, s)
	}
}

// The service down: failed, no lookups, tried again the next day.
func TestRunServiceUnreachable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.unknowns(t, 3)
	e.md.unreach = true
	r := e.runner()
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if _, l, s := e.md.calls(); l+s != 0 {
		t.Fatalf("%d lookups, %d posts", l, s)
	}
	if st := e.status(t); st.State != StateFailed {
		t.Fatalf("status = %+v", st)
	}
}

// 404 (an older metaserve, intake off): not_offered, a week without a call.
func TestRunNotOffered(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.unknowns(t, 3)
	e.md.postErr = meta.ErrReportNotOffered
	r := e.runner()
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := e.status(t)
	if st.State != StateNotOffered || st.QuietUntil != stamp(e.clock.now().Add(quietFor)) || st.LastSent != 0 {
		t.Fatalf("status = %+v", st)
	}
	if last, _ := e.cat.LastASINSentAt(context.Background()); last != "" {
		t.Fatalf("marked sent at %s", last)
	}
	p0, l0, s0 := e.md.calls()
	e.clock.add(3 * 24 * time.Hour)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, l, s := e.md.calls(); p != p0 || l != l0 || s != s0 {
		t.Fatal("a call within the quiet week")
	}
	e.md.postErr = nil
	e.clock.add(5 * 24 * time.Hour)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, s := e.md.calls(); s != s0+1 {
		t.Fatal("no report after the quiet week")
	}
	if st := e.status(t); st.State != StateOK || st.QuietUntil != "" || st.LastSent != 3 {
		t.Fatalf("status after = %+v", st)
	}
}

// 429 or 503: nothing marked; the next day sends them again.
func TestRunRefusedTriesNextDay(t *testing.T) {
	t.Parallel()
	for _, refusal := range []string{"rate limited (status 429)", "upstream status 503"} {
		e := newEnv(t)
		want := e.unknowns(t, 3)
		e.md.postErr = errors.New(refusal)
		r := e.runner()
		if err := r.RunOnce(context.Background()); err == nil {
			t.Fatalf("%s: no error", refusal)
		}
		if st := e.status(t); st.State != StateFailed || st.LastSent != 0 {
			t.Fatalf("%s: status = %+v", refusal, st)
		}
		if last, _ := e.cat.LastASINSentAt(context.Background()); last != "" {
			t.Fatalf("%s: marked sent", refusal)
		}
		e.md.postErr = nil
		e.clock.add(interval)
		if err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, l, s := e.md.calls(); l != 3 || s != 2 || !slices.Equal(sorted(e.md.posts[1]), want) {
			t.Fatalf("%s: %d lookups, %d posts (%v), want the same ASINs sent again without new lookups", refusal, l, s, e.md.posts)
		}
	}
}

// A failed lookup stops the lookups: confirmed unknowns are still sent, the
// rest stay unchecked for the next run.
func TestRunLookupFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	all := e.unknowns(t, 10)
	e.md.failAt = 4
	r := e.runner()
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("no error")
	}
	sent := e.md.posted()
	if len(sent) != 3 {
		t.Fatalf("sent %v, want the 3 confirmed before the failure", sent)
	}
	rows, _ := e.cat.UnknownASINRows(context.Background())
	if len(rows) != 3 {
		t.Fatalf("%d rows, want only the 3 asked", len(rows))
	}
	if st := e.status(t); st.State != StateFailed || st.LastSent != 3 {
		t.Fatalf("status = %+v", st)
	}
	// The next day: the 7 unasked are looked up and sent (a day after the last
	// send).
	e.md.failAt = 0
	e.clock.add(interval)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.md.posted(); !slices.Equal(sorted(got), all) {
		t.Fatalf("sent over two days %v, want %v", sorted(got), all)
	}
}

// At most maxChecks lookups a run, chunks of chunkSize, and a recheck and
// resend only after 30 days.
func TestRunBounds(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.unknowns(t, 600)
	r := e.runner()
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, l, _ := e.md.calls(); l != maxChecks {
		t.Fatalf("%d lookups, want %d", l, maxChecks)
	}
	var sizes []int
	for _, p := range e.md.posts {
		sizes = append(sizes, len(p))
	}
	if !slices.Equal(sizes, []int{200, 200, 100}) {
		t.Fatalf("chunks %v", sizes)
	}
	e.clock.add(interval)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, l, s := e.md.calls(); l != 600 || s != 4 || len(e.md.posts[3]) != 100 {
		t.Fatalf("day two: %d lookups, %d posts", l, s)
	}
	// Day 20: everything is fresh, nothing to do.
	e.clock.add(18 * 24 * time.Hour)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, l, s := e.md.calls(); l != 600 || s != 4 {
		t.Fatalf("day 20: %d lookups, %d posts, want none new", l, s)
	}
	// Day 31: the first day's are rechecked and, still unknown, sent again.
	e.clock.add(11 * 24 * time.Hour)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, l, s := e.md.calls(); l != 600+maxChecks || s != 7 {
		t.Fatalf("day 31: %d lookups, %d posts", l, s)
	}
}

// Once a day across a restart: a new Runner over the same database makes no
// call within the day, and its schedule starts a day after the last run.
func TestRunOnceADayAcrossRestart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.unknowns(t, 3)
	if err := e.runner().RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ran := e.clock.now()
	p0, l0, s0 := e.md.calls()
	e.book(t, "new", "B0NEWBOOK1", false)
	e.clock.add(2 * time.Hour)
	restarted := e.runner() // the server started again
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, l, s := e.md.calls(); p != p0 || l != l0 || s != s0 {
		t.Fatal("a restarted server ran again within the day")
	}
	if got := restarted.firstRun(context.Background()); !got.Equal(ran.Add(interval)) {
		t.Fatalf("first run after the restart at %v, want %v", got, ran.Add(interval))
	}
	if st, _ := restarted.Status(context.Background()); st.State != StateOK || st.LastSent != 3 {
		t.Fatalf("status after the restart = %+v", st)
	}
	if restarted.due(context.Background()) {
		t.Fatal("due within the day")
	}

	// Even with the run status lost, MAX(sent_at) holds the POST to once a day.
	if err := e.cat.SaveUnknownASINReportState(context.Background(), catalog.UnknownASINReport{}); err != nil {
		t.Fatal(err)
	}
	if err := e.runner().RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, l, s := e.md.calls(); l != l0+1 || s != s0 {
		t.Fatalf("%d new lookups, %d new posts: want the new book looked up, nothing sent", l-l0, s-s0)
	}
}

// A row whose ASIN no book holds any more is dropped.
func TestRunPrunes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.unknowns(t, 2)
	ctx := context.Background()
	if err := e.cat.SaveASINChecks(ctx, []catalog.ASINCheck{{ASIN: "B0GONE0001", Unknown: true}}, stamp(e.clock.now())); err != nil {
		t.Fatal(err)
	}
	if err := e.runner().RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := e.cat.UnknownASINRows(ctx)
	if _, ok := rows["B0GONE0001"]; ok || len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	if slices.Contains(e.md.posted(), "B0GONE0001") {
		t.Fatal("a gone ASIN was sent")
	}
}

// Wake runs at once when no run was made within the day.
func TestWakeRunsWhenDue(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.unknowns(t, 2)
	r := New(e.cat, e.md, Options{On: e.on.Load}) // the real clock: Run's timers
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	r.Wake()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.md.posted()) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("Wake did not run the report")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

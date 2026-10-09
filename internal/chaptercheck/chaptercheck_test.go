package chaptercheck

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// fakeSource answers RecordingChapters from a map by ASIN; err, when set,
// answers every call, and failing answers the ASINs it names with an error.
type fakeSource struct {
	byASIN  map[string]*meta.RecordingChapters
	err     error
	failing map[string]bool
	calls   atomic.Int32
}

func (f *fakeSource) RecordingChapters(_ context.Context, asin, _ string) (*meta.RecordingChapters, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	if f.failing[asin] {
		return nil, errors.New("upstream error for " + asin)
	}
	if rc, ok := f.byASIN[asin]; ok {
		return rc, nil
	}
	return nil, meta.ErrNotFound
}

type env struct {
	db  *store.DB
	cat *catalog.Catalog
	lib *catalog.Library
	src *fakeSource
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return &env{db: db, cat: cat, lib: lib, src: &fakeSource{byASIN: map[string]*meta.RecordingChapters{}}}
}

func (e *env) book(t *testing.T, path, asin string, duration float64) {
	t.Helper()
	if _, err := e.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: e.lib.ID, RelPath: path,
		Title: path, Duration: duration, ASIN: asin, AddedAt: "2020-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
}

// chapters is a community list of n chapters of length seconds each.
func chapters(n int, length float64) *meta.RecordingChapters {
	rc := &meta.RecordingChapters{WorkID: "w", RecordingID: "r"}
	for i := range n {
		rc.Chapters = append(rc.Chapters, meta.Chapter{Title: fmt.Sprintf("Part %c", 'A'+i),
			StartMS: int64(float64(i) * length * 1000), LengthMS: int64(length * 1000)})
	}
	return rc
}

func on() bool { return true }

func TestCheckFillsAChapterlessBook(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Book.m4b", "B000000001", 3600)
	e.src.byASIN["B000000001"] = chapters(4, 900)
	r := New(e.cat, e.src, "", on, nil)

	cc, err := r.Check(ctx, catalog.Ref{LibraryID: e.lib.ID, Path: "A/Book.m4b"})
	if err != nil {
		t.Fatal(err)
	}
	if cc.Status != "fill" || len(cc.Chapters) != 4 || cc.RecordingID != "r" {
		t.Fatalf("check %+v", cc)
	}
	b, err := e.cat.GetBookByPath(ctx, e.lib.ID, "A/Book.m4b")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Chapters) != 4 || b.Chapters[2].Title != "Part C" || b.ChaptersSource != catalog.ChaptersFromCommunity {
		t.Fatalf("book chapters %+v from %q", b.Chapters, b.ChaptersSource)
	}
	if r.Checking(catalog.Ref{LibraryID: e.lib.ID, Path: "A/Book.m4b"}) {
		t.Error("still marked as checking")
	}
}

func TestCheckRecordsNoMatchAndAnotherEdition(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Unknown.m4b", "B000000009", 3600)
	e.book(t, "A/Abridged.m4b", "B000000002", 1800)
	e.src.byASIN["B000000002"] = chapters(4, 900) // an hour against half of one
	r := New(e.cat, e.src, "", on, nil)

	r.pass(ctx)
	got := map[string]string{}
	for _, p := range []string{"A/Unknown.m4b", "A/Abridged.m4b"} {
		cc, err := e.cat.GetCommunityChapters(ctx, e.lib.ID, p)
		if err != nil || cc == nil {
			t.Fatalf("%s: %+v %v", p, cc, err)
		}
		got[p] = cc.Status
	}
	if got["A/Unknown.m4b"] != catalog.CommunityNoMatch || got["A/Abridged.m4b"] != "length_mismatch" {
		t.Fatalf("statuses %v", got)
	}
	// Both recorded: nothing is due until the book or its match changes.
	if due, _ := e.cat.DueChapterChecks(ctx, "2000-01-01T00:00:00Z", 10); len(due) != 0 {
		t.Fatalf("due %+v", due)
	}
	if b, _ := e.cat.GetBookByPath(ctx, e.lib.ID, "A/Abridged.m4b"); len(b.Chapters) != 0 {
		t.Fatalf("another edition's chapters were used: %+v", b.Chapters)
	}
}

func TestPassStopsWhenTheServiceFails(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := range 12 {
		e.book(t, fmt.Sprintf("A/%02d.m4b", i), fmt.Sprintf("B0000000%02d", i), 3600)
	}
	e.src.err = errors.New("upstream down")
	r := New(e.cat, e.src, "", on, nil)
	r.pass(ctx)
	if n := e.src.calls.Load(); n < maxFailStreak || n > maxFailStreak+workers {
		t.Fatalf("%d calls, want the pass to stop after %d failures", n, maxFailStreak)
	}
	// Nothing recorded: the books are still due.
	if due, _ := e.cat.DueChapterChecks(ctx, "2000-01-01T00:00:00Z", 20); len(due) != 12 {
		t.Fatalf("%d due, want 12", len(due))
	}
	// Off: the background checks nothing.
	e.src.err, e.src.calls = nil, atomic.Int32{}
	off := New(e.cat, e.src, "", func() bool { return false }, nil)
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { off.Run(run); close(done) }()
	off.Kick()
	time.Sleep(50 * time.Millisecond)
	stop()
	<-done
	if n := e.src.calls.Load(); n != 0 {
		t.Fatalf("%d calls with metadata off", n)
	}
}

func TestCheckSnapsToPausesWithFFmpeg(t *testing.T) {
	if !media.HasFFmpeg("ffmpeg") {
		t.Skip("ffmpeg not available")
	}
	e := newEnv(t)
	ctx := context.Background()
	// Four 30 s chapters, each ending in 2 s of silence; the copy has lost its
	// first 3 s, so the community's times run 3 s ahead of its audio.
	var inputs []string
	var labels string
	for i := range 4 {
		inputs = append(inputs, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:duration=28", 300+100*i),
			"-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono:d=2")
		labels += fmt.Sprintf("[%d][%d]", 2*i, 2*i+1)
	}
	path := filepath.Join(e.lib.Root, "A", "Book.m4a")
	args := append([]string{"-nostdin", "-loglevel", "error"}, inputs...)
	args = append(args, "-filter_complex", labels+"concat=n=8:v=0:a=1,atrim=start=3", "-c:a", "aac", "-y", path)
	if err := exec.Command("mkdir", "-p", filepath.Dir(path)).Run(); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not build the fixture: %v\n%s", err, out)
	}
	e.book(t, "A/Book.m4a", "B000000001", 117)
	e.src.byASIN["B000000001"] = chapters(4, 30)
	r := New(e.cat, e.src, "ffmpeg", on, nil)
	cc, err := r.Check(ctx, catalog.Ref{LibraryID: e.lib.ID, Path: "A/Book.m4a"})
	if err != nil {
		t.Fatal(err)
	}
	if cc.Status != "fill" || cc.Detail.Snapped != 3 {
		t.Fatalf("check %s, detail %+v", cc.Status, cc.Detail)
	}
	// Each chapter starts just before its tone: 30 s apart, 3 s early.
	for i, ch := range cc.Chapters[1:] {
		want := float64(30*(i+1)) - 3 - 0.3
		if math.Abs(ch.Start-want) > 0.15 {
			t.Errorf("chapter %d starts at %.3f, want about %.3f", i+1, ch.Start, want)
		}
	}
}

func TestStartChecksInTheBackgroundOnce(t *testing.T) {
	e := newEnv(t)
	e.book(t, "A/Book.m4b", "B000000001", 3600)
	block := make(chan struct{})
	src := &blockingSource{fakeSource: e.src, block: block}
	e.src.byASIN["B000000001"] = chapters(4, 900)
	r := New(e.cat, src, "", on, nil)
	ref := catalog.Ref{LibraryID: e.lib.ID, Path: "A/Book.m4b"}
	if !r.Start(context.Background(), ref) {
		t.Fatal("Start refused an idle book")
	}
	// Checking from the moment Start returns; a second start or check is refused.
	if !r.Checking(ref) || r.Start(context.Background(), ref) {
		t.Fatal("a book being checked was not marked, or was started twice")
	}
	if _, err := r.Check(context.Background(), ref); !errors.Is(err, ErrBusy) {
		t.Fatalf("Check while busy: %v", err)
	}
	close(block)
	r.wg.Wait()
	if r.Checking(ref) {
		t.Fatal("still checking after it finished")
	}
	if cc, _ := e.cat.GetCommunityChapters(context.Background(), e.lib.ID, ref.Path); cc == nil || cc.Status != "fill" {
		t.Fatalf("recorded %+v", cc)
	}
}

// blockingSource holds each answer until block closes.
type blockingSource struct {
	*fakeSource
	block chan struct{}
}

func (b *blockingSource) RecordingChapters(ctx context.Context, asin, isbn string) (*meta.RecordingChapters, error) {
	<-b.block
	return b.fakeSource.RecordingChapters(ctx, asin, isbn)
}

func TestRecheckOfTheSameListOnlyRenews(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Book.m4b", "B000000001", 3600)
	e.src.byASIN["B000000001"] = chapters(4, 900)
	r := New(e.cat, e.src, "", on, nil)
	ref := catalog.Ref{LibraryID: e.lib.ID, Path: "A/Book.m4b"}
	if _, err := r.Check(ctx, ref); err != nil {
		t.Fatal(err)
	}
	first, _ := e.cat.GetCommunityChapters(ctx, e.lib.ID, ref.Path)
	// A prober that would fail the test if the second check fitted again.
	r.ffmpeg = "/nonexistent/ffmpeg"
	again, err := r.Check(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "fill" || again.CheckedAt != first.CheckedAt {
		t.Fatalf("renewed %+v", again)
	}
	if cc, _ := e.cat.GetCommunityChapters(ctx, e.lib.ID, ref.Path); cc.CheckedAt == first.CheckedAt {
		t.Fatal("the renewal did not move checked_at")
	}
	// A changed list is fitted again.
	e.src.byASIN["B000000001"] = chapters(3, 1200)
	r.ffmpeg = ""
	if cc, err := r.Check(ctx, ref); err != nil || cc.Detail.CommunityChapters != 3 {
		t.Fatalf("changed list: %+v, %v", cc, err)
	}
}

// Books the service keeps failing for wait their turn: the next pass checks the
// books behind them.
func TestFailingBooksDontHoldUpTheRest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.src.failing = map[string]bool{}
	for i := range 7 { // enough that the first pass stops before the last book
		asin := fmt.Sprintf("B0000000%02d", i)
		e.book(t, fmt.Sprintf("A/%02d.m4b", i), asin, 3600)
		e.src.failing[asin] = true
	}
	e.book(t, "A/Good.m4b", "B000000099", 3600)
	e.src.byASIN["B000000099"] = chapters(4, 900)
	r := New(e.cat, e.src, "", on, nil)
	if !r.pass(ctx) {
		t.Fatal("a pass the service kept failing reported no failure")
	}
	if cc, _ := e.cat.GetCommunityChapters(ctx, e.lib.ID, "A/Good.m4b"); cc != nil {
		t.Fatalf("the first pass reached the good book: %+v", cc)
	}
	r.pass(ctx)
	if cc, _ := e.cat.GetCommunityChapters(ctx, e.lib.ID, "A/Good.m4b"); cc == nil || cc.Status != "fill" {
		t.Fatalf("the good book was held up: %+v", cc)
	}
	// An hour on, the failed books are tried again.
	later := time.Now().Add(retryAfter + time.Minute)
	r.now = func() time.Time { return later }
	before := e.src.calls.Load()
	r.pass(ctx)
	if e.src.calls.Load() == before {
		t.Fatal("the failed books were never tried again")
	}
}

// A check asked for fits again even when the list and the audio are unchanged.
func TestStartFitsAgain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Book.m4b", "B000000001", 3600)
	e.src.byASIN["B000000001"] = chapters(4, 900)
	r := New(e.cat, e.src, "", on, nil)
	ref := catalog.Ref{LibraryID: e.lib.ID, Path: "A/Book.m4b"}
	if _, err := r.Check(ctx, ref); err != nil {
		t.Fatal(err)
	}
	// Mark the recorded fit, so a renewal (which leaves it) shows.
	if _, err := e.db.ExecContext(ctx, `UPDATE community_chapters SET detail = '{}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Check(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if cc, _ := e.cat.GetCommunityChapters(ctx, e.lib.ID, ref.Path); cc.Detail.CommunityChapters != 0 {
		t.Fatalf("the background's recheck fitted again: %+v", cc.Detail)
	}
	if !r.Start(ctx, ref) {
		t.Fatal("Start refused an idle book")
	}
	r.wg.Wait()
	if cc, _ := e.cat.GetCommunityChapters(ctx, e.lib.ID, ref.Path); cc.Detail.CommunityChapters != 4 {
		t.Fatalf("the asked-for check only renewed: %+v", cc.Detail)
	}
}

// The prober reads the library's files through SafeJoin: a path inside the root
// reaches ffmpeg, one outside it never does.
func TestProberStaysInTheLibrary(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := New(e.cat, e.src, "/nonexistent/ffmpeg", on, nil)
	probe, err := r.prober(ctx, e.lib.ID)
	if err != nil || probe == nil {
		t.Fatalf("prober %v", err)
	}
	if _, err := probe(ctx, "../outside.m4a", 0, 5); !errors.Is(err, library.ErrOutsideRoot) {
		t.Fatalf("outside the root: %v, want ErrOutsideRoot", err)
	}
	// Inside the root the path is allowed: what fails is the (missing) ffmpeg.
	if _, err := probe(ctx, "A/Book.m4a", 0, 5); err == nil || errors.Is(err, library.ErrOutsideRoot) {
		t.Fatalf("inside the root: %v, want ffmpeg's error", err)
	}
}

func TestStartReportsAFailedCheck(t *testing.T) {
	e := newEnv(t)
	e.book(t, "A/Book.m4b", "B000000001", 3600)
	e.src.err = errors.New("upstream down")
	r := New(e.cat, e.src, "", on, nil)
	ref := catalog.Ref{LibraryID: e.lib.ID, Path: "A/Book.m4b"}
	r.Start(context.Background(), ref)
	r.wg.Wait()
	if !r.Failed(ref) {
		t.Fatal("a failed check is not reported")
	}
	e.src.err = nil
	e.src.byASIN["B000000001"] = chapters(4, 900)
	r.Start(context.Background(), ref)
	r.wg.Wait()
	if r.Failed(ref) {
		t.Fatal("a check that worked still reads as failed")
	}
}

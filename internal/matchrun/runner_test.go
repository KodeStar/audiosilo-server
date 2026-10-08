package matchrun

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// fakeMatcher answers Candidates by the book's title (or, for a repick, its ASIN).
type fakeMatcher struct {
	mu      sync.Mutex
	byTitle map[string][]meta.MatchCandidate
	byASIN  map[string][]meta.MatchCandidate
	fail    bool          // every query fails
	block   chan struct{} // when set, each query waits on it (or the context)
	queries []meta.MatchQuery
}

func (f *fakeMatcher) Candidates(ctx context.Context, q meta.MatchQuery) ([]meta.MatchCandidate, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	block, fail := f.block, f.fail
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail || q.Title == "Down" {
		return nil, errors.New("upstream status 500")
	}
	if q.ASIN != "" {
		return f.byASIN[q.ASIN], nil
	}
	return f.byTitle[q.Title], nil
}

type env struct {
	cat     *catalog.Catalog
	admin   int64
	matcher *fakeMatcher
	runner  *Runner
	lib     int64
	covers  atomic.Int32
	enabled atomic.Bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "audiosilo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &env{cat: catalog.New(db, time.Now), matcher: &fakeMatcher{byTitle: map[string][]meta.MatchCandidate{}, byASIN: map[string][]meta.MatchCandidate{}}}
	e.enabled.Store(true)
	admin, err := auth.New(db, time.Now).CreateUser(ctx, "admin", "correct horse battery", "admin")
	if err != nil {
		t.Fatal(err)
	}
	e.admin = admin.ID
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	e.lib = lib.ID
	save := func(_ context.Context, _ int64, _, url string, _ int64) error {
		if url == "https://c/broken.jpg" {
			return errors.New("cover unavailable")
		}
		e.covers.Add(1)
		return nil
	}
	e.runner = New(e.cat, e.matcher, save, e.enabled.Load, nil)
	return e
}

// book indexes a book; hasCover nil leaves it unchecked.
func (e *env) book(t *testing.T, path, title, asin string, hasCover *bool) {
	t.Helper()
	b := &catalog.Book{LibraryID: e.lib, RelPath: path, IsFolder: true, Title: title, Author: "Andy Weir",
		ASIN: asin, Duration: 634 * 60, AddedAt: "2020-01-01", HasCover: hasCover}
	if _, err := e.cat.UpsertBook(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

func cand(work string, score int, cover string) meta.MatchCandidate {
	c := martian
	c.WorkID, c.Score = work, score
	c.Recordings = slices.Clone(martian.Recordings)
	c.Recordings[0].CoverURL = cover
	return c
}

func (e *env) start(t *testing.T, mode, region string) *catalog.MatchRun {
	t.Helper()
	run, err := e.runner.Start(context.Background(), context.Background(), StartOptions{Mode: mode, Region: region, UserID: 0})
	if err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	got, err := e.cat.GetMatchRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *env) items(t *testing.T, run *catalog.MatchRun, outcome string) []ItemView {
	t.Helper()
	items, _, err := e.runner.Items(context.Background(), run, outcome, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

var no = new(bool)

func TestRunMatchesReviewsAndApplies(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Confident", "Confident", "", no) // no cover: fill takes the community's
	e.book(t, "A/Close", "Close", "", nil)
	e.book(t, "A/Nothing", "Nothing", "", nil)
	e.book(t, "A/Down", "Down", "", nil)
	e.book(t, "A/Matched", "Matched", "B0OLD00001", nil) // has an ASIN: not a run's
	e.book(t, "A/Ignored", "Ignored", "", nil)
	if err := e.cat.IgnoreIssue(ctx, catalog.IssueUnmatched, []catalog.Ref{{LibraryID: e.lib, Path: "A/Ignored"}}, e.admin); err != nil {
		t.Fatal(err)
	}
	e.matcher.byTitle["Confident"] = []meta.MatchCandidate{cand("the-martian", 95, "https://c/rec.jpg"), cand("artemis", 60, "")}
	e.matcher.byTitle["Close"] = []meta.MatchCandidate{cand("the-martian", 92, ""), cand("artemis", 88, "")}
	e.matcher.byTitle["Ignored"] = []meta.MatchCandidate{cand("the-martian", 99, "")}

	run := e.start(t, catalog.MatchModeMatch, "uk")
	if run.Status != catalog.MatchReady || run.Total != 4 || run.Done != 4 || run.Region != "uk" {
		t.Fatalf("run = %+v", run)
	}
	if c := run.Counts; c.Auto != 1 || c.Pending != 1 || c.Review != 1 || c.None != 1 || c.Error != 1 {
		t.Fatalf("counts = %+v", c)
	}
	// Every query is a bulk one: the preferred region, two works at most.
	for _, q := range e.matcher.queries {
		if q.Region != "uk" || q.Limit != bulkCandidates || q.Path == "" {
			t.Fatalf("query = %+v", q)
		}
	}

	auto := e.items(t, run, catalog.OutcomeAuto)
	if len(auto) != 1 || auto[0].Path != "A/Confident" || auto[0].Score != 95 || auto[0].RunnerUp != 60 ||
		auto[0].Proposal.Values["asin"] != "B0UK000001" || auto[0].Proposal.ASINRegion != "uk" || auto[0].Book.Title != "Confident" {
		t.Fatalf("auto = %+v", auto)
	}
	ch := auto[0].Changes
	if !slices.Equal(ch[ScopeIDs].Fields, []string{"asin", "isbn"}) || ch[ScopeIDs].Cover ||
		!slices.Contains(ch[ScopeFill].Fields, "narrator") || !ch[ScopeFill].Cover ||
		!slices.Contains(ch[ScopeOverwrite].Fields, "title") {
		t.Fatalf("changes = %+v", ch)
	}
	review := e.items(t, run, catalog.OutcomeReview)
	if len(review) != 1 || review[0].Path != "A/Close" {
		t.Fatalf("review = %+v", review)
	}
	if errs := e.items(t, run, catalog.OutcomeError); len(errs) != 1 || errs[0].Detail != ErrCodeUnavailable {
		t.Fatalf("errors = %+v", errs)
	}

	// Apply the confident one and the reviewed one the admin chose, filling gaps.
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill, Include: []int64{review[0].ID}, UserID: e.admin}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	run, _ = e.cat.GetMatchRun(ctx, run.ID)
	if run.Status != catalog.MatchApplied || run.Scope != ScopeFill || run.ApplyTotal != 2 || run.ApplyDone != 2 ||
		run.Counts.Applied != 2 || run.Counts.Pending != 0 || run.AppliedAt == nil {
		t.Fatalf("applied run = %+v", run)
	}
	st, err := e.cat.BookMatchState(ctx, e.lib, "A/Confident")
	if err != nil {
		t.Fatal(err)
	}
	f := st.Fields
	if f["asin"].Value != "B0UK000001" || f["asin"].Source != catalog.SourceCommunity || f["asin"].EditedBy != "admin" ||
		f["narrator"].Value != "R. C. Bray" ||
		f["title"].Value != "Confident" || f["series"].Value != "Mars" {
		t.Fatalf("applied fields = %+v", f)
	}
	if e.covers.Load() != 1 {
		t.Fatalf("covers saved = %d, want 1 (only the book with none)", e.covers.Load())
	}
	// A run applied is done: a second apply is refused.
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill}); !errors.Is(err, catalog.ErrRunNotReady) {
		t.Fatalf("second apply = %v, want ErrRunNotReady", err)
	}
	// The books it matched are matched now: a new run doesn't take them again.
	run = e.start(t, catalog.MatchModeMatch, "uk")
	if run.Total != 2 {
		t.Fatalf("next run total = %d, want the 2 still unmatched", run.Total)
	}
}

func TestApplyExcludesAndSkips(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/One", "One", "", nil)
	e.book(t, "A/Two", "Two", "", nil)
	e.book(t, "A/Gone", "Gone", "", nil)
	for _, title := range []string{"One", "Two", "Gone"} {
		e.matcher.byTitle[title] = []meta.MatchCandidate{cand("the-martian", 97, "")}
	}
	run := e.start(t, catalog.MatchModeMatch, "")
	items := e.items(t, run, catalog.OutcomeAuto)
	if len(items) != 3 {
		t.Fatalf("auto = %d", len(items))
	}
	if _, err := e.cat.DeleteBooksNotIn(ctx, e.lib, map[string]bool{"A/One": true, "A/Two": true}); err != nil {
		t.Fatal(err)
	}
	var two int64
	for _, it := range items {
		if it.Path == "A/Two" {
			two = it.ID
		}
	}
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeIDs, Exclude: []int64{two}}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	run, _ = e.cat.GetMatchRun(ctx, run.ID)
	if run.Counts.Applied != 1 || run.Counts.Skipped != 1 || run.ApplyTotal != 2 {
		t.Fatalf("run = %+v", run)
	}
	st, _ := e.cat.BookMatchState(ctx, e.lib, "A/Two")
	if st.Fields["asin"].Value != "" {
		t.Fatalf("an excluded book was matched: %+v", st.Fields["asin"])
	}
	st, _ = e.cat.BookMatchState(ctx, e.lib, "A/One")
	if st.Fields["asin"].Value == "" || st.Fields["narrator"].Value != "" {
		t.Fatalf("ids scope = %+v", st.Fields)
	}
}

func TestRunStopsWhenTheServiceIsDown(t *testing.T) {
	e := newEnv(t)
	for i := range 12 {
		e.book(t, "A/"+string(rune('a'+i)), "Book", "", nil)
	}
	e.matcher.fail = true
	run := e.start(t, catalog.MatchModeMatch, "")
	if run.Status != catalog.MatchFailed || run.Error != ErrCodeUnavailable || run.Done >= 12 {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunStopsWhenMetadataTurnsOff(t *testing.T) {
	e := newEnv(t)
	e.book(t, "A/a", "Book", "", nil)
	e.enabled.Store(false)
	run := e.start(t, catalog.MatchModeMatch, "")
	if run.Status != catalog.MatchFailed || run.Error != ErrCodeMetadataOff {
		t.Fatalf("run = %+v", run)
	}
}

func TestCancelAndBusy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/a", "Book", "", nil)
	e.book(t, "A/b", "Book", "", nil)
	e.matcher.block = make(chan struct{})
	run, err := e.runner.Start(ctx, ctx, StartOptions{Mode: catalog.MatchModeMatch})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.runner.Start(ctx, ctx, StartOptions{Mode: catalog.MatchModeMatch}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second start = %v, want ErrBusy", err)
	}
	if _, err := e.runner.Clear(ctx, 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("clear while matching = %v, want ErrBusy", err)
	}
	if e.runner.Cancel(run.ID + 1) {
		t.Fatal("cancelled a run that isn't working")
	}
	if !e.runner.Cancel(run.ID) {
		t.Fatal("cancel refused")
	}
	e.runner.Wait()
	got, _ := e.cat.GetMatchRun(ctx, run.ID)
	if got.Status != catalog.MatchCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
	// A cancelled run can't be applied, and the runner is free again.
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeIDs}); !errors.Is(err, catalog.ErrRunNotReady) {
		t.Fatalf("apply cancelled = %v", err)
	}
	close(e.matcher.block)
	if got := e.start(t, catalog.MatchModeMatch, ""); got.Status != catalog.MatchReady {
		t.Fatalf("next run = %+v", got)
	}
	// Free again, a clear goes through and takes the runs with it.
	if cleared, err := e.runner.Clear(ctx, 0); err != nil || cleared.Runs != 2 {
		t.Fatalf("clear = %+v %v, want both runs dropped", cleared, err)
	}
}

// TestServerStopLeavesTheRun: a run whose server stops is left as it was, for the
// next start to settle (InterruptMatchRuns), not recorded as cancelled.
func TestServerStopLeavesTheRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/a", "Book", "", nil)
	e.matcher.block = make(chan struct{})
	base, stop := context.WithCancel(ctx)
	run, err := e.runner.Start(ctx, base, StartOptions{Mode: catalog.MatchModeMatch})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	e.runner.Wait()
	if got, _ := e.cat.GetMatchRun(ctx, run.ID); got.Status != catalog.MatchMatching {
		t.Fatalf("status = %s, want still matching", got.Status)
	}
	if err := e.cat.InterruptMatchRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.cat.GetMatchRun(ctx, run.ID); got.Status != catalog.MatchInterrupted {
		t.Fatalf("status = %s, want interrupted", got.Status)
	}
}

func TestRepick(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/Community", "Community", "", nil)
	e.book(t, "A/Edited", "Edited", "", nil)
	e.book(t, "A/Tagged", "Tagged", "B0US000001", nil)
	set := func(path, source string) {
		if err := e.cat.EditBook(ctx, e.lib, path, catalog.BookEdit{Set: map[string]string{"asin": "B0US000001"}, Source: source}); err != nil {
			t.Fatal(err)
		}
	}
	set("A/Community", catalog.SourceCommunity)
	set("A/Edited", catalog.SourceEdited)
	hit := cand("the-martian", 100, "")
	hit.RecordingID = "bray"
	e.matcher.byASIN["B0US000001"] = []meta.MatchCandidate{hit}

	run := e.start(t, catalog.MatchModeRepick, "uk")
	if run.Total != 1 || run.Counts.Auto != 1 || run.Status != catalog.MatchReady {
		t.Fatalf("repick run = %+v", run)
	}
	items := e.items(t, run, "")
	if len(items) != 1 || items[0].Proposal.Values["asin"] != "B0UK000001" ||
		!slices.Equal(items[0].Changes[ScopeIDs].Fields, []string{"asin"}) {
		t.Fatalf("items = %+v", items)
	}
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeOverwrite}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	st, _ := e.cat.BookMatchState(ctx, e.lib, "A/Community")
	if st.Fields["asin"].Value != "B0UK000001" || st.Fields["narrator"].Value != "" {
		t.Fatalf("repicked = %+v", st.Fields)
	}

	// Already the preferred marketplace's: nothing to record.
	run = e.start(t, catalog.MatchModeRepick, "us")
	if run.Total != 1 || len(e.items(t, run, "")) != 0 {
		t.Fatalf("nothing to repick = %+v", run)
	}

	// The recording is listed twice in the UK store and the book has the second:
	// it is the UK store's already, nothing to switch.
	twice := hit
	twice.Recordings = slices.Clone(hit.Recordings)
	twice.Recordings[0].ASINRefs = append(slices.Clone(hit.Recordings[0].ASINRefs), meta.ASINRef{Region: "uk", ASIN: "B0UK000002"})
	e.matcher.byASIN["B0UK000002"] = []meta.MatchCandidate{twice}
	if err := e.cat.EditBook(ctx, e.lib, "A/Community", catalog.BookEdit{Set: map[string]string{"asin": "B0UK000002"}, Source: catalog.SourceCommunity}); err != nil {
		t.Fatal(err)
	}
	if run = e.start(t, catalog.MatchModeRepick, "uk"); run.Total != 1 || len(e.items(t, run, "")) != 0 {
		t.Fatalf("second UK listing = %+v", run)
	}
}

// TestApplyStopsWhenMetadataTurnsOff: an apply hands out no more books once
// community metadata is off (a cover is an outbound fetch, and Stop answers
// metadata_off then); the run is ready again, the rest still to apply.
func TestApplyStopsWhenMetadataTurnsOff(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/a", "Book", "", no)
	e.matcher.byTitle["Book"] = []meta.MatchCandidate{cand("the-martian", 97, "https://c/rec.jpg")}
	run := e.start(t, catalog.MatchModeMatch, "")
	e.enabled.Store(false)
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	run, _ = e.cat.GetMatchRun(ctx, run.ID)
	if run.Status != catalog.MatchReady || run.Counts.Pending != 1 || e.covers.Load() != 0 {
		t.Fatalf("run = %+v, covers %d; want ready with the book still to apply", run, e.covers.Load())
	}
}

// TestCancelledApplyMarksWhatWentIn: a book whose writes all went in before the
// admin stopped the apply counts as applied, not as one to take again.
func TestCancelledApplyMarksWhatWentIn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/a", "Book", "", no)
	e.matcher.byTitle["Book"] = []meta.MatchCandidate{cand("the-martian", 97, "https://c/rec.jpg")}
	run := e.start(t, catalog.MatchModeMatch, "")
	// The cover, the item's last write, lands as the admin presses Stop.
	e.runner.saveCover = func(context.Context, int64, string, string, int64) error {
		e.runner.Cancel(run.ID)
		return nil
	}
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	run, _ = e.cat.GetMatchRun(ctx, run.ID)
	if run.Status != catalog.MatchReady || run.Counts.Applied != 1 || run.Counts.Pending != 0 {
		t.Fatalf("run = %+v, want ready with the book marked applied", run)
	}
}

func TestApplyCoverFailureAndInterruptedApply(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.book(t, "A/a", "Book", "", no)
	e.matcher.byTitle["Book"] = []meta.MatchCandidate{cand("the-martian", 97, "https://c/broken.jpg")}
	run := e.start(t, catalog.MatchModeMatch, "")
	if _, err := e.runner.Apply(ctx, ctx, run.ID, ApplyOptions{Scope: ScopeFill}); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	// The fields went on; the cover that failed is noted, not fatal.
	items := e.items(t, run, catalog.OutcomeAuto)
	if items[0].Applied != catalog.ItemApplied || items[0].Detail != "cover_failed" {
		t.Fatalf("item = %+v", items[0])
	}

	// A run a stopped server left applying is ready again at the next start.
	e.book(t, "A/b", "Book", "", nil)
	run = e.start(t, catalog.MatchModeMatch, "")
	if err := e.cat.BeginApply(ctx, run.ID, ScopeFill, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.InterruptMatchRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.cat.GetMatchRun(ctx, run.ID); got.Status != catalog.MatchReady {
		t.Fatalf("status = %s, want ready", got.Status)
	}
}

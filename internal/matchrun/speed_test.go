package matchrun

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// A run reads a stub runner-up exactly as the whole candidate: Classify takes only its
// score and whether an identifier found it, whatever the rest of it says.
func TestClassifyReadsOnlyARunnerUpsScoreAndHit(t *testing.T) {
	hit := &meta.MatchReasons{Identifier: "asin"}
	for name, tc := range map[string]struct {
		best, second      int
		bestHit, otherHit *meta.MatchReasons
		want              string
	}{
		"far ahead":                 {97, 71, &meta.MatchReasons{}, &meta.MatchReasons{}, catalog.OutcomeAuto},
		"too close":                 {97, 90, &meta.MatchReasons{}, &meta.MatchReasons{}, catalog.OutcomeReview},
		"weak":                      {40, 20, &meta.MatchReasons{}, &meta.MatchReasons{}, catalog.OutcomeNone},
		"identifier needs no lead":  {100, 100, hit, &meta.MatchReasons{}, catalog.OutcomeAuto},
		"a runner-up's hit reviews": {100, 100, hit, hit, catalog.OutcomeReview},
	} {
		t.Run(name, func(t *testing.T) {
			best := cand("best", tc.best, "")
			best.Reasons = tc.bestHit
			full := cand("other", tc.second, "")
			full.Reasons = tc.otherHit
			stub := meta.MatchCandidate{WorkID: "other", Score: tc.second, Reasons: tc.otherHit}
			p := Propose(&best, meta.DefaultRecording(&best, 634*60, "", nil))
			gotFull, s1, r1 := Classify([]meta.MatchCandidate{best, full}, p, 634*60)
			gotStub, s2, r2 := Classify([]meta.MatchCandidate{best, stub}, p, 634*60)
			if gotFull != tc.want || gotStub != gotFull || s1 != s2 || r1 != r2 {
				t.Fatalf("full %s %d/%d, stub %s %d/%d, want %s", gotFull, s1, r1, gotStub, s2, r2, tc.want)
			}
		})
	}
}

// A run asks for the lean answer; a repick and a refresh, which go through no
// works/match, don't.
func TestOnlyAMatchRunAsksForTheBestOnly(t *testing.T) {
	e := newEnv(t)
	e.book(t, "A/one", "The Martian", "", nil)
	e.start(t, catalog.MatchModeMatch, "us")
	e.book(t, "A/two", "Another", "B0TWO00001", nil)
	e.start(t, catalog.MatchModeRefresh, "us")
	if len(e.matcher.queries) != 2 {
		t.Fatalf("queries = %+v", e.matcher.queries)
	}
	if q := e.matcher.queries[0]; !q.BestOnly || q.NoDetailsBelow != ReviewFloor || q.Limit != bulkCandidates {
		t.Fatalf("a run's query = %+v", q)
	}
	if q := e.matcher.queries[1]; q.BestOnly {
		t.Fatalf("a refresh's query = %+v", q)
	}
}

const speedWork = `{"id":"the-martian","title":"The Martian","language":"en","authors":[{"id":"aw","name":"Andy Weir"}],"series":[],
	"recordings":[{"id":"bray","narrators":[{"id":"rb","name":"R. C. Bray"}],"runtime_min":634,"asin":[{"region":"us","asin":"B0US000001"}]}]}`

// speedResult is a works/match result.
func speedResult(id string, score int, reasons, extra string) string {
	r := `{"kind":"work","id":"` + id + `","score":` + strconv.Itoa(score) + `,"reasons":{` + reasons + `}`
	if extra != "" {
		r += "," + extra
	}
	return r + "}"
}

// speedMeta is a metaserve for a real Service under a run: answer gives the works/match
// response for the nth request, and works/{id} serves speedWork, counted.
type speedMeta struct {
	answer func(n int32, w http.ResponseWriter) bool
	match  atomic.Int32
	works  atomic.Int32
}

func (m *speedMeta) service(t *testing.T) *meta.Service {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/works/match", func(w http.ResponseWriter, _ *http.Request) {
		if !m.answer(m.match.Add(1), w) {
			_, _ = w.Write([]byte(`{"results":[` + speedResult("the-martian", 97, `"title":1`, `"work":`+speedWork) + `]}`))
		}
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.works.Add(1)
		if r.PathValue("id") != "the-martian" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(speedWork))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return meta.NewService(srv.URL, nil)
}

// runOver is a run over count books with svc as its matcher, as it ends.
func runOver(t *testing.T, e *env, svc *meta.Service, count int) *catalog.MatchRun {
	t.Helper()
	for i := range count {
		e.book(t, "A/b"+strconv.Itoa(i), "The Martian", "", nil)
	}
	e.runner = New(e.cat, svc, nil, e.enabled.Load, nil)
	return e.start(t, catalog.MatchModeMatch, "us")
}

// The same outcome through a metaserve that inlines and through one that doesn't, the
// second paying one work per book and the first none.
func TestRunThroughTheServiceFallsBackWithoutExpansion(t *testing.T) {
	plain := func(_ int32, w http.ResponseWriter) bool {
		_, _ = w.Write([]byte(`{"results":[` + speedResult("the-martian", 97, `"title":1`, "") + `,` + speedResult("other", 40, `"title":0.5`, "") + `]}`))
		return true
	}
	inlined := func(_ int32, w http.ResponseWriter) bool {
		_, _ = w.Write([]byte(`{"results":[` + speedResult("the-martian", 97, `"title":1`, `"work":`+speedWork) + `,` + speedResult("other", 40, `"title":0.5`, "") + `]}`))
		return true
	}
	outcome := func(answer func(int32, http.ResponseWriter) bool, wantWorks int32) []byte {
		e := newEnv(t)
		m := &speedMeta{answer: answer}
		run := runOver(t, e, m.service(t), 3)
		if run.Status != catalog.MatchReady || m.works.Load() != wantWorks {
			t.Fatalf("run = %+v, works read %d, want %d", run, m.works.Load(), wantWorks)
		}
		items := e.items(t, run, "")
		if len(items) != 3 || items[0].Outcome != catalog.OutcomeAuto {
			t.Fatalf("items = %+v", items)
		}
		proposal := items[0].Proposal
		proposal.WebURL = "" // each stub is on its own port
		b, err := json.Marshal(proposal)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	withWork, without := outcome(inlined, 0), outcome(plain, 3)
	if string(withWork) != string(without) {
		t.Fatalf("proposals differ:\n%s\n%s", withWork, without)
	}
}

// A runner-up an identifier found holds a best one found too for review, from the
// stub alone, and without one the best goes through.
func TestRunnerUpIdentifierHitSendsToReview(t *testing.T) {
	for name, tc := range map[string]struct {
		reasons string
		want    string
	}{
		"runner-up found too": {`"identifier":"asin"`, catalog.OutcomeReview},
		"runner-up not found": {`"title":1`, catalog.OutcomeAuto},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			m := &speedMeta{answer: func(_ int32, w http.ResponseWriter) bool {
				_, _ = w.Write([]byte(`{"results":[` + speedResult("the-martian", 100, `"identifier":"asin"`, `"recording_id":"bray","work":`+speedWork) + `,` +
					speedResult("other", 100, tc.reasons, "") + `]}`))
				return true
			}}
			run := runOver(t, e, m.service(t), 1)
			items := e.items(t, run, "")
			if len(items) != 1 || items[0].Outcome != tc.want || items[0].Proposal.WorkID == "" {
				t.Fatalf("items = %+v, want %s", items, tc.want)
			}
			if m.works.Load() != 0 {
				t.Fatalf("%d works fetched, want none: the stub is enough", m.works.Load())
			}
		})
	}
}

// A weak best costs no fetch without expansion, and records what it did with one.
func TestRunWeakBestFetchesNothing(t *testing.T) {
	e := newEnv(t)
	m := &speedMeta{answer: func(_ int32, w http.ResponseWriter) bool {
		_, _ = w.Write([]byte(`{"results":[` + speedResult("the-martian", 40, `"title":0.4`, "") + `]}`))
		return true
	}}
	run := runOver(t, e, m.service(t), 2)
	items := e.items(t, run, "")
	if len(items) != 2 || items[0].Outcome != catalog.OutcomeNone || items[0].Detail != DetailWeakMatch || items[0].Score != 40 || len(items[0].Proposal.Values) != 0 {
		t.Fatalf("items = %+v", items)
	}
	if m.works.Load() != 0 {
		t.Fatalf("%d works fetched for a weak best", m.works.Load())
	}
}

// A burst of 503s (metaserve past a match's budget) is waited out: no book fails, and
// nothing stops the run.
func TestRunContinuesThrough503(t *testing.T) {
	e := newEnv(t)
	m := &speedMeta{answer: func(n int32, w http.ResponseWriter) bool {
		if n > 3 { // however they fall, one fewer than a book's four asks
			return false
		}
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}}
	run := runOver(t, e, m.service(t), 12)
	if run.Status != catalog.MatchReady || run.Error != "" {
		t.Fatalf("run = %+v", run)
	}
	for _, it := range e.items(t, run, "") {
		if it.Outcome != catalog.OutcomeAuto {
			t.Fatalf("item = %+v", it)
		}
	}
}

// A service that keeps answering 503 still stops the run, by the streak.
func TestRunStopsOnPersistent503(t *testing.T) {
	e := newEnv(t)
	m := &speedMeta{answer: func(_ int32, w http.ResponseWriter) bool {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}}
	const books = 60
	run := runOver(t, e, m.service(t), books)
	if run.Status != catalog.MatchFailed || run.Error != ErrCodeUnavailable {
		t.Fatalf("run = %+v", run)
	}
	if run.Done >= books {
		t.Fatalf("run worked through all %d books: %+v", books, run)
	}
	// Each book asks four times at most, so a stop after five in a row leaves most
	// books never asked about.
	if got := int(m.match.Load()); got >= books {
		t.Fatalf("%d requests, want the run stopped well before %d", got, books)
	}
}

// Cancelling stops a run promptly, even with every worker waiting out a long
// Retry-After.
func TestCancelStopsARunWaitingOutARetry(t *testing.T) {
	e := newEnv(t)
	waiting := make(chan struct{}, 64)
	m := &speedMeta{answer: func(_ int32, w http.ResponseWriter) bool {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusServiceUnavailable)
		waiting <- struct{}{}
		return true
	}}
	svc := m.service(t)
	for i := range 20 {
		e.book(t, "A/b"+strconv.Itoa(i), "The Martian", "", nil)
	}
	e.runner = New(e.cat, svc, nil, e.enabled.Load, nil)
	ctx := context.Background()
	run, err := e.runner.Start(ctx, ctx, StartOptions{Mode: catalog.MatchModeMatch, Region: "us"})
	if err != nil {
		t.Fatal(err)
	}
	<-waiting
	start := time.Now()
	if !e.runner.Cancel(run.ID) {
		t.Fatal("cancel found no run")
	}
	e.runner.Wait()
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("cancel took %v, want well under the 10 s Retry-After", took)
	}
	got, err := e.cat.GetMatchRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != catalog.MatchCancelled {
		t.Fatalf("run = %+v", got)
	}
}

// A run's requests go through its pacer: a whole run takes at least its spacing.
func TestRunIsPaced(t *testing.T) {
	e := newEnv(t)
	m := &speedMeta{answer: func(int32, http.ResponseWriter) bool { return false }}
	const books = 19
	start := time.Now()
	run := runOver(t, e, m.service(t), books)
	took := time.Since(start)
	if run.Status != catalog.MatchReady || int(m.match.Load()) != books {
		t.Fatalf("run = %+v, %d requests", run, m.match.Load())
	}
	if floor := time.Duration(books-1) * time.Second / maxRequestsPerSecond; took < floor {
		t.Fatalf("%d books in %v, want at least %v", books, took, floor)
	}
}

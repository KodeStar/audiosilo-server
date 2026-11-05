//go:build benchharness

// The matcher's benchmark harness: the LibraForge benchmark's books matched as
// a bulk match run matches them, against a local community artifact in mirror
// mode, scored against the benchmark's right answers. Never run by CI (the
// build tag); see TestLibraForge for how to run it.

package matchrun

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// lfBook is one benchmark book as data.json records it: its folder (f, under
// the library root), its file, its runtime in minutes, its ffprobe tag map and
// the right answer (ref; asin "" when it has no confirmed Audible listing).
type lfBook struct {
	F    string            `json:"f"`
	File string            `json:"file"`
	Min  float64           `json:"min"`
	Tags map[string]string `json:"tags"`
	Ref  struct {
		ASIN, Title, Author, Narrator string
	} `json:"ref"`
}

// lfResult is one book's result, written to LF_OUT for comparing two runs.
type lfResult struct {
	Path     string `json:"path"`
	Outcome  string `json:"outcome"` // auto | review | none | error
	Detail   string `json:"detail,omitempty"`
	Score    int    `json:"score"`
	RunnerUp int    `json:"runner_up"`
	Work     string `json:"work,omitempty"`
	RefWork  string `json:"ref_work,omitempty"`
	// Right: the top candidate is the right work (by the reference ASIN's
	// work, else by title and author). RecordingRight: and the recording the
	// proposal picked is the reference's (its ASIN's, else its narrator).
	Right bool `json:"right"`
	// Unjudged: the reference names no work this copy knows and no title.
	Unjudged       bool   `json:"unjudged,omitempty"`
	RecordingRight bool   `json:"recording_right"`
	Tidied         string `json:"tidied,omitempty"`
	// Title is the book's title as scanned, RefTitle the reference's.
	Title    string `json:"title"`
	RefTitle string `json:"ref_title"`
}

// localCopy is a meta.Mirror over one artifact file.
type localCopy struct{ h http.Handler }

func (l localCopy) Handler() http.Handler { return l.h }
func (localCopy) Ready() bool             { return true }

// capture records the candidates the run's Candidates call returned.
type capture struct {
	svc   *meta.Service
	cands []meta.MatchCandidate
}

func (c *capture) Candidates(ctx context.Context, q meta.MatchQuery) ([]meta.MatchCandidate, error) {
	cands, err := c.svc.Candidates(ctx, q)
	c.cands = cands
	return cands, err
}

// TestLibraForge runs the benchmark:
//
//	LF_DATA=<data.json> LF_ARTIFACT=<meta.sqlite> [LF_REGION=us] \
//	  [LF_OUT=<results.json>] [LF_WORKERS=8] [LF_LIMIT=n] [LF_BASE_URL=<metaserve>] \
//	  go test -tags benchharness -run TestLibraForge -timeout 0 -v ./internal/matchrun
//
// With LF_BASE_URL the books are matched through HTTP against that metaserve, as a
// run online is: the run's own worker count (LF_WORKERS overrides it) and pacing.
// Their time is reported on its own: the right answers are read afterwards from
// LF_ARTIFACT, in mirror mode, so nothing but the run reaches that URL and a proxy in
// front of it counts the run's requests per book.
//
// Each book is built as a scan builds it (the folder is a folder book; its
// names from the path layout and the probe's tags: library.setNames,
// metadata.applyProbe), runtime = min*60, then matched by the run's own
// matchBook (QueryFor, Candidates, Propose/DefaultRecording, Classify). The
// tags' ASIN, ISBN and language are the book's hints, as a scan reads them.
func TestLibraForge(t *testing.T) {
	dataPath, artifact := os.Getenv("LF_DATA"), os.Getenv("LF_ARTIFACT")
	if dataPath == "" || artifact == "" {
		t.Skip("set LF_DATA and LF_ARTIFACT")
	}
	region := cmp.Or(os.Getenv("LF_REGION"), "us")
	baseURL := os.Getenv("LF_BASE_URL")
	n, _ := strconv.Atoi(os.Getenv("LF_WORKERS"))
	if baseURL != "" {
		n = cmp.Or(n, workers)
	}
	n = cmp.Or(n, 8)

	raw, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	var books []lfBook
	if err := json.Unmarshal(raw, &books); err != nil {
		t.Fatal(err)
	}
	if n, _ := strconv.Atoi(os.Getenv("LF_LIMIT")); n > 0 && n < len(books) {
		books = books[:n]
	}

	db, err := query.Open(artifact, "local")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	quiet := log.New(os.Stderr, "query: ", 0)
	h := query.NewHandler(func() *query.DB { return db }, query.HandlerOptions{Logger: quiet})
	local := meta.NewService("http://127.0.0.1:9", time.Now) // never reached: the copy answers
	local.SetMirror(localCopy{h}, slog.Default())

	svc, ctx, waits := local, context.Background(), func() (int, time.Duration) { return 0, 0 }
	if baseURL != "" {
		svc = meta.NewService(baseURL, time.Now)
		ctx, waits = benchRunContext(ctx)
	}

	// The run, on its own clock.
	matched := make([]lfMatched, len(books))
	start := time.Now()
	each(n, len(books), func(i int) {
		if i > 0 && i%500 == 0 {
			t.Logf("%d books, %s", i, time.Since(start).Round(time.Second))
		}
		matched[i] = matchOne(ctx, t, svc, &books[i], region)
	})
	elapsed := time.Since(start)

	// Judged afterwards, from the local copy.
	results := make([]lfResult, len(books))
	each(8, len(books), func(i int) { results[i] = judge(local, &books[i], &matched[i], region) })

	printTable(results, region)
	failed := 0
	for _, m := range matched {
		if m.err != nil {
			failed++
		}
	}
	waited, waitedFor := waits()
	fmt.Printf("  run: %s, %d workers, %d books failed, %d waits the service asked for (%s), %s\n",
		elapsed.Round(100*time.Millisecond), n, failed, waited, waitedFor, cmp.Or(baseURL, "mirror mode"))
	if out := os.Getenv("LF_OUT"); out != "" {
		b, _ := json.MarshalIndent(results, "", " ")
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// scanBook is b as a scan indexes it.
func scanBook(b *lfBook) *catalog.Book {
	seconds := b.Min * 60
	book := &catalog.Book{LibraryID: 1, RelPath: b.F, IsFolder: true, Duration: seconds}
	if b.F == "" {
		book.RelPath, book.IsFolder = b.File, false
	}
	library.SetNamesForBench(book, metadata.ProbeTagsForBench(b.Tags, seconds))
	return book
}

// each runs fn on 0..count-1, n at a time.
func each(n, count int, fn func(i int)) {
	var wg sync.WaitGroup
	next := make(chan int)
	for range n {
		wg.Go(func() {
			for i := range next {
				fn(i)
			}
		})
	}
	for i := range count {
		next <- i
	}
	close(next)
	wg.Wait()
}

// benchRunContext is what a run's requests go under, and its count of the waits the
// service asked for.
func benchRunContext(ctx context.Context) (context.Context, func() (int, time.Duration)) {
	p := meta.NewPacer(maxRequestsPerSecond, slog.Default())
	return meta.WithPacer(ctx, p), p.Waits
}

// lfMatched is one book as the run left it, not yet judged.
type lfMatched struct {
	book  *catalog.Book
	item  *catalog.MatchRunItem
	cands []meta.MatchCandidate
	err   error
}

// matchOne is the run's part of one book: matchBook against svc.
func matchOne(ctx context.Context, t *testing.T, svc *meta.Service, b *lfBook, region string) lfMatched {
	book := scanBook(b)
	c := &capture{svc: svc}
	r := &Runner{matcher: c}
	item, err := r.matchBook(ctx, StartOptions{Mode: catalog.MatchModeMatch, Region: region}, book)
	if err != nil {
		t.Logf("%s: %v", book.RelPath, err)
	}
	return lfMatched{book: book, item: item, cands: c.cands, err: err}
}

// judge scores a book's match against its right answer, from the local copy.
func judge(local *meta.Service, b *lfBook, m *lfMatched, region string) lfResult {
	ctx := context.Background()
	book, item := m.book, m.item
	res := lfResult{Path: book.RelPath, Title: book.Title, RefTitle: b.Ref.Title}
	if m.err != nil || item == nil {
		res.Outcome = catalog.OutcomeError
		return res
	}
	res.Outcome, res.Detail, res.Score, res.RunnerUp = item.Outcome, item.Detail, item.Score, item.RunnerUp

	// The right work: the one the reference ASIN names in this copy.
	var refRec string
	if b.Ref.ASIN != "" {
		if hits, err := local.Candidates(ctx, meta.MatchQuery{ASIN: b.Ref.ASIN, Limit: 1, NoSeries: true}); err == nil && len(hits) > 0 {
			res.RefWork, refRec = hits[0].WorkID, hits[0].RecordingID
		}
	}
	if len(m.cands) == 0 {
		return res
	}
	top := &m.cands[0]
	res.Work = top.WorkID
	if top.Title == "" {
		// A weak best with no details (a metaserve that inlines nothing): the run
		// reads none of them, but judging it does.
		q := QueryFor(book, region)
		q.Limit = bulkCandidates
		if full, err := local.Candidates(ctx, q); err == nil {
			if i := slices.IndexFunc(full, func(c meta.MatchCandidate) bool { return c.WorkID == top.WorkID }); i >= 0 {
				top = &full[i]
			}
		}
	}
	switch {
	case res.RefWork != "":
		res.Right = top.WorkID == res.RefWork
	case strings.TrimSpace(b.Ref.Title) == "":
		res.Unjudged = true
	default:
		res.Right = sameBook(top, b.Ref.Title, b.Ref.Author)
	}
	if res.Right {
		// A weak match proposes nothing: then the recording the run would pick.
		pick := cmp.Or(item.Proposal.RecordingID, top.DefaultRecordingID)
		res.RecordingRight = (refRec != "" && pick == refRec) || (refRec == "" && narratedBy(top, pick, b.Ref.Narrator))
	}
	if item.Outcome == catalog.OutcomeAuto {
		st := matchStateOf(book)
		if w := Plan(ScopeFill, st, item.Proposal); w.Set[catalog.FieldTitle] != "" && st.Fields[catalog.FieldTitle].Value != "" {
			res.Tidied = w.Set[catalog.FieldTitle]
		}
	}
	return res
}

// narratedBy reports whether recording id of c is read by the reference
// narrator credit (any name of it, folded).
func narratedBy(c *meta.MatchCandidate, id, credit string) bool {
	want := match.Fold(credit)
	for _, r := range c.Recordings {
		if r.ID != id {
			continue
		}
		for _, n := range r.Narrators {
			if f := match.Fold(n.Name); f != "" && strings.Contains(want, f) {
				return true
			}
		}
	}
	return false
}

// sameBook is the fuzzy check for a reference without an ASIN this copy
// knows: the work's title (alone, with its subtitle, or the part before a
// colon on either side) reads like the reference title ("&" as "and", a
// leading article dropped), and one of its authors' surnames is in the
// reference author.
func sameBook(c *meta.MatchCandidate, title, author string) bool {
	ref := []string{normTitle(title), normTitle(beforeColon(title))}
	work := []string{normTitle(c.Title), normTitle(c.Title + " " + c.Subtitle), normTitle(beforeColon(c.Title))}
	same := false
	for _, r := range ref {
		same = same || (r != "" && slices.Contains(work, r))
	}
	if !same {
		return false
	}
	a := match.Fold(author)
	for _, p := range c.Authors {
		if f := strings.Fields(p.Name); len(f) > 0 && strings.Contains(a, match.Fold(f[len(f)-1])) {
			return true
		}
	}
	return false
}

func beforeColon(s string) string { before, _, _ := strings.Cut(s, ":"); return before }

func normTitle(s string) string {
	ws := titleWords(strings.ReplaceAll(s, "&", " and "))
	if len(ws) > 1 && (ws[0] == "the" || ws[0] == "a" || ws[0] == "an") {
		ws = ws[1:]
	}
	return strings.Join(ws, " ")
}

// matchStateOf is book as Plan sees an unedited, scanned book.
func matchStateOf(b *catalog.Book) *catalog.MatchState {
	st := &catalog.MatchState{Title: b.Title, Author: b.Author, Duration: b.Duration, Fields: map[string]catalog.FieldValue{}}
	set := func(field, v string) {
		st.Fields[field] = catalog.FieldValue{Value: v, Scanned: v, Source: catalog.SourceTag}
	}
	set(catalog.FieldTitle, b.Title)
	set(catalog.FieldAuthor, b.Author)
	set(catalog.FieldNarrator, b.Narrator)
	set(catalog.FieldSeries, b.Series)
	if b.SeriesIndex > 0 {
		set(catalog.FieldSeriesIndex, strconv.FormatFloat(b.SeriesIndex, 'f', -1, 64))
	}
	return st
}

func printTable(rs []lfResult, region string) {
	count := map[string]int{}
	for _, r := range rs {
		key := r.Outcome
		switch r.Outcome {
		case catalog.OutcomeAuto, catalog.OutcomeReview:
			key += map[bool]string{true: "-right", false: "-wrong"}[r.Right]
			if r.Unjudged {
				key = r.Outcome + "-unjudged"
			}
			if r.Outcome == catalog.OutcomeAuto && r.Right && r.RecordingRight {
				count["auto-right, recording right"]++
			}
		case catalog.OutcomeNone:
			if r.Detail == DetailWeakMatch {
				count["none: weak_match"]++
				if r.Right {
					count["none: weak_match, top was right"]++
				}
			}
		}
		count[key]++
		if r.Tidied != "" {
			count["auto: title tidied"]++
			if match.Fold(r.Tidied) == match.Fold(r.RefTitle) {
				count["auto: title tidied to the reference"]++
			}
		}
	}
	fmt.Printf("\nLibraForge harness: %d books, region %q\n", len(rs), region)
	keys := []string{"auto-right", "auto-right, recording right", "auto-wrong", "auto-unjudged", "review-right", "review-wrong", "review-unjudged",
		"none", "none: weak_match", "none: weak_match, top was right", "error", "auto: title tidied", "auto: title tidied to the reference"}
	for _, k := range keys {
		fmt.Printf("  %-34s %5d\n", k, count[k])
	}
}

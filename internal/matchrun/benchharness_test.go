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
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/pool"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// lfBook is one benchmark book as data.json records it: its folder (f, under
// the library root), its file, its runtime in minutes, its ffprobe tag map and
// the right answer (ref; asin "" when it has no confirmed Audible listing).
// Cls, Match and Trace are LibraForge's own pick, hand-checked: its class
// (rightBook), the listing it chose and its trace, whose last "X MATCH:" header
// names the store the listing is from. O holds the hand-audited verdicts of
// other tools, O["audiosilo"] = [class, asin] for AudioSilo v2.5.1.
type lfBook struct {
	F    string            `json:"f"`
	File string            `json:"file"`
	Min  float64           `json:"min"`
	Tags map[string]string `json:"tags"`
	Ref  struct {
		ASIN, Title, Author, Narrator string
	} `json:"ref"`
	Cls   string `json:"cls"`
	Match struct {
		ASIN, ISBN string
	} `json:"match"`
	Trace []string            `json:"trace"`
	O     map[string][]string `json:"o"`
}

// rightBook is a hand-checked class that names the right book: fully
// correct, a field off, another recording of it, or held with the right pick.
// LibraForge's classes and the audited AudioSilo ones share these names.
var rightBook = []string{"correct", "partial", "recording", "correct-held"}

// What a verdict was judged against (lfResult.JudgedBy).
const (
	byRefASIN = "ref_asin" // the work the reference ASIN names
	byLFPick  = "lf_pick"  // the work LibraForge's hand-checked pick names (no reference ASIN)
	byHand    = "hand"     // the same pick as v2.5.1's, whose hand-audited verdict it takes
	byTitle   = "title"    // the reference title and author alone: the last resort
)

// lfPick is the listing LibraForge's hand-checked pick names, when it named
// the right book: its ASIN when the pick is an Audible listing (another
// store's id is no ASIN), and its ISBN from any store. recording says the
// pick is another recording of the book, not the book's own.
func lfPick(b *lfBook) (asin, isbn string, recording bool) {
	if !slices.Contains(rightBook, b.Cls) {
		return "", "", false
	}
	if lfSource(b.Trace) == "AUDIBLE" {
		asin = strings.TrimSpace(b.Match.ASIN)
	}
	return asin, strings.TrimSpace(b.Match.ISBN), b.Cls == "recording"
}

// lfSource is the store of the last "X MATCH:" header in a LibraForge trace
// ("AUDIBLE", "AUDIOBOOK GUILD", "GOODREADS", ...), "" when it has none.
func lfSource(trace []string) string {
	src := ""
	for _, l := range trace {
		l = strings.TrimSpace(l)
		head, ok := strings.CutSuffix(l, " MATCH:")
		if ok && head != "" && !strings.ContainsFunc(head, func(r rune) bool {
			return (r < 'A' || r > 'Z') && r != ' '
		}) {
			src = head
		}
	}
	return src
}

// handVerdict is the hand-audited AudioSilo v2.5.1 verdict on b: the listing
// it picked and its class (right when in rightBook); ok is false when v2.5.1
// picked nothing (or wasn't run).
func handVerdict(b *lfBook) (asin, class string, ok bool) {
	v := b.O["audiosilo"]
	if len(v) < 2 || v[1] == "" || v[0] == "none" || v[0] == "not run" {
		return "", "", false
	}
	return v[1], v[0], true
}

// answers are what a book's top work is judged against.
type answers struct {
	refWork    string // the work the reference ASIN names ("" when this copy knows none)
	refHasASIN bool
	lfWork     string // the work LibraForge's pick names
	handWork   string // the work v2.5.1's hand-audited pick names
	handRight  bool   // and whether the audit found it the right book
	hasTitle   bool   // the reference has a title (else the title can't judge)
	titleSame  bool   // the top work has the reference title and author
}

// verdictOf judges the top work: against the reference ASIN's work; for a
// reference with no ASIN, else against the work LibraForge's pick names, else
// by the audited v2.5.1 verdict when v2.5.1 picked the same work; else by the
// reference title and author (titleSame; when the reference has no title, the
// book is unjudged).
func verdictOf(top string, a answers) (right, unjudged bool, by string) {
	switch {
	case a.refWork != "":
		return top == a.refWork, false, byRefASIN
	case !a.refHasASIN && a.lfWork != "":
		return top == a.lfWork, false, byLFPick
	case !a.refHasASIN && a.handWork != "" && top == a.handWork:
		return a.handRight, false, byHand
	case !a.hasTitle:
		return false, true, ""
	default:
		return a.titleSame, false, byTitle
	}
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
	// Right: the top candidate is the right work (verdictOf; JudgedBy says by
	// what). RecordingRight: and the recording the proposal picked is the
	// reference's (its ASIN's, else LibraForge's pick's, else its narrator).
	Right bool `json:"right"`
	// Unjudged: no reference ASIN work, LibraForge pick or v2.5.1 pick of the
	// same work to judge by, and no reference title.
	Unjudged bool   `json:"unjudged,omitempty"`
	JudgedBy string `json:"judged_by,omitempty"`
	// RefHasASIN: the reference has an ASIN (known to this copy or not).
	// LFWork: the work LibraForge's pick names, for a reference without one.
	RefHasASIN bool   `json:"ref_has_asin"`
	LFWork     string `json:"lf_work,omitempty"`
	// TitleRight and TitleUnjudged are the verdict by the reference ASIN, else
	// by title and author alone (the judging before LibraForge's pick was
	// read), kept to compare with older results files; TitleRecordingRight is
	// RecordingRight by that judging (the reference ASIN's recording, else the
	// narrator).
	TitleRight          bool `json:"title_right"`
	TitleUnjudged       bool `json:"title_unjudged,omitempty"`
	TitleRecordingRight bool `json:"title_recording_right"`
	// Hand is the hand-audited v2.5.1 class (handVerdict), HandWork the work
	// its listing names: where HandWork is Work, both judged the same pick.
	Hand           string `json:"hand,omitempty"`
	HandWork       string `json:"hand_work,omitempty"`
	HandRight      bool   `json:"hand_right,omitempty"`
	RecordingRight bool   `json:"recording_right"`
	Tidied         string `json:"tidied,omitempty"`
	// Title is the book's title as scanned, RefTitle the reference's.
	Title    string `json:"title"`
	RefTitle string `json:"ref_title"`
	// Diag is what the run saw (LF_DIAG=1 only).
	Diag *diag `json:"diag,omitempty"`
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
//	  [LF_OUT=<results.json>] [LF_DIAG=1] [LF_WORKERS=8] [LF_LIMIT=n] [LF_BASE_URL=<metaserve>] \
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
//
// Each top work is judged (verdictOf) against the reference ASIN's work; for a
// reference with no ASIN, against the work LibraForge's hand-checked pick names,
// else by the audited v2.5.1 verdict when v2.5.1 picked the same work; title and
// author are the last resort. The tables split by reference ASIN or none and by
// what each verdict was judged against, print the older judging (reference ASIN,
// else title and author) beside it, and how often the judging agrees with the
// v2.5.1 audit on the picks the two share.
//
// LF_DIAG=1 adds each book's diagnostics to LF_OUT (lfResult.Diag, diagOf):
// what Classify saw, the query sent and the local copy's top 10 candidates in
// full, so a change can say which books moved and why.
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

	svc, ctx := local, context.Background()
	pacer := meta.NewPacer(maxRequestsPerSecond, slog.Default())
	if baseURL != "" {
		svc = meta.NewService(baseURL, time.Now)
		ctx = meta.WithPacer(ctx, pacer)
	}

	// The library, as a scan indexes it, and its language as a run derives it: the
	// books counted by what each says (catalog.LanguageCounts' rows, one a book).
	scanned := make([]*catalog.Book, len(books))
	counts := make([]catalog.LanguageCount, len(books))
	for i := range books {
		scanned[i] = scanBook(&books[i])
		counts[i] = catalog.LanguageCount{Language: scanned[i].Language, TagLanguage: scanned[i].TagLanguage, Books: 1}
	}
	language := LibraryLanguage(counts)
	fmt.Printf("library language: %q\n", language)

	// The run, on its own clock.
	matched := make([]lfMatched, len(books))
	start := time.Now()
	each(n, len(books), func(i int) {
		if i > 0 && i%500 == 0 {
			t.Logf("%d books, %s", i, time.Since(start).Round(time.Second))
		}
		matched[i] = matchOne(ctx, t, svc, scanned[i], region, language)
	})
	elapsed := time.Since(start)

	// A book that failed is asked again, off the clock, so the table compares matches
	// rather than how busy the machine was (the local copy's own timeout, a metaserve
	// past its budget). The line below says how many it was.
	failed := []int{}
	for i, m := range matched {
		if m.err != nil {
			failed = append(failed, i)
		}
	}
	stillFailed := 0
	for _, i := range failed {
		matched[i] = matchOne(ctx, t, svc, scanned[i], region, language)
		if matched[i].err != nil {
			stillFailed++
		}
	}

	// Judged afterwards, from the local copy.
	results := make([]lfResult, len(books))
	diagOn := os.Getenv("LF_DIAG") == "1"
	each(8, len(books), func(i int) {
		results[i] = judge(local, &books[i], &matched[i], region, language)
		if diagOn {
			results[i].Diag = diagOf(local, &books[i], &matched[i], &results[i], region, language)
		}
	})

	printTable(results, region)
	waited, waitedFor := pacer.Waits()
	fmt.Printf("  run: %s, %d workers, %d books failed (%d still when asked again), %d waits the service asked for (%s), %s\n",
		elapsed.Round(100*time.Millisecond), n, len(failed), stillFailed, waited, waitedFor, cmp.Or(baseURL, "mirror mode"))
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
	md := metadata.ProbeTagsForBench(b.Tags, seconds)
	library.SetNamesForBench(book, md)
	book.Genres = md.Genres // as the scan sets them beside the names
	return book
}

// each runs fn on 0..count-1, n at a time.
func each(n, count int, fn func(i int)) {
	indexes := make([]int, count)
	for i := range indexes {
		indexes[i] = i
	}
	pool.Each(context.Background(), n, indexes, func() bool { return true }, func(i *int) { fn(*i) })
}

// lfMatched is one book as the run left it, not yet judged.
type lfMatched struct {
	book  *catalog.Book
	item  *catalog.MatchRunItem
	cands []meta.MatchCandidate
	err   error
}

// matchOne is the run's part of one scanned book of a library in language: matchBook
// against svc.
func matchOne(ctx context.Context, t *testing.T, svc *meta.Service, book *catalog.Book, region, language string) lfMatched {
	c := &capture{svc: svc}
	r := &Runner{matcher: c}
	item, err := r.matchBook(ctx, StartOptions{Mode: catalog.MatchModeMatch, Region: region}, book, language)
	if err != nil {
		t.Logf("%s: %v", book.RelPath, err)
	}
	return lfMatched{book: book, item: item, cands: c.cands, err: err}
}

// judge scores a book's match against its right answer, from the local copy.
func judge(local *meta.Service, b *lfBook, m *lfMatched, region, language string) lfResult {
	ctx := context.Background()
	book, item := m.book, m.item
	res := lfResult{Path: book.RelPath, Title: book.Title, RefTitle: b.Ref.Title}
	res.RefHasASIN = b.Ref.ASIN != ""
	if m.err != nil || item == nil {
		res.Outcome = catalog.OutcomeError
		return res
	}
	res.Outcome, res.Detail, res.Score, res.RunnerUp = item.Outcome, item.Detail, item.Score, item.RunnerUp

	// The right work: the one the reference ASIN names in this copy, else (no
	// reference ASIN) the one LibraForge's hand-checked pick names.
	var refRec string
	if res.RefHasASIN {
		res.RefWork, refRec = lookup(ctx, local, meta.MatchQuery{ASIN: b.Ref.ASIN})
	} else if asin, isbn, recording := lfPick(b); asin != "" || isbn != "" {
		var rec string
		if asin != "" {
			res.LFWork, rec = lookup(ctx, local, meta.MatchQuery{ASIN: asin})
		}
		if res.LFWork == "" && isbn != "" {
			res.LFWork, rec = lookup(ctx, local, meta.MatchQuery{ISBN: isbn})
		}
		if !recording {
			refRec = rec
		}
	}
	if asin, class, ok := handVerdict(b); ok {
		res.Hand, res.HandRight = class, slices.Contains(rightBook, class)
		if asin == b.Ref.ASIN {
			res.HandWork = res.RefWork // already looked up
		} else {
			res.HandWork, _ = lookup(ctx, local, meta.MatchQuery{ASIN: asin})
		}
	}
	if len(m.cands) == 0 {
		return res
	}
	top := &m.cands[0]
	res.Work = top.WorkID
	if top.Title == "" {
		// A weak best has no details: the run reads none of them, but judging it does.
		q := QueryFor(book, region, language)
		q.Limit = bulkCandidates
		if full, err := local.Candidates(ctx, q); err == nil {
			if i := slices.IndexFunc(full, func(c meta.MatchCandidate) bool { return c.WorkID == top.WorkID }); i >= 0 {
				top = &full[i]
			}
		}
	}
	hasTitle := strings.TrimSpace(b.Ref.Title) != ""
	titleSame := hasTitle && sameBook(top, b.Ref.Title, b.Ref.Author)
	res.Right, res.Unjudged, res.JudgedBy = verdictOf(top.WorkID, answers{
		refWork: res.RefWork, refHasASIN: res.RefHasASIN, lfWork: res.LFWork,
		handWork: res.HandWork, handRight: res.HandRight, hasTitle: hasTitle, titleSame: titleSame,
	})
	// The older judging: no LibraForge or v2.5.1 pick to read.
	res.TitleRight, res.TitleUnjudged, _ = verdictOf(top.WorkID, answers{refWork: res.RefWork, hasTitle: hasTitle, titleSame: titleSame})
	// A weak match proposes nothing: then the recording the run would pick.
	pick := cmp.Or(item.Proposal.RecordingID, top.DefaultRecordingID)
	recordingIs := func(rec string) bool {
		return (rec != "" && pick == rec) || (rec == "" && narratedBy(top, pick, b.Ref.Narrator))
	}
	res.RecordingRight = res.Right && recordingIs(refRec)
	// The older judging read no LibraForge pick's recording.
	oldRec := ""
	if res.RefHasASIN {
		oldRec = refRec
	}
	res.TitleRecordingRight = res.TitleRight && recordingIs(oldRec)
	if item.Outcome == catalog.OutcomeAuto {
		st := matchStateOf(book)
		if w := Plan(ScopeFill, st, item.Proposal); w.Set[catalog.FieldTitle] != "" && st.Fields[catalog.FieldTitle].Value != "" {
			res.Tidied = w.Set[catalog.FieldTitle]
		}
	}
	return res
}

// lookup is the work and recording an identifier query names in the local
// copy ("" when it knows none).
func lookup(ctx context.Context, local *meta.Service, q meta.MatchQuery) (work, recording string) {
	q.Limit, q.NoSeries = 1, true
	var hits []meta.MatchCandidate
	var err error
	for range 3 { // a busy machine's timeout isn't an unknown identifier
		if hits, err = local.Candidates(ctx, q); err == nil {
			break
		}
	}
	if err != nil || len(hits) == 0 {
		return "", ""
	}
	return hits[0].WorkID, hits[0].RecordingID
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

// verdict is a judging of one result: whether its top work is right, whether
// it could be judged at all, and whether the recording picked is right too.
type verdict func(r *lfResult) (right, unjudged, recordingRight bool)

func newJudging(r *lfResult) (bool, bool, bool) { return r.Right, r.Unjudged, r.RecordingRight }
func titleJudging(r *lfResult) (bool, bool, bool) {
	return r.TitleRight, r.TitleUnjudged, r.TitleRecordingRight
}

// tableKeys are the rows of a table, in order.
var tableKeys = []string{"auto-right", "auto-right, recording right", "auto-wrong", "auto-unjudged", "review-right", "review-wrong", "review-unjudged",
	"none", "none: weak_match", "none: weak_match, top was right", "error", "auto: title tidied", "auto: title tidied to the reference"}

// tally counts the results keep admits per row of tableKeys, judged by v.
func tally(rs []lfResult, keep func(*lfResult) bool, v verdict) map[string]int {
	count := map[string]int{}
	for i := range rs {
		r := &rs[i]
		if !keep(r) {
			continue
		}
		right, unjudged, recordingRight := v(r)
		key := r.Outcome
		switch r.Outcome {
		case catalog.OutcomeAuto, catalog.OutcomeReview:
			key += map[bool]string{true: "-right", false: "-wrong"}[right]
			if unjudged {
				key = r.Outcome + "-unjudged"
			}
			if r.Outcome == catalog.OutcomeAuto && right && recordingRight {
				count["auto-right, recording right"]++
			}
		case catalog.OutcomeNone:
			if r.Detail == DetailWeakMatch {
				count["none: weak_match"]++
				if right {
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
	return count
}

// column is one column of a printed table.
type column struct {
	name string
	keep func(*lfResult) bool
	v    verdict
}

func printColumns(title string, rs []lfResult, cols []column) {
	fmt.Printf("\n%s\n  %-34s", title, "")
	counts := make([]map[string]int, len(cols))
	for i, c := range cols {
		fmt.Printf(" %12s", c.name)
		counts[i] = tally(rs, c.keep, c.v)
	}
	fmt.Println()
	for _, k := range tableKeys {
		fmt.Printf("  %-34s", k)
		for _, c := range counts {
			fmt.Printf(" %12d", c[k])
		}
		fmt.Println()
	}
}

// agreement is how often v agrees with the hand-audited v2.5.1 verdict on the
// results keep admits where both judged the same pick (the top work is the
// work v2.5.1's listing names) and v could judge it without taking that
// verdict (byHand).
func agreement(rs []lfResult, keep func(*lfResult) bool, v verdict) (agree, shared int) {
	for i := range rs {
		r := &rs[i]
		if !keep(r) || r.Work == "" || r.HandWork != r.Work || r.JudgedBy == byHand {
			continue
		}
		right, unjudged, _ := v(r)
		if unjudged {
			continue
		}
		shared++
		if right == r.HandRight {
			agree++
		}
	}
	return agree, shared
}

func printTable(rs []lfResult, region string) {
	all := func(*lfResult) bool { return true }
	withASIN := func(r *lfResult) bool { return r.RefHasASIN }
	noASIN := func(r *lfResult) bool { return !r.RefHasASIN }
	judgedBy := func(by string) func(*lfResult) bool {
		return func(r *lfResult) bool { return r.JudgedBy == by }
	}
	byRefASINOrNot := func(v verdict) []column {
		return []column{{"all", all, v}, {"ref ASIN", withASIN, v}, {"no ref ASIN", noASIN, v}}
	}
	fmt.Printf("\nLibraForge harness: %d books, region %q\n", len(rs), region)
	printColumns("Judged by the reference ASIN, else LibraForge's pick, else v2.5.1's audited pick, else title and author:", rs, byRefASINOrNot(newJudging))
	// "not judged" holds the rest (no candidates, an error, or unjudged), so the columns sum to all.
	printColumns("The same, by what each verdict was judged against:", rs, []column{
		{"ref ASIN", judgedBy(byRefASIN), newJudging}, {"LF pick", judgedBy(byLFPick), newJudging},
		{"v2.5.1 hand", judgedBy(byHand), newJudging}, {"title only", judgedBy(byTitle), newJudging},
		{"not judged", judgedBy(""), newJudging},
	})
	printColumns("Judged by the reference ASIN, else title and author (the older judging):", rs, byRefASINOrNot(titleJudging))
	fmt.Println("\nAgreement with the hand-audited v2.5.1 verdict where both judged the same pick (not by it):")
	for _, c := range append(byRefASINOrNot(newJudging), column{"no ref ASIN, older judging", noASIN, titleJudging}) {
		agree, shared := agreement(rs, c.keep, c.v)
		pct := 0.0
		if shared > 0 {
			pct = 100 * float64(agree) / float64(shared)
		}
		fmt.Printf("  %-34s %5d of %5d (%.1f%%)\n", c.name, agree, shared, pct)
	}
}

// diag is what the run saw for one book (LF_DIAG=1): whether the proposal
// carries an identifier and its length fits (two of Classify's conditions),
// the book's and the proposal's runtimes, the query sent, the run's own
// candidates (run_cands: the best in full, the rest stubs) and the local copy's
// top 10 in full (cands), with the right work's rank among them.
type diag struct {
	Identified bool    `json:"identified"`
	LengthFits bool    `json:"length_fits"`
	BookMin    float64 `json:"book_min"`
	PropMin    int     `json:"prop_min"`
	PropASIN   string  `json:"prop_asin,omitempty"`
	PropISBN   string  `json:"prop_isbn,omitempty"`
	// The query: the book's title, series, position (range), author, language,
	// identifier hints and narrators.
	QTitle   string            `json:"q_title,omitempty"`
	QSeries  string            `json:"q_series,omitempty"`
	QPos     float64           `json:"q_pos,omitempty"`
	QPosEnd  float64           `json:"q_pos_end,omitempty"`
	QAuthor  string            `json:"q_author,omitempty"`
	QLang    string            `json:"q_lang,omitempty"`
	QASIN    string            `json:"q_asin,omitempty"`
	QISBN    string            `json:"q_isbn,omitempty"`
	QNarr    []string          `json:"q_narr,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`
	Cands    []diagCand        `json:"cands"`
	RunCands []diagCand        `json:"run_cands"`
	// RightRank is the right work's 1-based rank in cands (0 = absent or no
	// right work known: the reference ASIN's, else, for a reference without
	// one, LibraForge's pick's, else v2.5.1's pick when the audit found it
	// right), RightScore its score.
	RightRank  int `json:"right_rank"`
	RightScore int `json:"right_score"`
	// Err is why cands could not be asked of the local copy ("" when they were).
	Err string `json:"err,omitempty"`
}

// diagCand is one candidate: its work, score, title, language, authors,
// series and positions, metaserve's reasons, whether it was an identifier hit,
// and its recordings (how many, how many with an ASIN or ISBN, their runtimes).
type diagCand struct {
	Work     string             `json:"work"`
	Score    int                `json:"score"`
	Title    string             `json:"title,omitempty"`
	Sub      string             `json:"sub,omitempty"`
	Lang     string             `json:"lang,omitempty"`
	Authors  string             `json:"authors,omitempty"`
	Series   []meta.MatchSeries `json:"series,omitempty"`
	Reasons  *meta.MatchReasons `json:"reasons,omitempty"`
	Hit      bool               `json:"hit,omitempty"`
	Recs     int                `json:"recs"`
	RecsID   int                `json:"recs_id"`
	Runtimes []int              `json:"runtimes,omitempty"`
}

func diagCandOf(c *meta.MatchCandidate) diagCand {
	d := diagCand{
		Work: c.WorkID, Score: c.Score, Title: c.Title, Sub: c.Subtitle, Lang: c.Language,
		Authors: joinNames(c.Authors), Series: c.Series, Reasons: c.Reasons, Hit: c.IdentifierHit(),
		Recs: len(c.Recordings),
	}
	for _, r := range c.Recordings {
		if len(r.ASINs) > 0 || len(r.ISBNs) > 0 {
			d.RecsID++
		}
		d.Runtimes = append(d.Runtimes, r.RuntimeMin)
	}
	return d
}

// diagOf is m's diagnostics, the top 10 asked of the local copy again.
func diagOf(local *meta.Service, b *lfBook, m *lfMatched, res *lfResult, region, language string) *diag {
	q := QueryFor(m.book, region, language)
	d := &diag{
		BookMin: m.book.Duration / 60, Tags: b.Tags,
		QTitle: q.Title, QSeries: q.Series, QPos: q.SeriesIndex, QPosEnd: q.SeriesIndexEnd, QAuthor: q.Author,
		QLang: q.Language, QASIN: q.BookASIN, QISBN: q.BookISBN, QNarr: q.Narrators,
		Cands: []diagCand{}, RunCands: []diagCand{},
	}
	if m.item != nil {
		p := m.item.Proposal
		d.PropASIN, d.PropISBN, d.PropMin = p.Values[catalog.FieldASIN], p.Values[catalog.FieldISBN], p.RuntimeMin
		d.Identified = d.PropASIN != "" || d.PropISBN != ""
		narrated := len(m.cands) > 0 && narratorReads(&m.cands[0], p, q.Narrators)
		d.LengthFits = lengthFits(p, m.book.Duration, narrated)
	}
	for i := range m.cands {
		d.RunCands = append(d.RunCands, diagCandOf(&m.cands[i]))
	}
	q.Limit = 10
	var full []meta.MatchCandidate
	var err error
	for range 3 { // a busy machine's timeout, as in lookup
		if full, err = local.Candidates(context.Background(), q); err == nil {
			break
		}
	}
	if err != nil {
		d.Err = err.Error()
		return d
	}
	// The right work as verdictOf reads it: the reference ASIN's, else (no
	// reference ASIN) LibraForge's pick's, else v2.5.1's when found right.
	right := res.RefWork
	if right == "" && !res.RefHasASIN {
		right = res.LFWork
		if right == "" && res.HandRight {
			right = res.HandWork
		}
	}
	for i := range full {
		d.Cands = append(d.Cands, diagCandOf(&full[i]))
		if right != "" && full[i].WorkID == right && d.RightRank == 0 {
			d.RightRank, d.RightScore = i+1, full[i].Score
		}
	}
	return d
}

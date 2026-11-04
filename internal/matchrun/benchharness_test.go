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
	"unicode"

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
		ASIN, Title, Author, Narrator, Series, Sequence string
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
//	LF_DATA=<data.json> LF_ARTIFACT=<meta.sqlite> [LF_REGION=us] [LF_TAG_IDS=1] \
//	  [LF_OUT=<results.json>] [LF_WORKERS=8] [LF_LIMIT=n] \
//	  go test -tags benchharness -run TestLibraForge -timeout 0 -v ./internal/matchrun
//
// Each book is built as a scan builds it (the folder is a folder book; its
// names from the path layout and the probe's tags: library.setNames,
// metadata.applyProbe), runtime = min*60, then matched by the run's own
// matchBook (QueryFor, Candidates, Propose/DefaultRecording, Classify). The
// scanner reads no ASIN, ISBN or language tag, so by default a book has none;
// LF_TAG_IDS=1 gives it the tags' (asin/audible_asin, isbn, language), as a
// scanner that read them would.
func TestLibraForge(t *testing.T) {
	dataPath, artifact := os.Getenv("LF_DATA"), os.Getenv("LF_ARTIFACT")
	if dataPath == "" || artifact == "" {
		t.Skip("set LF_DATA and LF_ARTIFACT")
	}
	region := cmp.Or(os.Getenv("LF_REGION"), "us")
	tagIDs := os.Getenv("LF_TAG_IDS") == "1"
	workers, _ := strconv.Atoi(os.Getenv("LF_WORKERS"))
	workers = cmp.Or(workers, 8)

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
	svc := meta.NewService("http://127.0.0.1:9", time.Now) // never reached: the copy answers
	svc.SetMirror(localCopy{h}, slog.Default())

	results := make([]lfResult, len(books))
	var wg sync.WaitGroup
	next := make(chan int)
	for range workers {
		wg.Go(func() {
			for i := range next {
				results[i] = runBook(t, svc, &books[i], region, tagIDs)
			}
		})
	}
	start := time.Now()
	for i := range books {
		next <- i
		if i > 0 && i%500 == 0 {
			t.Logf("%d books, %s", i, time.Since(start).Round(time.Second))
		}
	}
	close(next)
	wg.Wait()

	printTable(results, region, tagIDs)
	if out := os.Getenv("LF_OUT"); out != "" {
		b, _ := json.MarshalIndent(results, "", " ")
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// lfLibrary is the library the books sit in: only its id is read.
var lfLibrary int64 = 1

// scanBook is b as a scan indexes it.
func scanBook(b *lfBook, tagIDs bool) *catalog.Book {
	seconds := b.Min * 60
	book := &catalog.Book{LibraryID: lfLibrary, RelPath: b.F, IsFolder: true, Duration: seconds}
	if b.F == "" {
		book.RelPath, book.IsFolder = b.File, false
	}
	library.SetNamesForBench(book, metadata.ProbeTagsForBench(b.Tags, seconds))
	if tagIDs {
		book.ASIN = match.NormalizeASIN(cmp.Or(b.Tags["asin"], b.Tags["audible_asin"]))
		book.ISBN = match.NormalizeISBN(b.Tags["isbn"])
		if l := strings.ToLower(strings.TrimSpace(b.Tags["language"])); l != "und" {
			book.Language = l
		}
	}
	return book
}

func runBook(t *testing.T, svc *meta.Service, b *lfBook, region string, tagIDs bool) lfResult {
	ctx := context.Background()
	book := scanBook(b, tagIDs)
	c := &capture{svc: svc}
	r := &Runner{matcher: c}
	item, err := r.matchBook(ctx, StartOptions{Mode: catalog.MatchModeMatch, Region: region}, book)
	res := lfResult{Path: book.RelPath, Title: book.Title, RefTitle: b.Ref.Title}
	if err != nil || item == nil {
		res.Outcome = catalog.OutcomeError
		t.Logf("%s: %v", book.RelPath, err)
		return res
	}
	res.Outcome, res.Detail, res.Score, res.RunnerUp = item.Outcome, item.Detail, item.Score, item.RunnerUp

	// The right work: the one the reference ASIN names in this copy.
	var refRec string
	if b.Ref.ASIN != "" {
		if hits, err := svc.Candidates(ctx, meta.MatchQuery{ASIN: b.Ref.ASIN, Limit: 1, NoSeries: true}); err == nil && len(hits) > 0 {
			res.RefWork, refRec = hits[0].WorkID, hits[0].RecordingID
		}
	}
	if len(c.cands) == 0 {
		return res
	}
	top := &c.cands[0]
	res.Work = top.WorkID
	switch {
	case res.RefWork != "":
		res.Right = top.WorkID == res.RefWork
	case strings.TrimSpace(b.Ref.Title) == "":
		res.Unjudged = true
	default:
		res.Right = sameBook(top, b.Ref.Title, b.Ref.Author)
	}
	if res.Right {
		pick := item.Proposal.RecordingID
		if pick == "" { // a weak match proposes nothing: the recording the run would pick
			if rec := pickedRecording(top); rec != nil {
				pick = rec.ID
			}
		}
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

// pickedRecording is the recording a candidate names as the book's.
func pickedRecording(c *meta.MatchCandidate) *meta.MatchRecording {
	for i := range c.Recordings {
		if c.Recordings[i].ID == c.DefaultRecordingID {
			return &c.Recordings[i]
		}
	}
	return nil
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
	ws := strings.FieldsFunc(strings.ToLower(strings.NewReplacer("&", " and ", "'", "", "\u2019", "").Replace(s)),
		func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
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

func printTable(rs []lfResult, region string, tagIDs bool) {
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
			if r.Detail == "weak_match" { // DetailWeakMatch, spelled out so the harness builds on main too
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
	fmt.Printf("\nLibraForge harness: %d books, region %q, tag ids %v\n", len(rs), region, tagIDs)
	keys := []string{"auto-right", "auto-right, recording right", "auto-wrong", "auto-unjudged", "review-right", "review-wrong", "review-unjudged",
		"none", "none: weak_match", "none: weak_match, top was right", "error", "auto: title tidied", "auto: title tidied to the reference"}
	for _, k := range keys {
		fmt.Printf("  %-34s %5d\n", k, count[k])
	}
}

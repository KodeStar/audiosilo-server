package meta

import (
	"context"
	"errors"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// Matching: the admin console's "Match with community" dialog. Given a book the
// server holds, find the community works it might be, each with its recordings, so
// an admin can attach one (its ASIN/ISBN, through the existing enrichment write) and
// accept fields from it (as community-sourced overrides). Admin-only and
// interactive, so results are not cached; the fan-out is bounded instead.

// maxMatchCandidates caps how many search hits are expanded into full works.
const maxMatchCandidates = 6

// MatchQuery describes the book being matched. Text is what to search for and
// ASIN/ISBN an identifier to look up directly; when all three are empty the book's
// own facts are used (its title and author as the text, its ASIN/ISBN). Title,
// Author and Duration also score the candidates.
type MatchQuery struct {
	Text     string
	ASIN     string
	ISBN     string
	Title    string
	Author   string
	Duration float64 // seconds; 0 when unknown
	BookASIN string
	BookISBN string
}

// MatchCandidate is one community work a book might be.
type MatchCandidate struct {
	WorkID         string           `json:"work_id"`
	Title          string           `json:"title"`
	Subtitle       string           `json:"subtitle,omitempty"`
	Authors        []MetaPersonRef  `json:"authors"`
	Language       string           `json:"language,omitempty"`
	FirstPublished string           `json:"first_published,omitempty"`
	Description    string           `json:"description,omitempty"`
	Series         []MatchSeries    `json:"series"`
	CoverURL       string           `json:"cover_url,omitempty"`
	WebURL         string           `json:"web_url"`
	Recordings     []MatchRecording `json:"recordings"`
	// RecordingID names the recording an ASIN/ISBN lookup resolved to.
	RecordingID string `json:"recording_id,omitempty"`
	// Score (0-100) is how well the work fits the book: title, author and runtime
	// agreement, or 100 for an identifier hit.
	Score int `json:"score"`
}

// MatchSeries is a series the work belongs to, with its position.
type MatchSeries struct {
	Name     string `json:"name"`
	Position string `json:"position"`
}

// MatchRecording is one recording (narration/edition) of a candidate work.
type MatchRecording struct {
	ID          string          `json:"id"`
	Narrators   []MetaPersonRef `json:"narrators"`
	Abridged    bool            `json:"abridged,omitempty"`
	RuntimeMin  int             `json:"runtime_min,omitempty"`
	ReleaseDate string          `json:"release_date,omitempty"`
	Publisher   string          `json:"publisher,omitempty"`
	ASINs       []string        `json:"asins"`
	ISBNs       []string        `json:"isbns"`
	CoverURL    string          `json:"cover_url,omitempty"`
}

// Candidates finds the community works a book might be, best first. It returns an
// empty list when nothing matches, and an error only when the community service
// could not be reached.
func (s *Service) Candidates(ctx context.Context, q MatchQuery) ([]MatchCandidate, error) {
	ctx, cancel := context.WithTimeout(ctx, s.composeBudget)
	defer cancel()

	text, asin, isbn := strings.TrimSpace(q.Text), strings.TrimSpace(q.ASIN), strings.TrimSpace(q.ISBN)
	if text == "" && asin == "" && isbn == "" {
		text = strings.TrimSpace(q.Title + " " + q.Author)
		asin, isbn = q.BookASIN, q.BookISBN
	}

	// The identifier lookup and the text search are independent; run them together.
	type hit struct {
		id, recordingID, cover string
		identified             bool
	}
	var (
		lookupHit          *hit
		search             *upstreamSearch
		lookupErr, findErr error
		first              sync.WaitGroup
	)
	if asin != "" || isbn != "" {
		first.Go(func() {
			lookup, err := s.client.lookup(ctx, asin, isbn)
			switch {
			case err != nil:
				lookupErr = err
			case lookup.Work != nil:
				lookupHit = &hit{id: lookup.Work.ID, recordingID: lookup.RecordingID, cover: deref(lookup.Work.CoverURL), identified: true}
			}
		})
	}
	if text != "" {
		first.Go(func() { search, findErr = s.client.searchWorks(ctx, text, maxMatchCandidates) })
	}
	first.Wait()

	var hits []hit
	if lookupHit != nil {
		hits = append(hits, *lookupHit)
	}
	if search != nil {
		for _, r := range search.Results {
			if r.ID != "" && !slices.ContainsFunc(hits, func(h hit) bool { return h.id == r.ID }) {
				hits = append(hits, hit{id: r.ID, cover: deref(r.CoverURL)})
			}
		}
	}
	// One failed leg (the lookup or the search) still leaves the other's hits worth
	// showing; only when neither produced any is a failure an outage.
	if len(hits) == 0 {
		for _, err := range []error{lookupErr, findErr} {
			if err != nil && !errors.Is(err, ErrNotFound) {
				return nil, err
			}
		}
	}
	if len(hits) > maxMatchCandidates {
		hits = hits[:maxMatchCandidates]
	}

	// Expand each hit into its full work document, a few at a time.
	out := make([]*MatchCandidate, len(hits))
	errs := make([]error, len(hits))
	var wg sync.WaitGroup
	for i, h := range hits {
		wg.Go(func() {
			detail, err := s.fetchWork(ctx, h.id)
			if err != nil {
				errs[i] = err
				return
			}
			c := s.toCandidate(detail, h.cover)
			c.RecordingID = h.recordingID
			if h.identified {
				c.Score = 100
			} else {
				c.Score = scoreCandidate(q, c)
			}
			out[i] = c
		})
	}
	wg.Wait()

	cands := make([]MatchCandidate, 0, len(hits))
	var firstErr error
	for i, c := range out {
		switch {
		case c != nil:
			cands = append(cands, *c)
		case !errors.Is(errs[i], ErrNotFound) && firstErr == nil:
			firstErr = errs[i]
		}
	}
	// Some candidates are better than none; an all-failed fan-out is an outage.
	if len(cands) == 0 && firstErr != nil {
		return nil, firstErr
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })
	return cands, nil
}

func (s *Service) toCandidate(d *upstreamWorkDetail, cover string) *MatchCandidate {
	c := &MatchCandidate{
		WorkID: d.ID, Title: d.Title, Subtitle: d.Subtitle, Authors: toPersonRefs(d.Authors),
		Language: d.Language, FirstPublished: d.FirstPublished, Description: d.Description,
		Series: []MatchSeries{}, CoverURL: cover, WebURL: s.workURL(d.ID), Recordings: []MatchRecording{},
	}
	for _, sr := range d.Series {
		// Alternate reading orders of one series repeat it; the console wants the
		// series once, at its main position.
		if sr.OrderingOf == "" {
			c.Series = append(c.Series, MatchSeries{Name: sr.Name, Position: sr.Position})
		}
	}
	for _, r := range d.Recordings {
		mr := MatchRecording{
			ID: r.ID, Narrators: toPersonRefs(r.Narrators), Abridged: r.Abridged, RuntimeMin: r.RuntimeMin,
			ReleaseDate: r.ReleaseDate, Publisher: r.Publisher, ASINs: []string{}, ISBNs: []string{}, CoverURL: r.CoverURL,
		}
		for _, a := range r.ASIN {
			if a.ASIN != "" && !slices.Contains(mr.ASINs, a.ASIN) {
				mr.ASINs = append(mr.ASINs, a.ASIN)
			}
		}
		mr.ISBNs = append(mr.ISBNs, r.ISBN...)
		if c.CoverURL == "" {
			c.CoverURL = r.CoverURL
		}
		c.Recordings = append(c.Recordings, mr)
	}
	return c
}

// Score weights: the title says most about identity, the author confirms it, the
// runtime separates editions (and abridgements) of the same work.
const (
	weightTitle   = 55.0
	weightAuthor  = 30.0
	weightRuntime = 15.0
)

// scoreCandidate rates a candidate against the book, 0-100. A fact the book (or
// the candidate) lacks is left out of the denominator rather than counted as a
// mismatch, so an untagged book isn't penalized for having no author.
func scoreCandidate(q MatchQuery, c *MatchCandidate) int {
	var got, total float64
	if q.Title != "" {
		total += weightTitle
		got += weightTitle * titleSimilarity(q.Title, c.Title, c.Subtitle)
	}
	if q.Author != "" && len(c.Authors) > 0 {
		total += weightAuthor
		best := 0.0
		for _, a := range c.Authors {
			best = math.Max(best, tokenSimilarity(q.Author, a.Name))
		}
		got += weightAuthor * best
	}
	if q.Duration > 0 {
		if fit, ok := runtimeFit(q.Duration, c.Recordings); ok {
			total += weightRuntime
			got += weightRuntime * fit
		}
	}
	if total == 0 {
		return 0
	}
	return int(math.Round(100 * got / total))
}

// titleSimilarity compares a book title with a work's title (and title plus
// subtitle, since files often carry both): 1 when the normalized forms agree, else
// the word overlap.
func titleSimilarity(title, workTitle, subtitle string) float64 {
	full := workTitle
	if subtitle != "" {
		full += " " + subtitle
	}
	n := match.Normalize(title)
	if n != "" && (n == match.Normalize(workTitle) || n == match.Normalize(full)) {
		return 1
	}
	return math.Max(tokenSimilarity(title, workTitle), tokenSimilarity(title, full))
}

// tokenSimilarity is the Jaccard overlap of two strings' lowercase words.
func tokenSimilarity(a, b string) float64 {
	ta, tb := words(a), words(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for w := range ta {
		if tb[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9' || r > 127)
	}) {
		out[w] = true
	}
	return out
}

// runtimeFit scores the closest recording's runtime against the book's duration:
// within 3% is the same edition, within 10% plausibly so; ok is false when no
// recording states a runtime.
func runtimeFit(duration float64, recs []MatchRecording) (float64, bool) {
	best, ok := 0.0, false
	for _, r := range recs {
		if r.RuntimeMin <= 0 {
			continue
		}
		ok = true
		off := math.Abs(float64(r.RuntimeMin)*60-duration) / duration
		switch {
		case off <= 0.03:
			best = 1
		case off <= 0.10:
			best = math.Max(best, 0.5)
		}
	}
	return best, ok
}

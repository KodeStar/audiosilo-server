package meta

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// Matching: the admin console's "Match with community" dialog. Given a book the
// server holds, find the community works it might be, each with its recordings, so
// an admin can attach one (its ASIN/ISBN, through the existing enrichment write) and
// accept fields from it (as community-sourced overrides). Admin-only and
// interactive, so results are not cached; the fan-out is bounded instead.
//
// The candidates come from metaserve's STRUCTURED match (GET works/match), which
// takes the book's facts separately - title and author guesses from the tags AND
// from the folder path, the series folder and the volume number, the runtime, the
// typed text, an identifier - because a book's tags are often garbage where its
// path is good, and works/search, which needs every word of one query, finds
// neither. An older metaserve without the route answers 404, and then the dialog
// searches exactly as it did before (searchHits, frozen).

// maxMatchCandidates caps how many search hits are expanded into full works.
const maxMatchCandidates = 6

// MatchQuery describes the book being matched. Text is what the admin typed and
// ASIN/ISBN an identifier they gave; when all three are empty the book's own
// identifiers are looked up. Title, Series, SeriesIndex and Author are the book's
// (tagged or edited) facts, Path and IsFolder its library path, which is read for
// facts of its own (metadata.ReadPathLayout); all of them, with Duration, find and score
// the candidates. Region is the server's preferred Audible marketplace (config
// metadata.region, "" for none), which orders each recording's ASINs; Limit caps
// the works expanded (0 = maxMatchCandidates; a bulk run needs only the best two).
type MatchQuery struct {
	Text        string
	ASIN        string
	ISBN        string
	Title       string
	Series      string
	SeriesIndex float64
	Author      string
	Duration    float64 // seconds; 0 when unknown
	Path        string  // the book's rel_path
	IsFolder    bool
	BookASIN    string
	BookISBN    string
	Region      string
	Limit       int
}

// limit is how many works the query expands.
func (q MatchQuery) limit() int {
	if q.Limit > 0 && q.Limit < maxMatchCandidates {
		return q.Limit
	}
	return maxMatchCandidates
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
	// DefaultRecordingID is the recording the book most likely is
	// (DefaultRecording): what the console's dialog and a bulk run start from.
	DefaultRecordingID string `json:"default_recording_id,omitempty"`
	// Score (0-100) is how well the work fits the book: metaserve's structured
	// match score, 100 for an identifier hit, or (against an older metaserve)
	// the title, author and runtime agreement scoreCandidate finds.
	Score int `json:"score"`
	// Reasons say which facts agreed, as metaserve's match reports them. From
	// the older search it is absent, except on an identifier lookup's hit,
	// which carries the identifier it was found by.
	Reasons *MatchReasons `json:"reasons,omitempty"`
}

// MatchReasons is metaserve's account of a match score (works/match `reasons`),
// passed through unchanged. Each field is omitted when the request did not let
// it be judged.
type MatchReasons struct {
	// Title is the best title similarity (0-1) over the title guesses.
	Title *float64 `json:"title,omitempty"`
	// Text is the typed text's similarity (0-1), read as a title.
	Text *float64 `json:"text,omitempty"`
	// Author is "full", "surname" or "none".
	Author string `json:"author,omitempty"`
	// Series is "position", "name", "conflict" or "none".
	Series string `json:"series,omitempty"`
	// Runtime is the relative difference to the closest recording (0.02 = 2%).
	Runtime *float64 `json:"runtime,omitempty"`
	// Identifier is "asin" or "isbn" when the work was found by it.
	Identifier string `json:"identifier,omitempty"`
}

// MatchSeries is a series the work belongs to, with its position.
type MatchSeries struct {
	Name     string `json:"name"`
	Position string `json:"position"`
}

// MatchRecording is one recording (narration/edition) of a candidate work.
// ASINs are best first for this server (orderASINs): the preferred marketplace's,
// then the US store's, then the rest. ASINRefs keeps each with its marketplace, as
// metaserve lists them, and ASINRegion names the first one's.
type MatchRecording struct {
	ID          string          `json:"id"`
	Narrators   []MetaPersonRef `json:"narrators"`
	Abridged    bool            `json:"abridged,omitempty"`
	RuntimeMin  int             `json:"runtime_min,omitempty"`
	ReleaseDate string          `json:"release_date,omitempty"`
	Publisher   string          `json:"publisher,omitempty"`
	ASINs       []string        `json:"asins"`
	ASINRefs    []ASINRef       `json:"asin_refs"`
	// ASINRegion is the marketplace ASINs[0] sells in ("" without an ASIN).
	ASINRegion string   `json:"asin_region,omitempty"`
	ISBNs      []string `json:"isbns"`
	CoverURL   string   `json:"cover_url,omitempty"`
}

// ASINRef is one of a recording's ASINs with the Audible marketplace it sells in
// (metaserve's region vocabulary: us, uk, ca, au, de, fr, es, it, jp, in, br). A
// publisher-direct ASIN sold worldwide under one id appears once per region.
type ASINRef struct {
	Region string `json:"region"`
	ASIN   string `json:"asin"`
}

// fallbackRegion is the marketplace whose ASIN comes next when the preferred one
// has none: the one nearly every recording carries, and metaserve's own default.
const fallbackRegion = "us"

// orderASINs is a recording's distinct ASINs best first for a server preferring
// region: that marketplace's, then fallbackRegion's, then the rest in metaserve's
// order (by region). Without the ordering the first would be whichever region
// sorts first alphabetically (au before uk before us).
func orderASINs(refs []ASINRef, region string) []string {
	out := []string{}
	add := func(match func(ASINRef) bool) {
		for _, r := range refs {
			if r.ASIN != "" && match(r) && !slices.Contains(out, r.ASIN) {
				out = append(out, r.ASIN)
			}
		}
	}
	if region != "" {
		add(func(r ASINRef) bool { return r.Region == region })
	}
	add(func(r ASINRef) bool { return r.Region == fallbackRegion })
	add(func(ASINRef) bool { return true })
	return out
}

// asinRegion is the marketplace to name asin by, as orderASINs put it first: the
// preferred one when it sells there, else fallbackRegion when it sells there (a
// publisher-direct ASIN sold worldwide under one id is listed by region, au
// first), else the first that lists it.
func asinRegion(refs []ASINRef, asin, region string) string {
	best := ""
	for _, r := range refs {
		switch {
		case r.ASIN != asin:
		case region != "" && r.Region == region:
			return r.Region
		case best == "" || r.Region == fallbackRegion:
			best = r.Region
		}
	}
	return best
}

// RegionASIN is the recording's ASIN in region, "" when it sells there under none.
func (r MatchRecording) RegionASIN(region string) string {
	for _, a := range r.ASINRefs {
		if a.Region == region && a.ASIN != "" {
			return a.ASIN
		}
	}
	return ""
}

// HasRegion reports whether the recording sells in region.
func (r MatchRecording) HasRegion(region string) bool {
	return region != "" && r.RegionASIN(region) != ""
}

// DefaultRecording is the recording of c a book most likely is, as the match
// dialog picks it (admin-ui match-model.ts defaultRecording): the one an
// identifier lookup resolved to, else the one whose runtime is closest to the
// book's (seconds), a recording selling in the preferred region winning a tie;
// recordings without a runtime come last. nil when c has none.
func DefaultRecording(c *MatchCandidate, seconds float64, region string) *MatchRecording {
	var best *MatchRecording
	bestGap := math.Inf(1)
	for i := range c.Recordings {
		r := &c.Recordings[i]
		if c.RecordingID != "" && r.ID == c.RecordingID {
			return r
		}
	}
	for i := range c.Recordings {
		r := &c.Recordings[i]
		gap := math.Inf(1)
		if r.RuntimeMin > 0 {
			gap = math.Abs(float64(r.RuntimeMin)*60 - seconds)
		}
		if best == nil || gap < bestGap || (gap == bestGap && r.HasRegion(region) && !best.HasRegion(region)) {
			best, bestGap = r, gap
		}
	}
	return best
}

// matchHit is one work to expand into a candidate. A hit whose score is known
// carries its reasons (an identifier hit, metaserve's match); one without them
// came from the older search and scoreCandidate scores it.
type matchHit struct {
	id, recordingID, cover string
	score                  int
	reasons                *MatchReasons
}

// matchUnsupportedTTL is how long a metaserve that answered works/match as an
// unknown route is taken to lack it: the dialog then searches directly instead
// of paying for that 404 on every open, and a metaserve upgraded meanwhile is
// noticed within the TTL.
const matchUnsupportedTTL = 15 * time.Minute

// maxMatchIdentifier is the longest asin/isbn works/match reads (metaserve's
// own maxMatchIdentifier).
const maxMatchIdentifier = 20

// maxMatchValueBytes is how much of each text value works/match reads
// (metaserve's maxQueryBytes).
const maxMatchValueBytes = 256

// boundBytes cuts s to at most n bytes on a rune boundary.
func boundBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Candidates finds the community works a book might be, best first. It returns an
// empty list when nothing matches, and an error only when the community service
// could not be reached.
func (s *Service) Candidates(ctx context.Context, q MatchQuery) ([]MatchCandidate, error) {
	ctx, cancel := context.WithTimeout(ctx, s.composeBudget)
	defer cancel()

	text, asin, isbn := strings.TrimSpace(q.Text), normalizeASIN(q.ASIN), normalizeISBN(q.ISBN)
	// An identifier the admin typed (with no text) asks for that record alone.
	identifierOnly := text == "" && (asin != "" || isbn != "")
	if text == "" && asin == "" && isbn == "" {
		asin, isbn = normalizeASIN(q.BookASIN), normalizeISBN(q.BookISBN)
	}

	var (
		hits    []matchHit
		legErrs []error
		matched bool
	)
	// A book that says nothing metaserve can match on (no title, author, series
	// or usable identifier anywhere) would only earn a 400, and the search has
	// nothing to look for either: an empty answer, without a request.
	params := matchParams(q, text, asin, isbn)
	if !identifierOnly && !hasMatchFacts(params) {
		return []MatchCandidate{}, nil
	}
	if !identifierOnly && s.now().UnixNano() >= s.matchUnsupportedUntil.Load() {
		// One request answers it all: metaserve looks the identifier up too.
		res, err := s.client.matchWorks(ctx, params)
		switch {
		case err == nil:
			matched = true
			for _, r := range res {
				if r.ID == "" {
					continue
				}
				h := matchHit{id: r.ID, cover: deref(r.CoverURL), score: r.Score, reasons: r.Reasons}
				if h.reasons == nil {
					h.reasons = &MatchReasons{}
				}
				if h.reasons.Identifier != "" {
					h.recordingID = r.RecordingID
				}
				hits = append(hits, h)
			}
		case ctx.Err() == nil && routeMissing(err):
			s.matchUnsupportedUntil.Store(s.now().Add(matchUnsupportedTTL).UnixNano())
		default:
			// A metaserve that HAS the route but failed (a 503 over its budget, a
			// 5xx, a timeout) is an outage the dialog reports, not a reason to
			// answer from a weaker search.
			return nil, err
		}
	}
	if !matched {
		hits, legErrs = s.searchHits(ctx, q, text, asin, isbn, identifierOnly)
	}
	if len(hits) > q.limit() {
		hits = hits[:q.limit()]
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
			c := s.toCandidate(detail, h.cover, q.Region)
			c.RecordingID = h.recordingID
			if rec := DefaultRecording(c, q.Duration, q.Region); rec != nil {
				c.DefaultRecordingID = rec.ID
			}
			if h.reasons != nil {
				c.Score, c.Reasons = h.score, h.reasons
			} else {
				c.Score = scoreCandidate(q, c)
			}
			out[i] = c
		})
	}
	wg.Wait()

	cands := make([]MatchCandidate, 0, len(hits))
	var expandErr error
	for i, c := range out {
		switch {
		case c != nil:
			cands = append(cands, *c)
		case !errors.Is(errs[i], ErrNotFound) && expandErr == nil:
			expandErr = errs[i]
		}
	}
	// Some candidates are better than none: one failed leg (the lookup or the
	// search) still leaves the other's worth showing. With none, any failure on the
	// way - either leg, or expanding a hit - is an outage, not "no match": the leg
	// that failed may well have found the book.
	if len(cands) == 0 {
		for _, err := range append([]error{expandErr}, legErrs...) {
			if err != nil && !errors.Is(err, ErrNotFound) {
				return nil, err
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })
	return cands, nil
}

// routeMissing reports whether a works/match error says the route does not
// exist - an older metaserve 404s it (through works/{id} reading "match" as an
// id), a 405 or a 200 that is not a match answer says the same - as opposed
// to a metaserve that has it and failed.
func routeMissing(err error) bool {
	var st *statusError
	return errors.Is(err, ErrNotFound) || errors.Is(err, errNoMatchResults) ||
		(errors.As(err, &st) && st.code == http.StatusMethodNotAllowed)
}

// searchHits is the dialog's search before works/match: the identifier lookup
// and works/search, concurrently. It is FROZEN - the fallback for a metaserve
// that predates works/match, kept as it was (and scored by the tag-only
// scoreCandidate) except that a lookup hit now names its identifier in
// reasons. Delete it, with scoreCandidate and the unsupported-route
// memo, once the production metaserve serves works/match. legErrs holds each
// leg's failure.
func (s *Service) searchHits(ctx context.Context, q MatchQuery, text, asin, isbn string, identifierOnly bool) (hits []matchHit, legErrs []error) {
	if text == "" && !identifierOnly {
		// The book's own facts. metaserve's search requires every word, so the title
		// goes in without what tags add and a community record lacks: the series
		// name and "(Unabridged)" or ", Book 1" fluff.
		text = strings.TrimSpace(match.CleanTitle(q.Title, q.Series) + " " + q.Author)
	}
	var (
		lookupHit          *matchHit
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
				by := "isbn"
				if asin != "" {
					by = "asin"
				}
				lookupHit = &matchHit{id: lookup.Work.ID, recordingID: lookup.RecordingID, cover: deref(lookup.Work.CoverURL),
					score: 100, reasons: &MatchReasons{Identifier: by}}
			}
		})
	}
	if text != "" {
		first.Go(func() { search, findErr = s.client.searchWorks(ctx, text, maxMatchCandidates) })
	}
	first.Wait()

	if lookupHit != nil {
		hits = append(hits, *lookupHit)
	}
	if search != nil {
		for _, r := range search.Results {
			if r.ID != "" && !slices.ContainsFunc(hits, func(h matchHit) bool { return h.id == r.ID }) {
				hits = append(hits, matchHit{id: r.ID, cover: deref(r.CoverURL)})
			}
		}
	}
	return hits, []error{lookupErr, findErr}
}

// matchParams is the works/match request for a book: the typed text and
// identifiers, then every guess its path and tags make. Tag and path guesses go
// in side by side because metaserve judges each fact by its BEST guess - a
// garbage tag beside a good path costs nothing - and the path's lead, since
// they are the ones that are usually right when the two disagree. Numbering is
// left on: metaserve reads it ("Sharpe - 08 - Sharpe's Eagle" is volume 8 of
// the series) and cleans each title guess against the series itself.
func matchParams(q MatchQuery, text, asin, isbn string) url.Values {
	p := metadata.ReadPathLayout(q.Path, q.IsFolder)
	v := url.Values{}
	add := func(key, val string) {
		// metaserve reads at most maxMatchValueBytes of each value, so a longer
		// tag is cut here too: past that it only lengthens the URL, and a proxy's
		// 414 would fail the dialog for that book every time.
		val = strings.TrimSpace(boundBytes(strings.TrimSpace(val), maxMatchValueBytes))
		if val != "" && !slices.Contains(v[key], val) {
			v.Add(key, val)
		}
	}
	add("q", text)
	// metaserve ignores a longer identifier (a garbage tag), so one alone must
	// not make a request it would refuse as naming nothing.
	if len(asin) <= maxMatchIdentifier {
		add("asin", asin)
	}
	if len(isbn) <= maxMatchIdentifier {
		add("isbn", isbn)
	}
	add("title", p.Title)
	add("title", q.Title)
	add("author", p.Author)
	add("author", q.Author)
	// Both series guesses go up, the folder's first, then the tag's unless it
	// names the same series: metaserve judges each and counts the better, so the
	// author folder a "Fiction/<author>/<book>" layout puts in the series slot
	// costs nothing beside a right tag. (A metaserve that reads one series reads
	// the folder's, as before.)
	// The folder's own numbering ("03 - Tawny Man") is not part of the name it
	// shares with a tag ("Tawny Man"); metaserve cuts it the same way.
	_, folderName := metadata.SplitSeriesIndex(p.Series)
	differ := p.Series != "" && q.Series != "" && match.Fold(q.Series) != match.Fold(p.Series) &&
		match.Fold(q.Series) != match.Fold(folderName)
	add("series", p.Series)
	if p.Series == "" || differ {
		add("series", q.Series)
	}
	// The tagged position numbers the tagged series, but metaserve takes one
	// position for every series guess, and a stated one replaces the volume a
	// title's numbering carries. So it goes up only when no DIFFERENT series
	// folder goes up with it: beside "03 - Tawny Man" (a book tagged Realm of the
	// Elderlings #15) it would turn the folder's agreement into a conflict, and
	// left out, metaserve takes the volume from the leaf ("TM02 - Golden Fool")
	// while the tag's series still agrees by name. A volume the path gives the
	// series folder ("Stormlight Archive/03") is paired the same way, and leads
	// the tag's like every path fact.
	switch {
	case differ:
	case p.Position != "":
		v.Set("position", p.Position)
	case q.SeriesIndex > 0:
		v.Set("position", strconv.FormatFloat(q.SeriesIndex, 'f', -1, 64))
	}
	if q.Duration >= 1 {
		v.Set("runtime", strconv.Itoa(int(math.Round(q.Duration))))
	}
	v.Set("limit", strconv.Itoa(q.limit()))
	return v
}

// hasMatchFacts reports whether a works/match request names anything metaserve
// can match on; one with none (only a runtime, a position, the limit) it
// refuses with a 400.
func hasMatchFacts(v url.Values) bool {
	for _, k := range []string{"q", "title", "author", "series", "asin", "isbn"} {
		if v.Has(k) {
			return true
		}
	}
	return false
}

// normalizeASIN and normalizeISBN put an identifier in the form metaserve's exact
// lookup holds: an ASIN upper-cased, an ISBN without the hyphens and spaces it is
// usually printed with.
func normalizeASIN(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

func normalizeISBN(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s)))
}

func (s *Service) toCandidate(d *upstreamWorkDetail, cover, region string) *MatchCandidate {
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
			ReleaseDate: r.ReleaseDate, Publisher: r.Publisher, ASINRefs: []ASINRef{}, ISBNs: []string{}, CoverURL: r.CoverURL,
		}
		for _, a := range r.ASIN {
			if ref := ASINRef(a); a.ASIN != "" && !slices.Contains(mr.ASINRefs, ref) {
				mr.ASINRefs = append(mr.ASINRefs, ref)
			}
		}
		mr.ASINs = orderASINs(mr.ASINRefs, region)
		if len(mr.ASINs) > 0 {
			mr.ASINRegion = asinRegion(mr.ASINRefs, mr.ASINs[0], region)
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

// scoreCandidate rates a candidate against the book, 0-100, for the frozen
// works/search fallback (searchHits). A fact the book (or the candidate) lacks
// is left out of the denominator rather than counted as a mismatch, so an
// untagged book isn't penalized for having no author.
func scoreCandidate(q MatchQuery, c *MatchCandidate) int {
	var got, total float64
	if q.Title != "" {
		total += weightTitle
		// The book's title as tagged, or without its series name and edition fluff,
		// whichever fits the work better. Only the book's side is cleaned, so a
		// community work's own "(Dramatized Adaptation)" still tells it apart.
		sim := titleSimilarity(q.Title, c.Title, c.Subtitle)
		if clean := match.CleanTitle(q.Title, q.Series); clean != q.Title {
			sim = math.Max(sim, titleSimilarity(clean, c.Title, c.Subtitle))
		}
		got += weightTitle * sim
	}
	if q.Author != "" && len(c.Authors) > 0 {
		total += weightAuthor
		best := 0.0
		for _, a := range c.Authors {
			best = math.Max(best, authorSimilarity(q.Author, a.Name))
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
// subtitle, since files often carry both): 1 when the folded forms agree, else the
// word overlap. Folding keeps every script's letters (match.Fold): an ASCII-only
// form would equate two different Cyrillic titles sharing a "1".
func titleSimilarity(title, workTitle, subtitle string) float64 {
	full := workTitle
	if subtitle != "" {
		full += " " + subtitle
	}
	n := match.Fold(title)
	if n != "" && (n == match.Fold(workTitle) || n == match.Fold(full)) {
		return 1
	}
	return math.Max(tokenSimilarity(title, workTitle), tokenSimilarity(title, full))
}

// authorSimilarity compares the book's author credit with one of the work's
// authors: 1 when the two fold alike ("JRR Tolkien" / "J. R. R. Tolkien") or when
// every word of a multi-word name is in the credit (a co-written book credited
// "Brandon Sanderson, Mary Robinette Kowal" names both), else the word overlap.
func authorSimilarity(credit, name string) float64 {
	if f := match.Fold(name); f != "" && f == match.Fold(credit) {
		return 1
	}
	if nw := words(name); len(nw) > 1 {
		cw := words(credit)
		all := true
		for w := range nw {
			all = all && cw[w]
		}
		if all {
			return 1
		}
	}
	return tokenSimilarity(credit, name)
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

// words is a string's set of lowercase words: runs of letters and digits, in any
// script.
func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
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

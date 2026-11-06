package meta

import (
	"cmp"
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
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
// ASIN/ISBN an identifier they gave; when all three are empty BookASIN/BookISBN
// are looked up (the book's own identifiers, else its tag hints: matchrun.QueryFor).
// Title, Series, SeriesIndex and Author are the book's
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
	// SeriesIndexEnd is the end of a SeriesIndex that is a range (an omnibus of
	// books 1-3; 0 when it isn't one): the position goes up as "1-3", which
	// metaserve compares as a span, never as the "1" of book 1.
	SeriesIndexEnd float64
	Author         string
	Duration       float64 // seconds; 0 when unknown
	Path           string  // the book's rel_path
	IsFolder       bool
	BookASIN       string
	BookISBN       string
	Region         string
	Limit          int
	// NoSeries says the caller reads no candidate's series (a repick), so the
	// series records the series rules read aren't fetched for it.
	NoSeries bool
	// Language is the language the book says it is in (QueryFor: the book's
	// language, else its tags'), "" for unknown.
	// metaserve reads a language tag or name ("en", "eng", "English") and
	// scores a work it knows to be in another language lower; an older
	// metaserve ignores it.
	Language string
	// Genres are the book's genres (Book.Genres). Only read here: a genre naming a
	// dramatized form ("Radio Drama") says the book is one (statesDramatization);
	// metaserve is not sent them.
	Genres []string
	// Narrators are the book's narrators, one name each (names.Split of its
	// narrator credit). metaserve counts a recording they narrated in the
	// work's favour (a name that is also the author isn't sent: matchParams),
	// and DefaultRecording prefers one; an older metaserve ignores them.
	Narrators []string
	// BestOnly says the caller reads the whole of the best candidate alone, and of
	// the next only Score and IdentifierHit (a bulk run: matchrun's matchBook and
	// Classify). So works/match is asked to inline the best's work and series facts
	// (expand=work), and nothing else is read: every other candidate comes back a
	// stub, its work id, cover, score and reasons. A metaserve that inlines nothing
	// costs the fetches for the best. The dialog lists every candidate and leaves it
	// off.
	BestOnly bool
	// NoDetailsBelow, with BestOnly, is the score under which the caller drops a best
	// candidate no identifier found (matchrun.ReviewFloor): a stub too, inlined
	// details or none. 0 for no such score.
	NoDetailsBelow int
	// WorkID asks for that community work alone (works/{id}), whatever else the
	// query says: a refresh of a book whose recorded match has no ASIN or ISBN
	// (catalog.MatchAnchor). A retired id's redirect is followed, so the
	// candidate is the record it was merged into (its WorkID the new one); an
	// unknown id is no candidate. RecordingID is the recording the book was
	// matched to: the candidate's RecordingID while the work still lists it.
	WorkID      string
	RecordingID string
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
	// PlainBook says the book's own text doesn't state it is a dramatized production,
	// which is what keeps DefaultRecording off a dramatization.
	PlainBook bool `json:"-"`
	// DramatizationOnly says the work holds only a dramatization the book isn't, so
	// DefaultRecording found none: the console's dialog preselects nothing for it.
	DramatizationOnly bool `json:"dramatization_only,omitempty"`
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

// IdentifierHit reports whether the identifier the query sent (the book's own
// ASIN or ISBN, or a tag hint) found the work.
func (c MatchCandidate) IdentifierHit() bool {
	return c.Reasons != nil && c.Reasons.Identifier != ""
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
	// Narrator is "full", "surname" or "none": how well the book's narrators
	// name one of the best-agreeing recording's (sent narrators only).
	Narrator string `json:"narrator,omitempty"`
	// Language is "same" or "other" when the book's language and the work's
	// are both known.
	Language string `json:"language,omitempty"`
}

// MatchSeries is a series the work belongs to, with its position and its
// community id.
type MatchSeries struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Position string `json:"position"`
}

// MatchRecording is one recording (narration/edition) of a candidate work.
// ASINs are best first for this server (orderASINs): the preferred marketplace's,
// then the US store's, then the rest. ASINRefs keeps each with its marketplace, as
// metaserve lists them (an ASIN the work was found by first: toCandidate), and
// ASINRegion names the first one's.
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
	// Series is the series a book that is this recording is in, its main one
	// first: the candidate's, with an edition named for this recording in front
	// (seriesrules.go). The console and a bulk run read it, so neither has a
	// copy of the rule.
	Series []MatchSeries `json:"series"`
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

// A dramatization is a recording of its book, so a plain library book rightly
// matches a work holding one. What it must not do is take that recording. This is
// metaserve's rule by hand (model.Recording.IsDramatization, titlerule.DramatizationOf):
// change one, change the other (CROSS-REPO.md).
var (
	// graphicAudioRE is GraphicAudio's imprint in every spelling the catalogue holds.
	graphicAudioRE = regexp.MustCompile(`(?i)^\s*graphic\s?audio(\s+llc)?\s*$`)
	// dramatizationTextRE is a text saying it is a dramatized production. Narrower
	// than fullCastRE: "Radio Free Albemuth" states nothing here.
	dramatizationTextRE = regexp.MustCompile(`(?i)\b(?:full[- ]?cast|graphic\s?audio|dramati[sz](?:ed|ations?))\b`)
	// adaptationLabelRE is the label metaserve's importer takes off a title, as a whole
	// bracketed group.
	adaptationLabelRE = regexp.MustCompile(`(?i)[(\[]\s*(?:full[- ]cast\s+)?dramati[sz]ed\s+adaptation\s*[)\]]`)
	// splitPartRE is a split-release part, whose title keeps the label.
	splitPartRE = regexp.MustCompile(`(?i)[(\[]\s*(?:(?:part|pt\.?|teil)\s+)?[0-9]+\s+(?:of|von)\s+[0-9]+\s*[)\]]|,?\s*vol\.\s*[0-9]+\s+of\s+[0-9]+|\b(?:parts?|pts?\.?)\s*(?:[0-9]+|one|two|three|four|five|six|seven|eight|nine|ten)\b`)
)

// titleSpeaksFor reports whether a work's title or subtitle speaks for a recording of
// this publisher that says nothing of itself. A statement that stays on the title
// ("(Dramatized)", a split part's label) names the production every recording is,
// whoever published it. The adaptation label is coming off titles, and a plain
// reading can sit under it until then, so it speaks only for a recording stating no
// publisher.
func titleSpeaksFor(title, subtitle, publisher string) bool {
	for _, t := range []string{title, subtitle} {
		if !dramatizationTextRE.MatchString(t) {
			continue
		}
		if publisher == "" {
			return true
		}
		if splitPartRE.MatchString(t) || dramatizationTextRE.MatchString(adaptationLabelRE.ReplaceAllString(t, " ")) {
			return true
		}
	}
	return false
}

// saysDramatization reports whether a recording says of itself it is a dramatized
// production: GraphicAudio's imprint or a full-cast credit. Several named readers are
// not enough, as a volunteer reading credits them too.
func (r *MatchRecording) saysDramatization() bool {
	return graphicAudioRE.MatchString(r.Publisher) ||
		slices.ContainsFunc(r.Narrators, func(n MetaPersonRef) bool { return castMemberRE.MatchString(n.Name) })
}

// isDramatization reports whether a recording of the work c is one: it says so, or the
// work's title speaks for it. When no recording of the work says so, any statement on
// the title is the work's only one and covers them all: metaserve's importer keeps the
// label on such a work.
func (r *MatchRecording) isDramatization(c *MatchCandidate) bool {
	if r.saysDramatization() || titleSpeaksFor(c.Title, c.Subtitle, r.Publisher) {
		return true
	}
	return titleSpeaksFor(c.Title, c.Subtitle, "") &&
		!slices.ContainsFunc(c.Recordings, func(o MatchRecording) bool { return o.saysDramatization() })
}

// statesDramatization reports whether the book's own text says it is one: its title,
// series, folders, typed text, author or narrator credit ("Michael J. Sullivan -
// Graphic Audio"), or a genre naming a dramatized form ("Radio Drama").
func (q MatchQuery) statesDramatization() bool {
	return slices.ContainsFunc(slices.Concat([]string{q.Text, q.Title, q.Series, q.Path, q.Author}, q.Narrators, q.Genres), dramatizationTextRE.MatchString) ||
		slices.ContainsFunc(q.Genres, dramaGenreRE.MatchString)
}

// dramaGenreRE is a genre naming a dramatized form beyond what dramatizationTextRE
// reads anywhere: a radio or audio drama or play (the BBC's own tags say "Radio
// Drama"; "Audio-Drama", "Audioplays") or a German Hörspiel, "Hoerspiel" in ASCII and
// inside a compound ("Kinderhörspiel").
var dramaGenreRE = regexp.MustCompile(`(?i)\b(?:radio|audio)[\s-]?(?:drama|play)s?\b|h(?:ö|oe)rspiel`)

// lengthSays reports whether a dramatization's runtime is close enough to a plain
// book's of seconds for its length alone to say the book is that production, when the
// work holds nothing else (DefaultRecording): within 1% of the book's length, or two
// minutes on a short one (the dramatization slack). A store rounds a runtime to the
// minute and a rip trims a second or two, so a copy of the production lands inside
// it; another production of the text hardly ever does (the BBC's 1984 collection, 1.9%
// off the work's dramatization, is one of its own).
func lengthSays(r *MatchRecording, seconds float64) bool {
	off, known := RuntimeOff(r.RuntimeMin, seconds)
	return known && off <= max(0.01, 120/seconds)
}

// DefaultRecording is the recording of c a book most likely is, as the match
// dialog picks it (admin-ui match-model.ts defaultRecording): the one an
// identifier lookup resolved to, else, among the recordings one of the book's
// narrators narrated that may be its edition by length (PlausibleRuntimeOff,
// or a length unknown; all of them when there is none such, or the book names
// no narrator), the one whose runtime is closest to the book's (seconds), a
// recording selling in the preferred region winning a tie; recordings without
// a runtime come last. A narrator's recording of another length is another
// edition: the closest runtime then decides. A plain book (MatchCandidate.PlainBook)
// takes a dramatization only when an identifier names it, or one of its narrators reads
// it at a plausible length and reads no plain recording that fits as well, or the work
// holds nothing but dramatizations and this one's runtime is the book's
// (lengthSays: the length says the book is that production); so a work
// holding only dramatizations of other lengths gives nil: the work still fits, but no
// recording does. nil when c has none.
func DefaultRecording(c *MatchCandidate, seconds float64, region string, narrators []string) *MatchRecording {
	return chooseRecording(c, seconds, region, narrators, c.PlainBook && (c.Reasons == nil || c.Reasons.Identifier == ""), nil)
}

// FitRecording is rec, the recording DefaultRecording picked for a book of seconds
// read by narrators, unless rec is another edition by length (more than
// PlausibleRuntimeOff off) that none of the book's narrators reads while another
// recording of c may be the book's by length: then the one of those DefaultRecording's
// order puts first. A tag's ASIN can name another recording of the book's work (the
// plain reading's ASIN on a GraphicAudio rip), and so can metaserve's guess; the work
// is still the book, the recording the length agrees with is the better guess. A
// narrator reading rec agrees with the hint against the length (a release of the same
// production at another length), so it stays: within NarratedRuntimeOff a run applies
// it, further off the length holds the book for review. It stays even when that
// narrator reads another recording of c that fits (server#325: swapping to it gained
// no book on the LibraForge benchmark, and swapping within NarratedRuntimeOff too lost
// one whose hint was right). A match run calls it, where an identifier is always a
// tag's hint; never on the book's own identifier (a refresh, a repick), which names
// the recording the book is. rec nil (the work holds only a dramatization the book
// isn't) stays nil. The identifier vouched for rec alone, so a plain book
// (MatchCandidate.PlainBook) gives way to a dramatization only as when nothing named
// one (DefaultRecording's rule).
func FitRecording(c *MatchCandidate, rec *MatchRecording, seconds float64, region string, narrators []string) *MatchRecording {
	fits := func(r *MatchRecording) bool {
		off, known := RuntimeOff(r.RuntimeMin, seconds)
		return known && off <= PlausibleRuntimeOff
	}
	if rec == nil || ReadBy(rec, narrators) || rec.RuntimeMin <= 0 || seconds <= 0 || fits(rec) {
		return rec
	}
	return cmp.Or(chooseRecording(c, seconds, region, narrators, c.PlainBook, fits), rec)
}

// chooseRecording is DefaultRecording's choice among the recordings keep admits (nil:
// all); an identifier's recording that keep refuses is passed over. guarded keeps a
// plain book off a dramatization nothing else names.
func chooseRecording(c *MatchCandidate, seconds float64, region string, narrators []string, guarded bool, keep func(*MatchRecording) bool) *MatchRecording {
	if keep == nil {
		keep = func(*MatchRecording) bool { return true }
	}
	// A plain book whose work holds only dramatizations is one when its length
	// names it (lengthSays).
	dramaOnly := guarded && !slices.ContainsFunc(c.Recordings, func(r MatchRecording) bool { return !r.isDramatization(c) })
	lengthNames := func(r *MatchRecording) bool { return dramaOnly && lengthSays(r, seconds) }
	for i := range c.Recordings {
		r := &c.Recordings[i]
		if c.RecordingID != "" && r.ID == c.RecordingID && keep(r) && !(guarded && r.isDramatization(c) && !lengthNames(r)) {
			return r
		}
	}
	var best *MatchRecording
	bestGap, bestTheirs, bestDrama := math.Inf(1), false, false
	for i := range c.Recordings {
		r := &c.Recordings[i]
		off, known := RuntimeOff(r.RuntimeMin, seconds)
		theirs := (!known || off <= PlausibleRuntimeOff) && ReadBy(r, narrators)
		drama := guarded && r.isDramatization(c)
		if !keep(r) || (drama && !theirs && !lengthNames(r)) {
			continue
		}
		gap := math.Inf(1)
		if r.RuntimeMin > 0 {
			gap = math.Abs(float64(r.RuntimeMin)*60 - seconds)
		}
		if best == nil || (theirs && !bestTheirs) || (theirs == bestTheirs && ((bestDrama && !drama) || (drama == bestDrama && (gap < bestGap ||
			(gap == bestGap && r.HasRegion(region) && !best.HasRegion(region)))))) {
			best, bestGap, bestTheirs, bestDrama = r, gap, theirs, drama
		}
	}
	return best
}

// ReadBy reports whether one of the book's narrators narrated r: a name that
// folds like one of r's narrators, or a multi-word one whose every word the
// other holds, either way round: a credit and the community's list spell a
// middle initial as they please ("George Wilson", "George K. Wilson").
func ReadBy(r *MatchRecording, narrators []string) bool {
	for _, n := range narrators {
		for _, rn := range r.Narrators {
			if authorSimilarity(n, rn.Name) == 1 || authorSimilarity(rn.Name, n) == 1 {
				return true
			}
		}
	}
	return false
}

// DramatizesPlainBook reports whether r, one of c's recordings, is a dramatized
// production (isDramatization) while the book states none: neither its own text
// (c.PlainBook) nor its narrator credit ("Full Cast", "GraphicAudio":
// dramatizationTextRE; a lookup by identifier alone sends no text, so the credit is
// all a refresh has). The book's reader among a cast's credits then says nothing
// about its length: the unabridged reading and the dramatization are not one
// production.
func DramatizesPlainBook(c *MatchCandidate, r *MatchRecording, narrators []string) bool {
	return c.PlainBook && !slices.ContainsFunc(narrators, dramatizationTextRE.MatchString) && r.isDramatization(c)
}

// PersonNarrator reports whether a narrator credit names a reader: not a cast or a
// production ("Full Cast", "Full Cast Recording", "GraphicAudio [Dramatized
// Adaptation]": dramatizationTextRE) and not a stand-in for nobody in particular
// ("Various", "Multiple Narrators", "Unknown", "Anonymous", a bare "cast" a
// co-credit splits off). Such a credit agrees with every recording that carries it,
// so it says nothing about which edition a book is.
func PersonNarrator(name string) bool {
	if dramatizationTextRE.MatchString(name) {
		return false
	}
	switch match.Fold(name) {
	case "", "various", "variousnarrators", "variousreaders", "variousartists", "variousvoices",
		"multiplenarrators", "multiplereaders", "multiplevoices", "unknown", "unknownnarrator",
		"anonymous", "anon", "uncredited", "cast", "narrator", "narrators", "none", "na", "va":
		return false
	}
	return true
}

// NormalizeLanguage reads a language tag as works/match takes it: the first of
// several ("eng;fre", "en/de") without a note ("English (US)"), a BCP-47 tag's
// primary subtag ("en-GB" -> "en"), lowercased (metaserve reads the names and
// 639-2 codes, "English", "eng"). "" for none, so a book's language falls back to
// the next that says one: blank, not a word, or a placeholder for no language
// ("und", "zxx", "mul", "mis", "xxx", "unknown", "N/A").
func NormalizeLanguage(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, ";,/|("); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if primary, _, ok := strings.Cut(strings.ReplaceAll(s, "_", "-"), "-"); ok && len(primary) >= 2 && len(primary) <= 3 {
		s = primary
	}
	switch s {
	case "und", "zxx", "mul", "mis", "xxx", "unknown", "none", "na":
		return ""
	}
	if len(s) < 2 || strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsMark(r) }) {
		return ""
	}
	return s
}

// maxMatchNarrators is how many of a book's narrators a works/match request
// names: a full cast's credit adds nothing past the first few.
const maxMatchNarrators = 4

// matchHit is one work to expand into a candidate. A hit whose score is known
// carries its reasons (an identifier hit, metaserve's match); one without them
// came from the older search and scoreCandidate scores it.
type matchHit struct {
	id, recordingID, cover string
	score                  int
	reasons                *MatchReasons
	// work and facts are the work document and series facts works/match inlined
	// (expand=work; only ever on its best result), nil when it inlined none.
	work  *upstreamWorkDetail
	facts map[string]*seriesFacts
}

// stub is the candidate h is without its work's details: what a caller of
// MatchQuery.BestOnly reads of a candidate other than the best.
func (h matchHit) stub() MatchCandidate {
	return MatchCandidate{WorkID: h.id, CoverURL: h.cover, RecordingID: h.recordingID, Score: h.score, Reasons: h.reasons,
		Authors: []MetaPersonRef{}, Series: []MatchSeries{}, Recordings: []MatchRecording{}}
}

// compareMatches is Candidates' order, best first: the higher score, and at one
// score the work the identifier named, which is the book whatever else scores as
// well (metaserve breaks a tie on the title, so it can rank that one second).
func compareMatches(aScore int, aIdentified bool, bScore int, bIdentified bool) int {
	if c := cmp.Compare(bScore, aScore); c != 0 {
		return c
	}
	switch {
	case aIdentified && !bIdentified:
		return -1
	case bIdentified && !aIdentified:
		return 1
	}
	return 0
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

	if id := strings.TrimSpace(q.WorkID); id != "" {
		return s.workCandidates(ctx, q, id)
	}
	text, asin, isbn := strings.TrimSpace(q.Text), match.NormalizeASIN(q.ASIN), match.NormalizeISBN(q.ISBN)
	// An identifier the admin typed (with no text) asks for that record alone.
	identifierOnly := text == "" && (asin != "" || isbn != "")
	if text == "" && asin == "" && isbn == "" {
		asin, isbn = match.NormalizeASIN(q.BookASIN), match.NormalizeISBN(q.BookISBN)
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
				h := matchHit{id: r.ID, cover: deref(r.CoverURL), score: r.Score, reasons: r.Reasons, work: r.Work, facts: r.SeriesFacts}
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
	if q.BestOnly && matched {
		return s.bestCandidates(ctx, q, hits, asin)
	}

	// Expand each hit into its full work document, a few at a time, then read the
	// facts of their series once each (works often share one).
	details := make([]*upstreamWorkDetail, len(hits))
	errs := make([]error, len(hits))
	var wg sync.WaitGroup
	for i, h := range hits {
		wg.Go(func() { details[i], errs[i] = s.fetchWork(ctx, h.id) })
	}
	wg.Wait()
	facts := map[string]*seriesFacts{}
	if !q.NoSeries {
		facts = s.seriesFactsFor(ctx, slices.DeleteFunc(slices.Clone(details), func(d *upstreamWorkDetail) bool { return d == nil }))
	}
	out := make([]*MatchCandidate, len(hits))
	for i, h := range hits {
		if detail := details[i]; detail != nil {
			out[i] = s.hitCandidate(q, h, detail, facts, asin)
		}
	}

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
	slices.SortStableFunc(cands, func(a, b MatchCandidate) int {
		return compareMatches(a.Score, a.IdentifierHit(), b.Score, b.IdentifierHit())
	})
	return cands, nil
}

// workCandidates is Candidates' answer for MatchQuery.WorkID: the work id names,
// as one candidate (score 100, no reasons: it was named, not matched), or none for
// an id the community doesn't know (a 404 after any redirect). The recording the
// query names is kept while the work lists it, and then speaks for itself as an
// identifier's does (no plain-book guard: the admin or a run chose it); else ""
// and DefaultRecording picks one by length.
func (s *Service) workCandidates(ctx context.Context, q MatchQuery, id string) ([]MatchCandidate, error) {
	detail, err := s.fetchWork(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return []MatchCandidate{}, nil
	}
	if err != nil {
		return nil, err
	}
	var facts map[string]*seriesFacts
	if !q.NoSeries {
		facts = s.seriesFactsFor(ctx, []*upstreamWorkDetail{detail})
	}
	c := s.toCandidate(detail, facts, "", q.Region, "")
	c.Score = 100
	if rid := strings.TrimSpace(q.RecordingID); rid != "" && slices.ContainsFunc(c.Recordings, func(r MatchRecording) bool { return r.ID == rid }) {
		c.RecordingID = rid
	}
	c.PlainBook = c.RecordingID == "" && !q.statesDramatization()
	setDefaultRecording(c, q)
	return []MatchCandidate{*c}, nil
}

// hitCandidate is hit h as a candidate, from its work document and series facts.
// asin is the identifier the query sent.
func (s *Service) hitCandidate(q MatchQuery, h matchHit, detail *upstreamWorkDetail, facts map[string]*seriesFacts, asin string) *MatchCandidate {
	foundBy := ""
	if h.reasons != nil && h.reasons.Identifier == "asin" {
		foundBy = asin
	}
	c := s.toCandidate(detail, facts, h.cover, q.Region, foundBy)
	c.RecordingID = h.recordingID
	c.PlainBook = !q.statesDramatization()
	if h.reasons != nil {
		c.Score, c.Reasons = h.score, h.reasons
	} else {
		c.Score = scoreCandidate(q, c)
	}
	setDefaultRecording(c, q)
	return c
}

// setDefaultRecording sets c's DefaultRecording for q (by length, marketplace and
// narrators), or marks it DramatizationOnly when it has recordings but none fits.
func setDefaultRecording(c *MatchCandidate, q MatchQuery) {
	if rec := DefaultRecording(c, q.Duration, q.Region, q.Narrators); rec != nil {
		c.DefaultRecordingID = rec.ID
	} else {
		c.DramatizationOnly = len(c.Recordings) > 0
	}
}

// bestCandidates is Candidates' answer for MatchQuery.BestOnly, from works/match's
// hits: what it answers without the flag, as far as the caller reads it, for the
// fewest requests. The hits are put in Candidates' order first, as the best is the
// one the details are for. A best whose details can't be read gives way to the
// next, and every one failing is the first outage, exactly as Candidates reads them.
// So after an outage the next is read however weak: a stub asks nothing, and would
// answer for a service that is down, the book a weak match and the run none the wiser.
func (s *Service) bestCandidates(ctx context.Context, q MatchQuery, hits []matchHit, asin string) ([]MatchCandidate, error) {
	slices.SortStableFunc(hits, func(a, b matchHit) int {
		return compareMatches(a.score, a.reasons.Identifier != "", b.score, b.reasons.Identifier != "")
	})
	var outage error
	for i, h := range hits {
		best, err := s.bestCandidate(ctx, q, h, asin)
		if err != nil {
			if !errors.Is(err, ErrNotFound) && outage == nil {
				outage = err
				q.NoDetailsBelow = 0
			}
			continue
		}
		cands := make([]MatchCandidate, 0, len(hits)-i)
		cands = append(cands, *best)
		for _, rest := range hits[i+1:] {
			cands = append(cands, rest.stub())
		}
		return cands, nil
	}
	if outage != nil {
		return nil, outage
	}
	return []MatchCandidate{}, nil
}

// bestCandidate is the best hit with its details, as works/match inlined them, else
// fetched; a stub when the caller would drop it anyway (MatchQuery.NoDetailsBelow). A
// series with no facts is unknown and drops nothing, as a failed read does.
func (s *Service) bestCandidate(ctx context.Context, q MatchQuery, h matchHit, asin string) (*MatchCandidate, error) {
	if h.score < q.NoDetailsBelow && h.reasons.Identifier == "" {
		stub := h.stub()
		return &stub, nil
	}
	if h.work != nil && h.work.ID != "" {
		return s.hitCandidate(q, h, h.work, s.inlinedFacts(h.work, h.facts), asin), nil
	}
	detail, err := s.fetchWork(ctx, h.id)
	if err != nil {
		return nil, err
	}
	var facts map[string]*seriesFacts
	if !q.NoSeries {
		facts = s.seriesFactsFor(ctx, []*upstreamWorkDetail{detail})
	}
	return s.hitCandidate(q, h, detail, facts, asin), nil
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
		pos := strconv.FormatFloat(q.SeriesIndex, 'f', -1, 64)
		if metadata.ValidSeriesRange(q.SeriesIndex, q.SeriesIndexEnd) {
			pos += "-" + strconv.FormatFloat(q.SeriesIndexEnd, 'f', -1, 64)
		}
		v.Set("position", pos)
	}
	if q.Duration >= 1 {
		v.Set("runtime", strconv.Itoa(int(math.Round(q.Duration))))
	}
	add("language", q.Language)
	for _, n := range q.Narrators[:min(len(q.Narrators), maxMatchNarrators)] {
		// A narrator credit naming the author is mostly a composer tag (the
		// narrator's fallback, metadata.Extract) a tagger filled with the author:
		// it says nothing of the recording, and would count against every work
		// whose recordings a real narrator read.
		if authorSimilarity(q.Author, n) != 1 {
			add("narrator", n)
		}
	}
	v.Set("limit", strconv.Itoa(q.limit()))
	if q.BestOnly {
		v.Set("expand", "work")
	}
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

// toCandidate builds a candidate from its work document. Its series are the ones
// a book of the work is in whatever its recording (seriesrules.go, with the
// series' facts): never a variant reading order (it repeats a series at another
// position), a translation or another language's series, nor an edition; each
// recording carries its own, an edition named for it first. foundBy is the ASIN
// that found the work ("" for none): where a recording sells under several (a
// book the store splits in two, or lists twice), it leads its marketplace's, so
// the book keeps the ASIN its tags gave when that store is the one preferred.
func (s *Service) toCandidate(d *upstreamWorkDetail, facts map[string]*seriesFacts, cover, region, foundBy string) *MatchCandidate {
	series := newWorkSeries(d, facts)
	c := &MatchCandidate{
		WorkID: d.ID, Title: d.Title, Subtitle: d.Subtitle, Authors: toPersonRefs(d.Authors),
		Language: d.Language, FirstPublished: d.FirstPublished, Description: d.Description,
		Series: series.of(""), CoverURL: cover, WebURL: s.workURL(d.ID), Recordings: []MatchRecording{},
	}
	rank := func(a ASINRef) int {
		if a.ASIN == foundBy {
			return 0
		}
		return 1
	}
	for _, r := range d.Recordings {
		mr := MatchRecording{
			ID: r.ID, Narrators: toPersonRefs(r.Narrators), Abridged: r.Abridged, RuntimeMin: r.RuntimeMin,
			ReleaseDate: r.ReleaseDate, Publisher: r.Publisher, ASINRefs: []ASINRef{}, ISBNs: []string{}, CoverURL: r.CoverURL,
			Series: series.of(r.ID),
		}
		for _, a := range r.ASIN {
			if ref := ASINRef(a); a.ASIN != "" && !slices.Contains(mr.ASINRefs, ref) {
				mr.ASINRefs = append(mr.ASINRefs, ref)
			}
		}
		slices.SortStableFunc(mr.ASINRefs, func(a, b ASINRef) int { return rank(a) - rank(b) })
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

// PlausibleRuntimeOff is how far a recording's runtime may be from a book's length,
// as a fraction of the book's, and still plausibly be its edition (RuntimeOff).
const PlausibleRuntimeOff = 0.10

// NarratedRuntimeOff is how far a recording that one of the book's narrators reads
// may be from the book's length and still be its edition: the narrator says it is
// the same production, timed differently (a store's runtime with or without credits
// and samples, a rip that drops the intro). Further off, it is that narrator's other
// edition.
const NarratedRuntimeOff = 0.20

// RuntimeOff is how far a recording of runtimeMin minutes is from a book of seconds,
// as a fraction of the book's length; ok is false when either is unknown.
func RuntimeOff(runtimeMin int, seconds float64) (off float64, ok bool) {
	if runtimeMin <= 0 || seconds <= 0 {
		return 0, false
	}
	return math.Abs(float64(runtimeMin)*60-seconds) / seconds, true
}

// runtimeFit scores the closest recording's runtime against the book's duration:
// within 3% is the same edition, within PlausibleRuntimeOff plausibly so; ok is
// false when no recording states a runtime.
func runtimeFit(duration float64, recs []MatchRecording) (float64, bool) {
	best, ok := 0.0, false
	for _, r := range recs {
		off, known := RuntimeOff(r.RuntimeMin, duration)
		if !known {
			continue
		}
		ok = true
		switch {
		case off <= 0.03:
			best = 1
		case off <= PlausibleRuntimeOff:
			best = math.Max(best, 0.5)
		}
	}
	return best, ok
}

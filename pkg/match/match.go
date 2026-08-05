// Package match identifies whether two book records refer to the same audiobook,
// across the wildly inconsistent ways titles are stored. It is the shared, reusable
// matcher used by the audiosilo-manager (to map an Audible library onto a server's
// books) and available to the server itself (enrichment, dedup, smarter lookup).
//
// Real libraries rarely carry an ASIN or a series index on every book, and titles
// appear every which way: "Diary of a Wimpy Kid: The Ugly Truth (Book 5)",
// "DWK05 - The Ugly Truth", "Return of the Runebound Professor 3",
// "Silverspear: Rise to Omniscience, Book 6", or a clean "Legion's Fifth Vault".
// Exact title equality therefore matches almost nothing. Best instead scores each
// same-author candidate on the overlap of its significant title tokens (with the
// series name and "(Book N)" fluff stripped), boosted when the series and the
// sequence agree - deriving the sequence from the title when it isn't a column.
// When both titles are nothing but "series + number", the number is the whole
// identity: a conflicting sequence disqualifies the candidate outright (titles
// with real words are exempt - sub-series numbering makes derived numbers noisy).
// Tuned against a real 1080-book / 720-book pairing (~4% → ~88% matched, no false
// positives in audit).
package match

import (
	"regexp"
	"strconv"
	"strings"
)

// Book is a candidate to match against (a server book, or any catalog entry).
type Book struct {
	Title       string
	Author      string
	Series      string
	SeriesIndex float64
	ASIN        string
}

// Query is the item being matched (an Audible record, or any import source item).
type Query struct {
	Title       string
	TitleShort  string
	Author      string
	Series      string
	Sequence    float64
	HasSequence bool
	ASIN        string
}

// threshold is the minimum score Best accepts. A same-series title-subset scores
// 1.5; a same-series strong token overlap or an author + full-title subset scores
// 1.0; weaker overlaps fall below. A same-series candidate whose bare title
// conflicts with the query on sequence never scores at all (skipped outright).
const threshold = 1.0

// Best returns the index of the best-matching book and whether it cleared the
// confidence threshold (-1, false when none match).
func Best(books []Book, q Query) (int, bool) {
	// 1. ASIN - exact, highest confidence (rare in practice, but decisive).
	for i := range books {
		if q.ASIN != "" && books[i].ASIN != "" && strings.EqualFold(q.ASIN, books[i].ASIN) {
			return i, true
		}
	}
	qSeries := NormalizeSeries(q.Series)
	qToks := titleTokens(q.Title, q.Series)
	if len(qToks) == 0 {
		qToks = titleTokens(q.TitleShort, q.Series)
	}
	qSeq, qHasSeq := querySeq(q)
	qBare := bareTitle(q.Title, q.Series)
	// The number a bare query title is compared on: its own explicit marker when
	// present (like-for-like with the candidate side), else the sequence column.
	qBareSeq, qBareSeqOK := bareSeq(q.Title, q.Series)
	if !qBareSeqOK {
		qBareSeq, qBareSeqOK = qSeq, qHasSeq
	}

	best, bestScore := -1, 0.0
	for i := range books {
		b := books[i]
		if !authorMatch(b.Author, q.Author) {
			continue
		}
		sameSeries := qSeries != "" && NormalizeSeries(b.Series) == qSeries
		// When BOTH titles reduce to the bare series name ("Series: Volume N"),
		// their full token overlap is the series matching itself, not title
		// evidence - the volume number is the only identity left. An explicit
		// marker number that disagrees means a sibling, not this book;
		// disqualify it so a not-yet-imported volume can't "match" an indexed
		// one. Titles with real residual words are exempt: sub-series/part
		// numbering makes derived numbers unreliable there ("Threads of
		// Destiny: Volume 5" is Destiny Cycle #8), and matching words outrank a
		// noisy number. Only bareSeq is trusted to veto - SeriesIndex can carry
		// junk parsed from a folder shortcode ("1PL02 - …" yields 1) and
		// SeqFromTitle grabs incidental numbers (a "(2015)" year), either of
		// which would veto true matches.
		if sameSeries && qBare && qBareSeqOK && bareTitle(b.Title, b.Series) {
			if bSeq, ok := bareSeq(b.Title, b.Series); ok && !seqEqual(bSeq, qBareSeq) {
				continue
			}
		}
		score := tokenScore(qToks, titleTokens(b.Title, b.Series))
		if sameSeries {
			score += 0.5 // series agreement is a strong confirmation
			// Same series + same sequence is near-certain. Many libraries leave the
			// series index at 0 and put the number in the title ("… 3", "(Book 5)"),
			// so derive it from the title when the field is empty - this is what
			// matches progression-fantasy books whose title is just "Series Name N"
			// (the residual tokens are only the number, which is dropped).
			if bSeq, ok := bookSeq(b); ok && qHasSeq && seqEqual(bSeq, qSeq) {
				score += 1.5
			}
		}
		if score > bestScore {
			bestScore, best = score, i
		}
	}
	if bestScore >= threshold {
		return best, true
	}
	return -1, false
}

// matchNoise strips parenthetical/bracketed groups and "book N"/"vol N" run tokens -
// the formatting that differs between sources' titles.
var matchNoise = regexp.MustCompile(`(?i)[\(\[][^\)\]]*[\)\]]|\b(?:book|bk|vol|volume|part|pt|episode|ep|#)\s*\.?\s*\d+(?:\.\d+)?\b`)

// fluffWords are standalone words that are noise in a book title (not "book"/"novel",
// which appear in real titles like "The Book Thief").
var fluffWords = regexp.MustCompile(`(?i)\b(?:series|unabridged|abridged|audiobook|dramatized|adaptation)\b`)

// CleanTitle returns a display title with the series name and edition/"Book N" fluff
// removed: "Diary of a Wimpy Kid: The Ugly Truth (Book 5)" + series "Diary of a
// Wimpy Kid" → "The Ugly Truth". Useful both to name a file and (tokenized) to
// match. Falls back to the original (minus parentheticals) if cleaning empties it.
func CleanTitle(title, series string) string {
	t := residualTitle(title, series)
	if t == "" {
		t = tidyTitle(matchNoise.ReplaceAllString(title, " "))
	}
	if t == "" {
		t = strings.TrimSpace(title)
	}
	return t
}

// residualTitle is the title with the series name and numbering/edition fluff
// removed and NO fallback - empty when the title carries nothing of its own
// ("Unintended Cultivator: Volume 9" with that series is pure series + number).
func residualTitle(title, series string) string {
	t := title
	if s := strings.TrimSpace(series); s != "" {
		t = removeFold(t, s)
	}
	t = dropGenreSubtitle(t)
	t = matchNoise.ReplaceAllString(t, " ")
	t = fluffWords.ReplaceAllString(t, " ")
	return tidyTitle(t)
}

// bareTitle reports whether a title reduces to nothing beyond its series name and
// numbering - the shape whose CleanTitle fallback reintroduces the series words,
// making token overlap between two such titles meaningless as title evidence.
func bareTitle(title, series string) bool {
	return len(tokenize(residualTitle(title, series))) == 0
}

// markerSeq matches an explicit sequence marker ("Volume 9", "Book 3", "(Book 5)" -
// the marker half of matchNoise) and captures its number.
var markerSeq = regexp.MustCompile(`(?i)\b(?:book|bk|vol|volume|part|pt|episode|ep|#)\s*\.?\s*(\d+(?:\.\d+)?)\b`)

// bareSeq derives the volume number of a bare "series + number" title from its
// explicit marker ("… Volume 9") or, failing that, from a residual that is nothing
// but the number ("Series Name 3"). Unlike SeqFromTitle it never grabs an
// incidental number (a "(2015)" year, a digit-named series like "86"), and unlike
// the SeriesIndex column it can't carry junk parsed from a folder shortcode -
// which makes it the only number trustworthy enough to disqualify a match on.
func bareSeq(title, series string) (float64, bool) {
	t := title
	if s := strings.TrimSpace(series); s != "" {
		t = removeFold(t, s)
	}
	if m := markerSeq.FindStringSubmatch(t); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			return v, true
		}
	}
	if s := tidyTitle(t); isAllDigits(s) {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

// SeqFromTitle extracts a series sequence from a title by stripping the series name
// and taking the first remaining number - the sequence in titles like "… 3",
// "(Book 5)", "5: A …". Reports false when there is none.
func SeqFromTitle(title, series string) (float64, bool) {
	t := title
	if strings.TrimSpace(series) != "" {
		t = removeFold(t, series)
	}
	m := firstNumber.FindStringSubmatch(t)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

var firstNumber = regexp.MustCompile(`\b(\d+(?:\.\d+)?)\b`)

func bookSeq(b Book) (float64, bool) {
	if b.SeriesIndex > 0 {
		return b.SeriesIndex, true
	}
	return SeqFromTitle(b.Title, b.Series)
}

func querySeq(q Query) (float64, bool) {
	if q.HasSequence {
		return q.Sequence, true
	}
	return SeqFromTitle(q.Title, q.Series)
}

// tidyTitle collapses whitespace and trims stray leading/trailing separators left by
// removing the series/fluff (": ", " - ", ", ").
func tidyTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.Trim(s, " -:,;|.")
}

// removeFold removes every case-insensitive occurrence of sub from s, replacing
// each with a space. It searches a lowercased copy but maps each match's byte
// offsets back onto s rune-by-rune, so a rune whose lowercase form differs in byte
// length (e.g. 'İ' → "i", 'ẞ' → "ß") can't desync the offsets and corrupt the
// result - the naive `s[:i] + s[i+len(sub):]` did, because i indexes the lowercased
// string, not s.
func removeFold(s, sub string) string {
	lsub := strings.ToLower(sub)
	if lsub == "" {
		return s
	}
	var b strings.Builder
	for s != "" {
		i := strings.Index(strings.ToLower(s), lsub)
		if i < 0 {
			b.WriteString(s)
			break
		}
		// Map the lowercased match span [i, i+len(lsub)) onto byte offsets in s by
		// lowering s one rune at a time until the lowered length reaches each bound.
		start, end, lowered := -1, -1, 0
		for off, r := range s {
			if lowered == i {
				start = off
			}
			lowered += len(strings.ToLower(string(r)))
			if lowered == i+len(lsub) {
				end = off + len(string(r))
				break
			}
		}
		if start < 0 || end < 0 {
			// Match straddles a rune whose case expansion has no clean byte boundary;
			// leave the remainder untouched rather than corrupt it.
			b.WriteString(s)
			break
		}
		b.WriteString(s[:start])
		b.WriteByte(' ')
		s = s[end:]
	}
	return b.String()
}

// genreFluff marks a trailing ": …"/" - …" subtitle made up entirely of these words
// as boilerplate to drop - "Dungeon Crawler Carl: A LitRPG/Gamelit Adventure" →
// "Dungeon Crawler Carl".
var genreFluff = map[string]bool{
	"litrpg": true, "gamelit": true, "gamlit": true, "progression": true,
	"fantasy": true, "epic": true, "novel": true, "novella": true, "saga": true,
	"adventure": true, "story": true, "tale": true, "tales": true, "cozy": true,
	"mystery": true, "romance": true, "thriller": true, "audible": true,
	"original": true, "dramatized": true, "adaptation": true, "omnibus": true,
	"complete": true, "collection": true, "edition": true, "anthology": true,
}

// dropGenreSubtitle removes a trailing ": …" or " - …" segment whose significant
// words are all genre/format fluff. It only fires when the whole tail is fluff, so a
// real subtitle ("An Unexpected Journey", "Seventh Bridge to the Heavens") is kept.
func dropGenreSubtitle(s string) string {
	sep := strings.LastIndex(s, ": ")
	if d := strings.LastIndex(s, " - "); d > sep {
		sep = d
	}
	if sep < 0 {
		return s
	}
	any, allFluff := false, true
	for _, w := range strings.FieldsFunc(strings.ToLower(s[sep:]), notAlnum) {
		if len(w) < 2 || titleStopwords[w] {
			continue
		}
		any = true
		if !genreFluff[w] {
			allFluff = false
			break
		}
	}
	if any && allFluff {
		return strings.TrimSpace(s[:sep])
	}
	return s
}

var titleStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "and": true, "to": true,
	"in": true, "on": true, "for": true, "with": true, "at": true, "by": true,
	"or": true, "nor": true, "from": true,
}

// titleTokens reduces a title to its set of significant lowercase word tokens after
// CleanTitle has removed the series name and fluff.
func titleTokens(title, series string) map[string]struct{} {
	return tokenize(CleanTitle(title, series))
}

// tokenize splits a cleaned title into its significant lowercase word tokens.
// Stopwords, single characters, and pure numbers are dropped so the distinctive
// book words drive the match.
func tokenize(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), notAlnum) {
		if len(w) < 2 || titleStopwords[w] || isAllDigits(w) {
			continue
		}
		out[w] = struct{}{}
	}
	return out
}

func notAlnum(r rune) bool {
	return (r < 'a' || r > 'z') && (r < '0' || r > '9')
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// tokenScore measures title-token overlap: 1.0 when one set is wholly contained in
// the other (a dropped subtitle or an added shortcode token), else the Jaccard
// similarity. 0 when there is no shared distinctive token.
func tokenScore(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if _, ok := b[w]; ok {
			inter++
		}
	}
	if inter == 0 {
		return 0
	}
	if inter == len(a) || inter == len(b) {
		return 1.0
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// authorMatch is tolerant of multi-author / "and"-joined credits: a normalized
// equality, or one author string contained in the other.
func authorMatch(a, b string) bool {
	na, nb := Normalize(a), Normalize(b)
	if na == "" || nb == "" {
		return false
	}
	return na == nb || strings.Contains(na, nb) || strings.Contains(nb, na)
}

func seqEqual(a, b float64) bool { return a-b > -0.001 && a-b < 0.001 }

// NormalizeSeries normalizes a series name for tolerant matching, dropping a leading
// article so "The Primal Hunter" and "Primal Hunter" compare equal. Exported so the
// manager identifies series identically to the server (shared matcher).
func NormalizeSeries(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	for _, art := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(t, art) {
			t = t[len(art):]
			break
		}
	}
	return Normalize(t)
}

// Normalize lowercases and strips non-alphanumeric runes for tolerant matching (so
// "L. A. McBride" and "L.A. McBride" compare equal). Exported so the manager
// normalizes authors/titles identically to the server (shared matcher).
func Normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

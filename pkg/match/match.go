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
// When a title is nothing but "series + number" - or just the bare series name,
// as Audible lists every volume of some series - the number is the whole
// identity: token overlap is the series matching itself, so such a title matches
// only on an agreeing volume number, and two EXPLICIT numbers that disagree
// disqualify the candidate outright. A title carrying real words never supplies
// that explicit number: its "Volume 5"/"Part 1" marker is sub-series or part
// numbering, so it may only ever confirm, and only from its column. A query
// carrying no series column at all is judged under a candidate's series only when
// its title spells that series out - the containment is the evidence linking the
// two.
// Tuned against a real 1080-book / 720-book pairing (~4% → ~88% matched, no false
// positives in audit).
package match

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Book is a candidate to match against (a server book, or any catalog entry).
// Narrators is optional and participates in the person gate (see personMatch).
type Book struct {
	Title       string
	Author      string
	Series      string
	SeriesIndex float64
	ASIN        string
	Narrators   []string
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
	Narrators   []string
}

// threshold is the minimum score Best accepts, over this ladder.
//
// A series-linked pair with a BARE side ("Series: Volume N", or just the series
// name) is scored on its number alone and never token-scored: scoreBareSeqAgree
// when the two sides' numbers agree, scoreBareVol1 when BOTH are bare with no
// usable number (a standalone / first volume shape), nothing otherwise - and two
// explicit numbers that disagree skip the candidate outright, never scoring.
//
// Every other candidate scores its token overlap (1.0 when one title's tokens are
// wholly contained in the other's, else the Jaccard similarity) plus, when the
// series agrees, scoreSeriesAgree, scoreSeqAgree for an agreeing sequence, and
// scoreFullTitle when the candidate's title is covered by the query's (titleCovers).
// So a same-series full title match reaches 2.5 and outranks a sequence agreement
// alone (2.0), which is the point: positions get renumbered far more often than
// books get retitled.
const threshold = 1.0

// The score ladder. Kept together so the relative ordering above is legible in one
// place rather than as magic numbers at each site.
const (
	scoreSeriesAgree  = 0.5 // the candidate is in the query's series
	scoreSeqAgree     = 1.5 // …and on the same sequence number
	scoreFullTitle    = 1.0 // …and one title's tokens contain the other's
	scoreBareSeqAgree = 2.0 // bare titles, agreeing volume number - near-certain
	scoreBareVol1     = 1.5 // bare titles, no usable number - a volume-1 shape
)

// qNums is the query side's view of the bare branch under one series reference.
// veto is the number the title itself spells out and is only ever usable when the
// title is BARE; conf is the number the pair may be confirmed on - for a wordy
// title that is its sequence column alone.
type qNums struct {
	bare   bool
	veto   float64
	vetoOK bool
	conf   float64
	confOK bool
}

// candLink is the memoized derivation for one candidate SERIES against a
// series-less query: whether a spelling of it occurs in the query title, and the
// query's numbers under that spelling.
type candLink struct {
	linked bool
	nums   qNums
}

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
	qTitleLower := strings.ToLower(q.Title)
	numsUnder := func(seriesRef string) qNums {
		n := qNums{bare: bareTitle(q.Title, seriesRef)}
		n.veto, n.vetoOK = bareSeq(q.Title, seriesRef)
		if n.bare {
			// A bare title's own number, else the sequence column it was scanned with.
			n.conf, n.confOK = n.veto, n.vetoOK
			if !n.confOK {
				n.conf, n.confOK = qSeq, qHasSeq
			}
			return n
		}
		// A wordy title states no position: its marker is sub-series or part
		// numbering, so it neither vetoes nor confirms - only the column does.
		n.vetoOK = false
		n.conf, n.confOK = q.Sequence, q.HasSequence
		return n
	}
	ownNums := numsUnder(q.Series)
	// Candidate lists repeat a series across its volumes, and this derivation
	// depends only on (q.Title, b.Series), so it is memoized per Best call.
	linkMemo := map[string]candLink{}

	best, bestScore := -1, 0.0
	for i := range books {
		b := books[i]
		if !personMatch(b, q) {
			continue
		}
		bSeriesTrim := strings.TrimSpace(b.Series)
		sameSeries := qSeries != "" && NormalizeSeries(b.Series) == qSeries
		// A query with NO series column is judged under the candidate's series when
		// its title spells that series out - the shape a title-only library scan
		// takes against catalog cards, which nearly all carry a series. Without the
		// link the pair never reached the bare branch, so both halves of it were
		// dead: the number agreement that identifies the book ("Chaos Seeds 1" vs
		// the #1 card titled "The Land: Founding") and, worse, the sibling veto
		// ("The Path of Ascension 4" vs the #10.5 card, whose empty residual makes
		// CleanTitle fall back to the series-laden title and score a full subset).
		qn, linkedByCandSeries := ownNums, false
		if qSeries == "" && bSeriesTrim != "" {
			link, seen := linkMemo[b.Series]
			if !seen {
				if ref, found := seriesRefIn(qTitleLower, b.Series); found {
					link = candLink{linked: true, nums: numsUnder(ref)}
				}
				linkMemo[b.Series] = link
			}
			if link.linked {
				qn, linkedByCandSeries = link.nums, true
			}
		}
		// The series string the candidate's bareness is judged against: its own,
		// or the query's when it has none - a server book scanned without a
		// series column often carries the series name AS its title. stripSeries
		// knows every spelling of it, so a candidate links under the same tolerance
		// the query side got (a paren-decorated or article-carrying series column
		// against a title that spells neither).
		bSeriesRef := b.Series
		if bSeriesTrim == "" {
			bSeriesRef = q.Series
		}
		// seriesLinked: the candidate is tied to a shared series - by its series
		// column, by the series-less query naming the candidate's series above, or
		// by being series-less with the series name as its title. bBare is computed
		// only where it is then read: those tests, and the branch below.
		seriesLinked := sameSeries || linkedByCandSeries
		bBare := false
		switch {
		case seriesLinked:
			bBare = bareTitle(b.Title, bSeriesRef)
		case bSeriesTrim == "" && qSeries != "":
			// The series must actually OCCUR in that title. Reducing to nothing is
			// not enough on its own: a card titled "Unabridged" also reduces to
			// nothing, and would then claim every numberless volume of the series
			// by the same author.
			if _, ok := seriesRefIn(strings.ToLower(b.Title), q.Series); ok {
				bBare = bareTitle(b.Title, bSeriesRef)
				seriesLinked = bBare
			}
		}
		score := 0.0
		if seriesLinked && (qn.bare || bBare) {
			// A bare title ("Series: Volume N", or just the series name - Audible
			// lists whole series that way) carries no title evidence: token overlap
			// is the series matching itself, and the volume number is the only
			// identity left. A pair of wordy titles never enters here; when only one
			// side is bare, the wordy side contributes its COLUMN number and nothing
			// else, because a marker inside a real title is sub-series or part
			// numbering ("Threads of Destiny: Volume 5" is Destiny Cycle #8, and
			// "Forged in Blood: Part 1" is The Emperor's Edge #6).
			bVetoSeq, bVetoOK := 0.0, false
			bConf, bConfOK := b.SeriesIndex, b.SeriesIndex > 0
			if bBare {
				bVetoSeq, bVetoOK = bareSeq(b.Title, bSeriesRef)
				bConf, bConfOK = bVetoSeq, bVetoOK
				if !bConfOK {
					bConf, bConfOK = bookSeqRef(b, bSeriesRef)
				}
			}
			// Only two bare sides stating their OWN explicit numbers may disagree
			// hard enough to mean "a sibling, not this book" (see bookSeqRef for why
			// no other number is safe to veto on).
			if qn.vetoOK && bVetoOK && !seqEqual(bVetoSeq, qn.veto) {
				continue
			}
			switch {
			case qn.confOK && bConfOK && seqEqual(bConf, qn.conf):
				// Series + agreeing number is the whole identity - near-certain.
				score = scoreBareSeqAgree
			case qn.bare && bBare && (!qn.confOK || seqEqual(qn.conf, 1)):
				// Both titles are just the series name and the candidate's title
				// states no number: that shape is a standalone / first volume, so
				// volume 1 (or a numberless query) may claim it. A query volume
				// ≥ 2 must NOT fall back here - one numberless bare-titled server
				// book would absorb every later volume of its series. The candidate
				// may still carry a SeriesIndex; it is deliberately NOT consulted,
				// being the junk-prone column ("Some Series (Unabridged)" indexed 3
				// is still claimable by volume 1 - pinned in TestBestSequenceConflict).
				score = scoreBareVol1
			}
		} else {
			bToks := titleTokens(b.Title, b.Series)
			score = tokenScore(qToks, bToks)
			if sameSeries {
				score += scoreSeriesAgree
				// Same series + same sequence is near-certain. Many libraries leave
				// the series index at 0 and put the number in the title ("… 3",
				// "(Book 5)"), so derive it from the title when the field is empty -
				// this is what matches progression-fantasy books whose title is just
				// "Series Name N" (the residual tokens are only the number, which is
				// dropped).
				if bSeq, ok := bookSeqRef(b, b.Series); ok && qHasSeq && seqEqual(bSeq, qSeq) {
					score += scoreSeqAgree
				}
				// A covered title inside the same series outranks a bare sequence
				// agreement, because either side's number can be the junk bookSeqRef
				// documents - here a local tag numbering a whole franchise rather than
				// the series ("Halo: Primordium" tagged 9, the catalog's #8). Catalogs
				// renumber positions far more often than a book is retitled. A
				// candidate agreeing on BOTH still ranks highest.
				if titleCovers(qToks, bToks) {
					score += scoreFullTitle
				}
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

// bracketGroup matches one parenthetical/bracketed group. Defined once and shared
// by matchNoise and parenGroup so the bracket rule has a single definition.
const bracketGroup = `[\(\[][^\)\]]*[\)\]]`

// matchNoise strips parenthetical/bracketed groups and "book N"/"vol N" run tokens -
// the formatting that differs between sources' titles.
var matchNoise = regexp.MustCompile(`(?i)` + bracketGroup + `|\b(?:book|bk|vol|volume|part|pt|episode|ep|#)\s*\.?\s*\d+(?:\.\d+)?\b`)

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
	t := stripSeries(title, series)
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

// seriesForms enumerates the spellings one series name appears in, most specific
// first: as given, without a parenthetical ordering note ("Vorkosigan Saga
// (chronological)"), without the catalog's trailing " Series" decoration
// (Audible's "Dragon Heart Series" vs a title that says "Dragon Heart"), without a
// leading article, and the combinations of those. ONE list, used both to FIND a
// series name in a title (seriesRefIn) and to REMOVE it (stripSeries), so the two
// can never disagree about what the series is called - a form that links a pair
// is by construction a form that strips.
func seriesForms(series string) []string {
	base := strings.TrimSpace(series)
	if base == "" {
		return nil
	}
	var out []string
	add := func(s string) {
		if s = tidyTitle(s); s == "" {
			return
		}
		for _, prev := range out {
			if strings.EqualFold(prev, s) {
				return
			}
		}
		out = append(out, s)
	}
	for _, s := range [2]string{base, stripParenGroups(base)} {
		noSuffix := dropSeriesSuffix(s)
		add(s)
		add(noSuffix)
		add(dropLeadingArticle(s))
		add(dropLeadingArticle(noSuffix))
	}
	return out
}

// seriesRefIn reports which spelling of series occurs in lowerTitle (an
// already-lowercased title). Containment is the whole safeguard when a series-LESS
// query is judged under a candidate's series: only a query title that spells the
// series out is linked to it. Judging every series-carrying candidate as a sibling
// of every query would put a 2.0 on a bare number agreement between unrelated
// books.
func seriesRefIn(lowerTitle, series string) (string, bool) {
	for _, form := range seriesForms(series) {
		if containsPhraseLower(lowerTitle, form) {
			return form, true
		}
	}
	return "", false
}

// parenGroup matches a parenthetical/bracketed decoration on a series name:
// "Vorkosigan Saga (chronological)" is the catalog's ordering note, not part of
// the name a library title carries.
var parenGroup = regexp.MustCompile(bracketGroup)

func stripParenGroups(s string) string {
	if !strings.ContainsAny(s, "([") {
		return s // the common case, kept off the regexp
	}
	return parenGroup.ReplaceAllString(s, " ")
}

// dropLeadingArticle removes a leading "the "/"a "/"an ", never emptying the
// string - an article-only name keeps its one word.
func dropLeadingArticle(s string) string {
	t := strings.TrimSpace(s)
	for _, art := range []string{"the ", "a ", "an "} {
		if len(t) > len(art) && strings.EqualFold(t[:len(art)], art) {
			return t[len(art):]
		}
	}
	return t
}

// dropSeriesSuffix removes a trailing " Series", never emptying the string, so a
// series genuinely named "Series" survives (pinned in TestNormalizeSeries).
func dropSeriesSuffix(s string) string {
	const suf = " series"
	t := strings.TrimSpace(s)
	if len(t) > len(suf) && strings.EqualFold(t[len(t)-len(suf):], suf) {
		return strings.TrimSpace(t[:len(t)-len(suf)])
	}
	return t
}

// containsPhraseLower reports whether the already-lowercased ls contains sub
// case-insensitively at alphanumeric boundaries. The boundaries matter: a short
// series name ("Land") must not link itself to an unrelated title that merely
// embeds it ("The Landlord's Daughter").
func containsPhraseLower(ls, sub string) bool {
	// Offsets index the lowered copies only, and lowering preserves whether a rune
	// is alphanumeric, so the boundary test needs no mapping back onto the original.
	lsub := strings.ToLower(sub)
	if lsub == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(ls[from:], lsub)
		if i < 0 {
			return false
		}
		i += from
		if boundedAt(ls, i, i+len(lsub)) {
			return true
		}
		from = i + 1
	}
}

// boundedAt reports whether s[start:end] sits on alphanumeric boundaries.
// Decoding an empty edge slice yields RuneError, which is neither letter nor
// digit, so the ends of the string need no special case.
func boundedAt(s string, start, end int) bool {
	before, _ := utf8.DecodeLastRuneInString(s[:start])
	after, _ := utf8.DecodeRuneInString(s[end:])
	return !isAlnumRune(before) && !isAlnumRune(after)
}

// isAlnumRune is deliberately Unicode-aware, unlike the ASCII-only notAlnum: it
// classifies the raw runes of real titles and series names, where notAlnum is the
// splitter that reduces a title to ASCII word tokens. Do not merge them.
func isAlnumRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// markerSeq matches an explicit sequence marker ("Volume 9", "Book 3", "(Book 5)" -
// the marker half of matchNoise) and captures its number.
var markerSeq = regexp.MustCompile(`(?i)\b(?:book|bk|vol|volume|part|pt|episode|ep|#)\s*\.?\s*(\d+(?:\.\d+)?)\b`)

// bareSeq derives the volume number of a bare "series + number" title from its
// explicit marker ("… Volume 9") or, failing that, from a residual that is nothing
// but the number ("Series Name 3"). It states a number the title itself spells
// out, so it never grabs an incidental one (a "(2015)" year, a digit-named series
// like "86") - which is what makes it, and only it, safe to veto a match on (see
// bookSeqRef for the untrustworthy alternatives).
func bareSeq(title, series string) (float64, bool) {
	t := stripSeries(title, series)
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

// stripSeries removes every spelling of the series name (seriesForms) from a
// title. Removal is WORD-BOUNDARY-aware: series "Land Series" contributes the form
// "Land", and an unbounded removal turned the unrelated title "The Landlord" into
// "The lord", destroying its exact-title match.
func stripSeries(title, series string) string {
	t := title
	for _, form := range seriesForms(series) {
		t = removeFoldBounded(t, form)
	}
	return t
}

// SeqFromTitle extracts a series sequence from a title by stripping the series name
// and taking the first remaining number - the sequence in titles like "… 3",
// "(Book 5)", "5: A …". Reports false when there is none.
func SeqFromTitle(title, series string) (float64, bool) {
	m := firstNumber.FindStringSubmatch(stripSeries(title, series))
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

// bookSeqRef is a candidate's volume number: its SeriesIndex column, else the
// number derived from its title against an explicit series reference, so a
// series-less candidate can still be judged under the query's series ("Dragon
// Heart 3" yields its number). CONFIRM-GRADE ONLY - never veto on it. Both
// sources carry junk in real libraries: SeriesIndex is parsed from folder
// shortcodes ("1PL02 - …" yields 1) or numbers a franchise rather than the
// series, and SeqFromTitle grabs incidental numbers (a "(2015)" year). bareSeq
// is the only number trustworthy enough to disqualify a match.
func bookSeqRef(b Book, seriesRef string) (float64, bool) {
	if b.SeriesIndex > 0 {
		return b.SeriesIndex, true
	}
	return SeqFromTitle(b.Title, seriesRef)
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
// each with a space. Series names go through removeFoldBounded instead.
func removeFold(s, sub string) string { return removeFoldWith(s, sub, false) }

// removeFoldBounded is removeFold restricted to occurrences on alphanumeric
// boundaries, so a series form can never be cut out of the middle of a word.
func removeFoldBounded(s, sub string) string { return removeFoldWith(s, sub, true) }

// removeFoldWith searches a lowercased copy but maps each match's byte offsets
// back onto s rune-by-rune, so a rune whose lowercase form differs in byte length
// (e.g. 'İ' → "i", 'ẞ' → "ß") can't desync the offsets and corrupt the result -
// the naive `s[:i] + s[i+len(sub):]` did, because i indexes the lowercased string,
// not s.
func removeFoldWith(s, sub string, bounded bool) string {
	lsub := strings.ToLower(sub)
	if lsub == "" {
		return s
	}
	var b strings.Builder
	// The rune already written, so a boundary test at the start of the REMAINING
	// text still sees what preceded it in the original ("aLandLand" must reject
	// both occurrences of "Land", not just the first).
	prev := utf8.RuneError
	for s != "" {
		ls := strings.ToLower(s)
		i := strings.Index(ls, lsub)
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
		keep := false
		if bounded {
			before := prev
			if i > 0 {
				before, _ = utf8.DecodeLastRuneInString(ls[:i])
			}
			after, _ := utf8.DecodeRuneInString(ls[i+len(lsub):])
			keep = isAlnumRune(before) || isAlnumRune(after)
		}
		if keep {
			b.WriteString(s[:end])
			prev, _ = utf8.DecodeLastRuneInString(s[:end])
		} else {
			b.WriteString(s[:start])
			b.WriteByte(' ')
			prev = ' '
		}
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

// titleCovers reports whether the CANDIDATE's title tokens all appear in the
// query's, strongly enough to reward with scoreFullTitle. Direction matters: the
// query may carry extra words (a subtitle the catalog dropped), but a candidate
// with extra words is a different book - an omnibus "Halo: Primordium & Silentium"
// must not tie the true "Halo: Primordium" card. And a lone shared token is a
// coincidence, not a title match ("Winter" ⊆ "Winds of Winter"), so a single token
// only counts when the two titles reduce to exactly it.
func titleCovers(qToks, bToks map[string]struct{}) bool {
	if len(qToks) == 0 || len(bToks) == 0 {
		return false
	}
	for w := range bToks {
		if _, ok := qToks[w]; !ok {
			return false
		}
	}
	return len(bToks) >= 2 || len(qToks) == len(bToks)
}

// personMatch is the person gate. Real libraries routinely tag the NARRATOR as
// the author (David Tennant on Cressida Cowell's books) while catalog cards list
// author and narrators separately, so a credit on one side may legitimately be a
// narrator on the other. Narrator-to-narrator is deliberately not a link: two
// unrelated books share a prolific narrator constantly.
func personMatch(b Book, q Query) bool {
	if authorMatch(b.Author, q.Author) {
		return true
	}
	for _, n := range b.Narrators {
		if authorMatch(n, q.Author) {
			return true
		}
	}
	for _, n := range q.Narrators {
		if authorMatch(b.Author, n) {
			return true
		}
	}
	return false
}

// authorMatch is tolerant of multi-author / "and"-joined credits: a normalized
// equality, or one author string contained in the other. An empty name never
// matches.
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
	t := dropLeadingArticle(strings.ToLower(s))
	// A trailing " series" is catalog decoration, not part of the name: Audible
	// lists "Dragon Heart Series" where the server has "Dragon Heart".
	t = strings.TrimSuffix(t, " series")
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

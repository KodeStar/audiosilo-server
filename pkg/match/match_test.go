package match

import (
	"strings"
	"testing"
)

// bestCase is one Best expectation. Each test builds its own books + queries; only
// the assertion loop is shared.
type bestCase struct {
	name string
	q    Query
	want int // -1 = expect no match
}

func runBestCases(t *testing.T, books []Book, cases []bestCase) {
	t.Helper()
	for _, tc := range cases {
		got, ok := Best(books, tc.q)
		if tc.want == -1 {
			if ok {
				t.Errorf("%s: expected no match, got idx %d (%q)", tc.name, got, books[got].Title)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("%s: got idx %d ok=%v, want %d", tc.name, got, ok, tc.want)
		}
	}
}

// Cases drawn from a real 1080-book library: full Audible titles with series
// prefixes + "(Book N)", clean book titles, shortcode prefixes, series suffixes, and
// "Series Name N" with the number only. The server rarely has an ASIN or a series
// index.
func TestBest(t *testing.T) {
	books := []Book{
		0: {Author: "Jeff Kinney", Series: "Diary of a Wimpy Kid", Title: "Diary of a Wimpy Kid: The Ugly Truth (Book 5)"},
		1: {Author: "Jeff Kinney", Series: "Diary of a Wimpy Kid", Title: "Diary Of A Wimpy Kid 4 - Dog Days"},
		2: {Author: "Jeff Kinney", Series: "Diary of a Wimpy Kid", Title: "The Deep End - Diary of a Wimpy Kid Series, Book 15"},
		3: {Author: "Actus", Series: "Return of the Runebound Professor", Title: "Return of the Runebound Professor 3"},
		4: {Author: "Actus", Series: "Return of the Runebound Professor", Title: "Return of the Runebound Professor 5: A Progression Fantasy Epic"},
		5: {Author: "A. F. Kay", Series: "Divine Apostasy", Title: "Legion's Fifth Vault"},
		6: {Author: "Aaron Oster", Series: "Rise to Omniscience", Title: "RO07 - Sandqueen"},
		7: {Author: "Aaron Oster", Series: "Rise to Omniscience", Title: "Silverspear: Rise to Omniscience, Book 6"},
		8: {Author: "Will Wight", Series: "Cradle", Title: "Unsouled", SeriesIndex: 1, ASIN: "B01"},
	}
	q := func(title, author, series string, seq float64) Query {
		return Query{Title: title, TitleShort: title, Author: author, Series: series, Sequence: seq, HasSequence: true}
	}
	cases := []bestCase{
		{"full title + (Book N)", q("Diary of a Wimpy Kid: The Ugly Truth (Book 5)", "Jeff Kinney", "Diary of a Wimpy Kid", 5), 0},
		{"alt scheme N - title", q("Diary of a Wimpy Kid: Dog Days (Book 4)", "Jeff Kinney", "Diary of a Wimpy Kid", 4), 1},
		{"series suffix on server", q("Diary of a Wimpy Kid: The Deep End (Book 15)", "Jeff Kinney", "Diary of a Wimpy Kid", 15), 2},
		{"series-name-N residual is just a number", q("Return of the Runebound Professor 3", "Actus", "Return of the Runebound Professor", 3), 3},
		{"series-name-N picks the right number", q("Return of the Runebound Professor 5", "Actus", "Return of the Runebound Professor", 5), 4},
		{"clean server vs subtitled audible", q("Legion's Fifth Vault: A Fantasy LitRPG Adventure", "A. F. Kay", "Divine Apostasy", 5), 5},
		{"shortcode prefix on server", q("Sandqueen", "Aaron Oster", "Rise to Omniscience", 7), 6},
		{"series suffix on server 2", q("Silverspear", "Aaron Oster", "Rise to Omniscience", 6), 7},
		{"absent number does not false-match", q("Return of the Runebound Professor 9", "Actus", "Return of the Runebound Professor", 9), -1},
		{"different author no match", q("The Ugly Truth", "Someone Else", "Whatever", 5), -1},
	}
	runBestCases(t, books, cases)
	if got, ok := Best(books, Query{ASIN: "B01", Title: "anything"}); !ok || got != 8 {
		t.Errorf("ASIN match: got %d ok=%v, want 8", got, ok)
	}
}

// TestBestSequenceConflict pins the real-world false positive behind a wrong
// "already on server": digit-numbered series entries ("Unintended Cultivator:
// Volume 9" vs the indexed "… Volume 2") both reduce to the bare series tokens, so
// they tied on full token overlap and a not-yet-imported volume matched an indexed
// sibling. When BOTH titles are bare "series + number", a sequence conflict must
// disqualify - but ONLY then: titles with real residual words keep matching on
// words even when the derived numbers disagree, because sub-series/part numbering
// makes those numbers unreliable (a first symmetric-penalty fix broke exactly
// that, marking whole libraries "to import" - the cases below are from it).
func TestBestSequenceConflict(t *testing.T) {
	books := []Book{
		0: {Author: "Eric Dontigney", Series: "Unintended Cultivator", Title: "Unintended Cultivator, Volume 2"},
		1: {Author: "Eric Dontigney", Series: "Unintended Cultivator", Title: "Unintended Cultivator: Volume 3"},
		2: {Author: "Eric Dontigney", Series: "Unintended Cultivator", Title: "Unintended Cultivator, Volume Eight"},
		3: {Author: "J.R. Mathews", Series: "Portal to Nova Roma", Title: "Portal to Nova Roma (Unabridged)"},
		// Real regression shapes: the title number is sub-series/part numbering that
		// disagrees with the Audible series sequence, or the SeriesIndex is junk.
		4: {Author: "Yrsillar", Series: "Destiny Cycle", Title: "Threads of Destiny: Volume 5"},
		5: {Author: "Lindsay Buroker", Series: "The Emperor's Edge", Title: "Forged in Blood: Part 1"},
		// "1PL04 - …" folder shortcode parses to SeriesIndex 1; the explicit
		// "Volume 4" in the bare title must outrank it.
		6: {Author: "Robert Blaise", Series: "1% Lifesteal", Title: "1% Lifesteal, Volume 4", SeriesIndex: 1},
		// Untrustworthy-number shapes the veto must NOT act on: a junk index on a
		// numberless bare title, an incidental "(2015)" year, a digit-named series
		// whose spelling difference keeps it from being stripped.
		7: {Author: "A. N. Author", Series: "Some Series", Title: "Some Series (Unabridged)", SeriesIndex: 3},
		8: {Author: "Some Author", Series: "Series Name", Title: "Series Name (2015): Volume 3", SeriesIndex: 3},
		9: {Author: "Asato Asato", Series: "Eighty-Six", Title: "86: Volume 9", SeriesIndex: 9},
	}
	q := func(title, author, series string, seq float64) Query {
		return Query{Title: title, Author: author, Series: series, Sequence: seq, HasSequence: true}
	}
	cases := []bestCase{
		{"not-yet-imported volume must not match a sibling", q("Unintended Cultivator: Volume 9", "Eric Dontigney", "Unintended Cultivator", 9), -1},
		{"digit volume still matches itself", q("Unintended Cultivator: Volume 3", "Eric Dontigney", "Unintended Cultivator", 3), 1},
		{"word-number volume still matches (no derivable sequence)", q("Unintended Cultivator, Volume Eight", "Eric Dontigney", "Unintended Cultivator", 8), 2},
		{"series-titled book 1 still matches (no derivable sequence)", q("Portal to Nova Roma", "J.R. Mathews", "Portal to Nova Roma", 1), 3},
		{"sub-series volume number outranked by title words", q("Threads of Destiny: Volume 5", "Yrsillar", "Destiny Cycle", 8), 4},
		{"part number outranked by title words", q("Forged in Blood: Part 1", "Lindsay Buroker", "The Emperor's Edge", 6), 5},
		{"junk SeriesIndex outranked by explicit bare-title volume", q("1% Lifesteal, Volume 4", "Robert Blaise", "1% Lifesteal", 4), 6},
		{"junk SeriesIndex still blocks a genuinely different bare volume", q("1% Lifesteal, Volume 9", "Robert Blaise", "1% Lifesteal", 9), -1},
		{"junk SeriesIndex on a numberless bare title never vetoes", q("Some Series", "A. N. Author", "Some Series", 1), 7},
		{"incidental year is not a volume number", q("Series Name: Volume 3", "Some Author", "Series Name", 3), 8},
		{"digit-named series is not a volume number", q("86: Volume 9", "Asato Asato", "Eighty-Six", 9), 9},
	}
	runBestCases(t, books, cases)
}

// TestBestBareSeriesTitle pins the real-world mass false positive: Audible lists
// every volume of some series (Kirill Klevanski's "Dragon Heart") with the BARE
// series name as each volume's title and the volume number only in the series
// sequence - and names the series "Dragon Heart Series" where the server has
// "Dragon Heart". The old matcher saw different series (no veto path) and matched
// every volume to whichever server book's title tokens were exactly the series
// name, marking a dozen unowned volumes "already on server" (and backfilling one
// of their ASINs onto that book). A bare title must match on the volume number
// alone: agree → match, disagree/unknown → no match (volume 1 may still claim a
// numberless standalone-shaped book).
func TestBestBareSeriesTitle(t *testing.T) {
	books := []Book{
		// A scanned title tag that is just the series name, no number anywhere.
		0: {Author: "Kirill Klevanski", Series: "Dragon Heart", Title: "Dragon Heart"},
		// The same shape with a series index - an indexed volume 7.
		1: {Author: "Kirill Klevanski", Series: "Dragon Heart", Title: "Dragon Heart", SeriesIndex: 7},
		// A series-less scan whose title is the series name, indexed volume 5.
		2: {Author: "Kirill Klevanski", Series: "", Title: "Dragon Heart", SeriesIndex: 5},
		// A real-word title in the series never absorbs bare-title volumes.
		3: {Author: "Kirill Klevanski", Series: "Dragon Heart", Title: "DH06 - Blaze of Fury"},
	}
	q := func(seq float64) Query {
		return Query{Title: "Dragon Heart", Author: "Kirill Klevanski", Series: "Dragon Heart Series", Sequence: seq, HasSequence: true}
	}
	cases := []bestCase{
		{"volume 7 matches the indexed volume 7", q(7), 1},
		{"volume 5 matches the series-less indexed volume 5", q(5), 2},
		{"volume 13 matches nothing", q(13), -1},
		{"volume 2 matches nothing (not absorbed by a numberless sibling)", q(2), -1},
		{"volume 1 may claim the numberless standalone shape", q(1), 0},
	}
	runBestCases(t, books, cases)
}

// TestBestSeriesLessQuery pins the series-less query shape: a scanned library book
// with no series column matched against catalog cards that nearly all carry one.
// Before the candidate-series derivation nothing linked the two, so the bare branch
// was unreachable from this side - certain matches scored 0 (the card's residual is
// a number, so it has no tokens at all) and sibling volumes won on token overlap
// with the veto that exists to stop them never running. Cases are real pairings from
// a 1147-book library.
func TestBestSeriesLessQuery(t *testing.T) {
	books := []Book{
		// Card title reduces to nothing once its own series is stripped: zero tokens.
		0: {Author: "Alex Maher", Series: "The Hedge Wizard", SeriesIndex: 2, Title: "The Hedge Wizard 2"},
		// The query title IS "card's series + number"; the card's own title shares
		// not one token with it.
		1: {Author: "Aleron Kong", Series: "Chaos Seeds", SeriesIndex: 1, Title: "The Land: Founding"},
		// Same, with a parenthetical ordering note decorating the card's series.
		2: {Author: "Lois McMaster Bujold", Series: "Vorkosigan Saga (chronological)", SeriesIndex: 1, Title: "Falling Free"},
		// The sibling: its empty residual makes CleanTitle fall back to the
		// series-laden title, a full token subset of the query. Only the veto stops it.
		3: {Author: "C. Mantis", Series: "The Path of Ascension", SeriesIndex: 10.5, Title: "The Path of Ascension Book 10.5"},
		4: {Author: "C. Mantis", Series: "The Path of Ascension", SeriesIndex: 4, Title: "The Path of Ascension 4"},
		// A junk duplicate card with no series wins on token subset alone (1.0);
		// the correct card shares no title token and must outrank it on the number.
		5: {Author: "Kevin Hearne", Series: "", Title: "Hammered: The Iron Druid Chronicles, Book 3"},
		6: {Author: "Kevin Hearne", Series: "Iron Druid Chronicles", SeriesIndex: 8.75, Title: "Oberon's Meaty Mysteries: The Squirrel on the Train"},
		// Edition decoration only: the residual is a parenthetical matchNoise drops.
		7: {Author: "Ben Aaronovitch", Series: "Rivers of London", SeriesIndex: 1, Title: "Rivers of London"},
		// No derivation - the title never names "Destiny Cycle" - so the conflicting
		// sub-series number stays outranked by matching title words, as before.
		8: {Author: "Yrsillar", Series: "Destiny Cycle", SeriesIndex: 8, Title: "Threads of Destiny: Volume 5"},
		// A short series name embedded in an unrelated word must not link.
		9: {Author: "Cate Land", Series: "Land", SeriesIndex: 3, Title: "Land 3"},
	}
	q := func(title, author string) Query {
		return Query{Title: title, TitleShort: title, Author: author}
	}
	cases := []bestCase{
		{"card title strips to nothing under its own series", q("The Hedge Wizard 2", "Alex Maher"), 0},
		{"query title is the card's series + number", q("Chaos Seeds 1", "Aleron Kong"), 1},
		{"parenthetical decoration on the card's series", q("Vorkosigan Saga 01", "Lois McMaster Bujold"), 2},
		{"conflicting volume loses to the right one", q("The Path of Ascension 4", "C. Mantis"), 4},
		{"numbered series card outranks a junk duplicate", q("Iron Druid Chronicles 08.75", "Kevin Hearne"), 6},
		{"edition decoration is a bare volume-1 shape", q("Rivers of London (Unabridged)", "Ben Aaronovitch"), 7},
		{"unnamed series still matches on title words", q("Threads of Destiny: Volume 5", "Yrsillar"), 8},
		{"embedded series name does not link", q("The Landlord's Daughter", "Cate Land"), -1},
	}
	runBestCases(t, books, cases)
	// The veto, asserted alone: with only the sibling card present the query must
	// match NOTHING rather than fall through to its token overlap.
	if got, ok := Best(books[3:4], q("The Path of Ascension 4", "C. Mantis")); ok {
		t.Errorf("conflicting volume card must not match: got idx %d (%q)", got, books[3:4][got].Title)
	}
	// The junk duplicate alone still matches as it always did - the derivation only
	// adds a competitor, it does not change a series-less card's scoring.
	if got, ok := Best(books[5:6], q("Iron Druid Chronicles 08.75", "Kevin Hearne")); !ok || got != 0 {
		t.Errorf("series-less card scoring changed: got idx %d ok=%v, want 0 true", got, ok)
	}
}

// TestBestFullTitleOutranksSequence pins the false positive a live replay found:
// the same-series sequence bonus awards near-certain credit on a number the
// matcher elsewhere refuses to trust, so a sibling agreeing only on the number
// beat a sibling whose title matched in full. Either side's number can be junk -
// a local tag numbering a whole franchise instead of the series, a folder
// shortcode - while a book is retitled far less often than a catalog renumbers
// its positions. Both failing cases are real pairings against live card payloads.
func TestBestFullTitleOutranksSequence(t *testing.T) {
	books := []Book{
		// The library tags Primordium 9 (Halo publication order); the catalog
		// numbers the series, where 9 is a different book.
		0: {Author: "Greg Bear", Series: "Halo", SeriesIndex: 8, Title: "Halo: Primordium"},
		1: {Author: "Greg Bear", Series: "Halo", SeriesIndex: 9, Title: "Halo: Silentium"},
		// The library's tag is the sub-series volume (1); the real position is 4.
		2: {Author: "Yrsillar", Series: "Destiny Cycle", SeriesIndex: 4, Title: "Threads of Destiny: Volume 1"},
		3: {Author: "Yrsillar", Series: "Destiny Cycle", SeriesIndex: 1, Title: "Forge of Destiny"},
		// A correctly-tagged library: one card wins the subset AND the number.
		4: {Author: "Ann Leckie", Series: "Imperial Radch", SeriesIndex: 2, Title: "Ancillary Sword"},
		5: {Author: "Ann Leckie", Series: "Imperial Radch", SeriesIndex: 3, Title: "Ancillary Mercy"},
		// Control: the full-subset card is in a DIFFERENT series, so it earns no
		// boost and stays at 1.0 - below the same-series number agreement's 2.0.
		// Were the boost unconditional it would tie at 2.0 and win on position.
		6: {Author: "Nona Trace", Series: "Other Series", SeriesIndex: 1, Title: "The Glass Bell"},
		7: {Author: "Nona Trace", Series: "Bell Cycle", SeriesIndex: 5, Title: "Winter Harbour"},
	}
	q := func(title, author, series string, seq float64) Query {
		return Query{Title: title, TitleShort: title, Author: author, Series: series, Sequence: seq, HasSequence: true}
	}
	cases := []bestCase{
		{"franchise-order tag loses to the matching title", q("Halo: Primordium (Unabridged)", "Greg Bear", "HALO", 9), 0},
		{"sub-series tag loses to the matching title", q("Threads of Destiny: Volume 1: Destiny Cycle, Book 4", "Yrsillar", "Destiny Cycle", 1), 2},
		{"subset plus agreeing sequence still ranks highest", q("Ancillary Sword", "Ann Leckie", "Imperial Radch", 2), 4},
		{"no boost without series agreement", q("The Glass Bell", "Nona Trace", "Bell Cycle", 5), 7},
	}
	runBestCases(t, books, cases)
}

// TestBestSeriesSpellingForms pins the ONE list of series spellings (seriesForms)
// that both finds a series name in a title and removes it. Each case failed before
// the two sides shared it: a form that could link a pair could not strip it, and an
// unbounded removal cut a series name out of the middle of an unrelated word.
func TestBestSeriesSpellingForms(t *testing.T) {
	books := []Book{
		// Audible's " Series" decoration: the title says "Dragon Heart".
		0: {Author: "Kirill Klevanski", Series: "Dragon Heart Series", Title: "Dragon Heart", SeriesIndex: 7},
		1: {Author: "Kirill Klevanski", Series: "Dragon Heart Series", Title: "Dragon Heart", SeriesIndex: 13},
		// "Land Series" contributes the form "Land", which must not be cut out of
		// "Landlord" - that turned the title into "The lord" and lost the match.
		2: {Author: "Ben Aaronovitch", Series: "Land Series", Title: "The Landlord", SeriesIndex: 2},
		// The catalog's ordering note: the title spells neither it nor the parens.
		3: {Author: "Lois McMaster Bujold", Series: "Vorkosigan Saga (chronological)", Title: "Vorkosigan Saga", SeriesIndex: 1},
		// A series-less card whose title omits the query series' leading article.
		4: {Author: "Alex Maher", Series: "", Title: "Hedge Wizard 3"},
		// A series-less card that reduces to nothing under FLUFF, not under the
		// series name - it is not the series-as-title shape and must not link.
		5: {Author: "A. N. Author", Series: "", Title: "Unabridged"},
	}
	bare := func(title, author string) Query {
		return Query{Title: title, TitleShort: title, Author: author}
	}
	tagged := func(title, author, series string, seq float64) Query {
		return Query{Title: title, TitleShort: title, Author: author, Series: series, Sequence: seq, HasSequence: true}
	}
	runBestCases(t, books, []bestCase{
		{"suffix-decorated series links its volume", bare("Dragon Heart 13", "Kirill Klevanski"), 1},
		{"suffix-decorated series claims no other volume", bare("Dragon Heart 4", "Kirill Klevanski"), -1},
		{"a series form is never cut out of a longer word", bare("The Landlord", "Ben Aaronovitch"), 2},
		{"paren-decorated series claims the volume-1 shape", bare("Vorkosigan Saga", "Lois McMaster Bujold"), 3},
		{"paren-decorated series absorbs no other volume", bare("Vorkosigan Saga 5", "Lois McMaster Bujold"), -1},
		{"candidate links on the article-dropped form", tagged("The Hedge Wizard 3", "Alex Maher", "The Hedge Wizard", 3), 4},
		{"a fluff-only title is not the series name", tagged("Some Series", "A. N. Author", "Some Series", 1), -1},
	})
}

// TestBestNumberTrust pins which number each side of the bare branch may speak
// with. A title carrying real words states no position - its "Volume 5"/"Part 1"
// marker is sub-series or part numbering - so it may neither veto nor confirm on
// it, in either direction. Every case below was decided by that noise before.
func TestBestNumberTrust(t *testing.T) {
	books := []Book{
		// "Part 1" is a part number; the book is Emperor's Edge #6.
		0: {Author: "Lindsay Buroker", Series: "The Emperor's Edge", Title: "Forged in Blood: Part 1", SeriesIndex: 6},
		// "Volume 5" is the sub-series volume; the book is Destiny Cycle #8.
		1: {Author: "Yrsillar", Series: "Destiny Cycle", Title: "Threads of Destiny: Volume 5", SeriesIndex: 8},
		2: {Author: "Yrsillar", Series: "Destiny Cycle", Title: "Destiny Cycle: Volume 5"},
		3: {Author: "Yrsillar", Series: "Destiny Cycle", Title: "Destiny Cycle: Volume 8"},
		// The same shape under another author, so the wordy-query case is not
		// simply won by the identical title above.
		4: {Author: "Y. Second", Series: "Destiny Cycle", Title: "Destiny Cycle: Volume 5"},
		5: {Author: "Y. Second", Series: "Destiny Cycle", Title: "Destiny Cycle: Volume 8"},
	}
	runBestCases(t, books, []bestCase{
		{
			"a wordy candidate's part number never vetoes",
			Query{Title: "The Emperor's Edge, Book 6", TitleShort: "The Emperor's Edge, Book 6", Author: "Lindsay Buroker", Series: "The Emperor's Edge", Sequence: 6, HasSequence: true},
			0,
		},
		{
			"a wordy candidate's marker never confirms",
			Query{Title: "Destiny Cycle 5", TitleShort: "Destiny Cycle 5", Author: "Yrsillar"},
			2,
		},
		{
			"a wordy query's marker never confirms",
			Query{Title: "Threads of Destiny: Volume 5", TitleShort: "Threads of Destiny: Volume 5", Author: "Y. Second", Series: "Destiny Cycle", Sequence: 8, HasSequence: true},
			5,
		},
	})
}

// TestBestBoostEvidence pins the two limits on the full-title boost: it is
// DIRECTIONAL (the candidate must be covered by the query, not the reverse), and a
// single shared token is only evidence when both titles reduce to exactly it.
func TestBestBoostEvidence(t *testing.T) {
	books := []Book{
		0: {Author: "George Martin", Series: "A Song of Ice and Fire", Title: "Winter", SeriesIndex: 9},
		1: {Author: "George Martin", Series: "A Song of Ice and Fire", Title: "A Dance with Dragons", SeriesIndex: 6},
		// The omnibus is listed FIRST: an undirected boost would tie it with the
		// true card and it would win on position.
		2: {Author: "Greg Bear", Series: "Halo", Title: "Halo: Primordium & Silentium"},
		3: {Author: "Greg Bear", Series: "Halo", Title: "Halo: Primordium", SeriesIndex: 8},
	}
	runBestCases(t, books, []bestCase{
		{
			"a lone shared token is not a title match",
			Query{Title: "The Winds of Winter", TitleShort: "The Winds of Winter", Author: "George Martin", Series: "A Song of Ice and Fire", Sequence: 6, HasSequence: true},
			1,
		},
		{
			"an omnibus card is not covered by the query title",
			Query{Title: "Halo: Primordium", TitleShort: "Halo: Primordium", Author: "Greg Bear", Series: "Halo"},
			3,
		},
	})
}

// TestBestNarratorGate pins the person gate. Real libraries tag the NARRATOR as
// the author (David Tennant on Cressida Cowell's books) while catalog cards carry
// author and narrators separately, so the gate crosses the two - but only against
// an author, never narrator-to-narrator, which a prolific narrator would turn into
// a link between unrelated books.
func TestBestNarratorGate(t *testing.T) {
	books := []Book{
		0: {Author: "Cressida Cowell", Title: "How to Train Your Dragon", Narrators: []string{"David Tennant"}},
		1: {Author: "Terry Pratchett", Title: "Guards! Guards!", Narrators: []string{"Nigel Planer"}},
	}
	runBestCases(t, books, []bestCase{
		{
			"a library tagging the narrator as author still matches",
			Query{Title: "How to Train Your Dragon", TitleShort: "How to Train Your Dragon", Author: "David Tennant"},
			0,
		},
		{
			"a query narrator matches the card's author",
			Query{Title: "How to Train Your Dragon", TitleShort: "How to Train Your Dragon", Author: "Nobody At All", Narrators: []string{"Cressida Cowell"}},
			0,
		},
		{
			"narrator to narrator is not a link",
			Query{Title: "Guards! Guards!", TitleShort: "Guards! Guards!", Author: "Nobody At All", Narrators: []string{"Nigel Planer"}},
			-1,
		},
		{
			"an unrelated person never matches",
			Query{Title: "How to Train Your Dragon", TitleShort: "How to Train Your Dragon", Author: "Unrelated Person"},
			-1,
		},
	})
}

func TestCleanTitle(t *testing.T) {
	cases := []struct{ title, series, want string }{
		{"Diary of a Wimpy Kid: The Ugly Truth (Book 5)", "Diary of a Wimpy Kid", "The Ugly Truth"},
		{"Silverspear: Rise to Omniscience, Book 6", "Rise to Omniscience", "Silverspear"},
		{"The Deep End - Diary of a Wimpy Kid Series, Book 15", "Diary of a Wimpy Kid", "The Deep End"},
		{"Legion's Fifth Vault: A Fantasy LitRPG Adventure", "Divine Apostasy", "Legion's Fifth Vault"},
		{"The Ugly Truth", "Diary of a Wimpy Kid", "The Ugly Truth"},
		{"Shadow's Edge (Unabridged)", "", "Shadow's Edge"},
	}
	for _, tc := range cases {
		if got := CleanTitle(tc.title, tc.series); got != tc.want {
			t.Errorf("CleanTitle(%q, %q) = %q, want %q", tc.title, tc.series, got, tc.want)
		}
	}
}

// TestRemoveFoldNonASCII pins that removeFold no longer corrupts titles containing
// a rune whose lowercase form differs in byte length (e.g. 'İ' U+0130 → "i"). The
// old version sliced s with byte offsets taken from its lowercased copy, dropping
// or garbling such characters.
func TestRemoveFoldNonASCII(t *testing.T) {
	if got := strings.TrimSpace(removeFold("İstanbul Saga", "Saga")); got != "İstanbul" {
		t.Errorf("removeFold corrupted non-ASCII title: got %q, want %q", got, "İstanbul")
	}
	if got := CleanTitle("İstanbul: The Saga Begins", "The Saga"); !strings.Contains(got, "İstanbul") {
		t.Errorf("CleanTitle corrupted non-ASCII title: got %q, want it to contain %q", got, "İstanbul")
	}
}

func TestSeqFromTitle(t *testing.T) {
	cases := []struct {
		title, series string
		want          float64
		ok            bool
	}{
		{"Return of the Runebound Professor 3", "Return of the Runebound Professor", 3, true},
		{"Diary of a Wimpy Kid: The Ugly Truth (Book 5)", "Diary of a Wimpy Kid", 5, true},
		{"Legion's Fifth Vault", "Divine Apostasy", 0, false},
	}
	for _, tc := range cases {
		got, ok := SeqFromTitle(tc.title, tc.series)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("SeqFromTitle(%q,%q) = %v,%v want %v,%v", tc.title, tc.series, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"L. A. McBride":     "lamcbride",
		"L.A. McBride":      "lamcbride", // differently-spaced initials normalize equal
		"Brandon Sanderson": "brandonsanderson",
		"  Will  Wight  ":   "willwight",
		"":                  "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeSeries(t *testing.T) {
	cases := map[string]string{
		"The Primal Hunter":   "primalhunter",
		"primal hunter":       "primalhunter",
		"A Court of Thorns":   "courtofthorns",
		"Theory of Magic":     "theoryofmagic", // "the" only stripped as a whole word
		"  Cradle  ":          "cradle",
		"Dragon Heart Series": "dragonheart", // trailing " series" is decoration
		"Series":              "series",      // …but only as a suffix, not the whole name
	}
	for in, want := range cases {
		if got := NormalizeSeries(in); got != want {
			t.Errorf("NormalizeSeries(%q) = %q, want %q", in, got, want)
		}
	}
}

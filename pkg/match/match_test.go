package match

import (
	"strings"
	"testing"
)

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
	cases := []struct {
		name string
		q    Query
		want int // -1 = expect no match
	}{
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
	cases := []struct {
		name string
		q    Query
		want int // -1 = expect no match
	}{
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
		"The Primal Hunter": "primalhunter",
		"primal hunter":     "primalhunter",
		"A Court of Thorns": "courtofthorns",
		"Theory of Magic":   "theoryofmagic", // "the" only stripped as a whole word
		"  Cradle  ":        "cradle",
	}
	for in, want := range cases {
		if got := NormalizeSeries(in); got != want {
			t.Errorf("NormalizeSeries(%q) = %q, want %q", in, got, want)
		}
	}
}

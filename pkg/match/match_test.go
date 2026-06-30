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

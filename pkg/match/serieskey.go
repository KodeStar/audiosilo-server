package match

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// SeriesKey reduces a series name to a comparison key with no case, accents,
// punctuation or spacing: "The Expanse", "the  expanse!" and "Thé Expanse" share
// one. It is the admin console's seriesKey (NFKD, keep every letter and number,
// lowercase), so a series the console places on a community rail and one the
// server places there agree. Not Fold over NFKD: Fold keeps digits only, and a
// number that has no decomposition (〇, ↂ) would fold away. "" when no
// letter or number is left.
func SeriesKey(name string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(name) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return strings.ToLower(b.String())
}

// SameSeries reports whether a and b name one series: the same SeriesKey, so
// "He Who Fights With Monsters" and "He Who Fights WIth Monsters" are one. A name
// with no letter or number has no key and is one series only with itself, and a
// blank name is no series at all.
func SameSeries(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ka, kb := SeriesKey(a), SeriesKey(b)
	if ka == "" || kb == "" {
		return a == b
	}
	return ka == kb
}

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
// server places there agree. "" when no letter or number is left.
func SeriesKey(name string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(name) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return strings.ToLower(b.String())
}

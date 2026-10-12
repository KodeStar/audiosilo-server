package metadata

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// PositionInTitle reads a book's position in its series from a title that is the
// series' name followed by a number, as many publishers title a series' books:
// "He Who Fights with Monsters 4: A LitRPG Adventure", "He Who Fights with
// Monsters 10 - A LitRPG Adventure", "The Expanse, Book 3", "Discworld #8". The
// name is compared as a series key is (case, accents, punctuation and spacing
// aside, so the tag "He Who Fights With Monsters" reads the title "He Who Fights
// with Monsters 4"), and must end where a word does. Kept narrow, since the
// position it gives places the book in its series:
//
//   - the number is set off by a space or a comma ("Catch-22" in a series named
//     "Catch" gives none), optionally after "Book", "Vol.", "Volume", "Part",
//     "No." or "#";
//   - it has one to three digits, maybe a decimal part ("2.5"), and is above 0, so a
//     year ("Monsters 1999") gives none;
//   - the title ends after it, or a separator follows (":", "-", a dash, "(",
//     "[", ",", ";", "|"), so "4th Edition" and "Jack Reacher 61 Hours" give none;
//     a dash or a comma and another number is a range of books ("Dork Diaries
//     1-12", "Books 1, 2"), which gives none;
//   - anything else between the name and the number gives none ("Monsters of 4"),
//     and so does a title that doesn't start with the name ("2001: A Space
//     Odyssey" in a series named "Space Odyssey").
//
// ok is false when the title isn't so.
func PositionInTitle(title, series string) (float64, bool) {
	rest, ok := afterName(title, series)
	if !ok {
		return 0, false
	}
	m := titleNumber.FindStringSubmatch(rest)
	if m == nil || titleRange.MatchString(rest[len(m[0])-len(m[2]):]) {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// titleNumber is the number after a series' name: set off by a space or a
// comma, maybe after a word for a volume, then the end or a separator (m[2]).
var titleNumber = regexp.MustCompile(`^(?:\s*,\s*|\s+)(?i:(?:book|vol(?:ume)?\.?|part|no\.)\s*)?#?(\d{1,3}(?:\.\d+)?)\s*($|[-:([,;|\x{2013}\x{2014}])`)

// titleRange is a separator that makes the number the start of a range: a dash
// or a comma, then another number.
var titleRange = regexp.MustCompile(`^[-,\x{2013}\x{2014}]\s*\d`)

// afterName is what follows the series' name at the start of title (compared by
// series key, match.SeriesKey: letters and numbers alone, without case or
// accents), when the name ends where a word does.
func afterName(title, series string) (string, bool) {
	want := match.SeriesKey(series)
	if want == "" {
		return "", false
	}
	var got strings.Builder
	for i, r := range title {
		got.WriteString(match.SeriesKey(string(r)))
		if !strings.HasPrefix(want, got.String()) {
			return "", false
		}
		if got.Len() == len(want) {
			// Marks combining with the name's last letter are part of it: a title
			// in decomposed form (NFD, as macOS names files) spells "é" as "e" + U+0301.
			rest := strings.TrimLeftFunc(title[i+utf8.RuneLen(r):], func(r rune) bool { return unicode.Is(unicode.Mn, r) })
			next, _ := utf8.DecodeRuneInString(rest)
			if unicode.IsLetter(next) || unicode.IsNumber(next) {
				return "", false // the name ends mid-word: "Catch" in "Catcher"
			}
			return rest, true
		}
	}
	return "", false
}

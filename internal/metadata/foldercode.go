package metadata

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// folderCode is a leading series code on a book's folder or file name: one to
// five letters, maybe a space, then the book's number with one to three digits,
// maybe a decimal part ("DH06 - Land of Magic", "BDM01 - Oathbound Healer",
// "DF15.5 Brief Cases", "HALO 07 - Evolutions"), or a range of them, the two
// numbers joined by a hyphen or an en dash with no spaces ("LAM05-06", an
// omnibus). The code must end where a word does ("PH15x" is none), and a
// third number joined on ("LAM05-06-07") makes it none, not a shorter range.
// A year or a number with a letter labels a single position. Check these
// suffixes before the range end, so "DH06-2019" cannot become a range to 201.
var folderCode = regexp.MustCompile(`^([A-Za-z]{1,5}) ?(\d{1,3}(?:\.\d+)?)(?:[-–](?:(?:19|20)\d{2}|\d{1,3}[A-Za-z]|(\d{1,3}(?:\.\d+)?)))?`)

// FolderCodePosition reads a book's position in series from the code that
// leads its folder (or file) name, name being that last path segment. The code's
// letters (apart from the explicit volume marker C) must be the series' initials, every word's or some of them in order
// ("DH" for "Dragon Heart", "BDM" for "Beneath the Dragoneye Moons", "FWM" for
// "He Who Fights with Monsters"): a code that isn't ("HALO 07" in series "Halo",
// a franchise's own numbering) gives none, as does a number of 0. The number
// reads as ParseSeriesPosition reads one: end is 0 for a single position and a
// range's end for a range ("LAM05-06" is 5 to 6), and a range that isn't one
// ("LAM06-05") gives none. ok is false when there is no such code.
func FolderCodePosition(name, series string) (start, end float64, ok bool) {
	name = strings.TrimSpace(name)
	m := folderCode.FindStringSubmatch(name)
	if m == nil || strings.TrimSpace(series) == "" {
		return 0, 0, false
	}
	// The scanner reads C02 as an explicit volume marker in any series
	// (splitSeriesIndex). Every other code must spell the series' initials.
	if !strings.EqualFold(m[1], "c") && !initialsOf(m[1], series) {
		return 0, 0, false
	}
	rest := name[len(m[0]):]
	if !wordEndsAt(rest) || joinsNumber(rest) {
		return 0, 0, false
	}
	position := m[2]
	if m[3] != "" {
		position += "-" + m[3]
	}
	start, end, ok = ParseSeriesPosition(position)
	if !ok || start == 0 {
		return 0, 0, false
	}
	return start, end, true
}

// apostrophes are dropped before a series is split into words, so one joins
// a word ("Emperor's Edge" is two words).
var apostrophes = strings.NewReplacer("'", "", "’", "")

// initialsOf reports whether code's letters are, case aside, the initials of
// series' words, all or some of them in order.
func initialsOf(code, series string) bool {
	words := strings.FieldsFunc(apostrophes.Replace(series), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	want := []rune(strings.ToLower(code))
	i := 0
	for _, w := range words {
		if i == len(want) {
			break
		}
		if first, _ := utf8.DecodeRuneInString(w); unicode.ToLower(first) == want[i] {
			i++
		}
	}
	return i == len(want)
}

// wordEndsAt reports whether no letter, digit or underscore starts s: the
// greedy number before it ended where a word does ("DF15.5x" is none, where a
// trailing \b would let the regexp back off to "DF15").
func wordEndsAt(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// joinsNumber reports whether s starts with a hyphen or an en dash then a digit:
// one more number joined onto a code's range ("LAM05-06-07").
func joinsNumber(s string) bool {
	r, n := utf8.DecodeRuneInString(s)
	if r != '-' && r != '–' {
		return false
	}
	d, _ := utf8.DecodeRuneInString(s[n:])
	return unicode.IsDigit(d)
}

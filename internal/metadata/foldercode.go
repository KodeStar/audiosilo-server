package metadata

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// folderCode is a leading series code on a book's folder or file name: one to
// five letters, maybe a space, then the book's number with one to three digits,
// maybe a decimal part ("DH06 - Land of Magic", "BDM01 - Oathbound Healer",
// "DF15.5 Brief Cases", "HALO 07 - Evolutions"), or a range of them, the two
// numbers joined by a hyphen or an en dash with no spaces ("LAM05-06", an
// omnibus). The code must end where a word does ("PH15x" is none).
var folderCode = regexp.MustCompile(`^([A-Za-z]{1,5}) ?(\d{1,3}(?:\.\d+)?)(?:[-–](\d{1,3}(?:\.\d+)?))?`)

// FolderCodePosition reads a book's position in series from the code that
// leads its folder (or file) name, name being that last path segment. The code's
// letters must be the series' initials, every word's or some of them in order
// ("DH" for "Dragon Heart", "BDM" for "Beneath the Dragoneye Moons", "FWM" for
// "He Who Fights with Monsters"): a code that isn't ("HALO 07" in series "Halo",
// a franchise's own numbering) gives none, as does a number of 0. end is 0 for
// a single position and a range's end for a range ("LAM05-06" is 5 to 6, so an
// omnibus never reads as its first volume); a range that isn't one by
// ValidSeriesRange ("LAM06-05") gives none. ok is false when there is no such
// code.
func FolderCodePosition(name, series string) (start, end float64, ok bool) {
	name = strings.TrimSpace(name)
	m := folderCode.FindStringSubmatch(name)
	if m == nil || !wordEndsAt(name[len(m[0]):]) || !initialsOf(m[1], series) {
		return 0, 0, false
	}
	start, err := strconv.ParseFloat(m[2], 64)
	if err != nil || start == 0 { // the pattern caps it at three digits
		return 0, 0, false
	}
	if m[3] == "" {
		return start, 0, true
	}
	end, err = strconv.ParseFloat(m[3], 64)
	if err != nil || !ValidSeriesRange(start, end) {
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

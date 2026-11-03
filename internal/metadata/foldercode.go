package metadata

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// folderCode is a leading series code on a book's folder or file name: one to
// five letters, maybe a space, then the book's number with one to three digits,
// maybe a decimal part ("DH06 - Land of Magic", "BDM01 - Oathbound Healer",
// "DF15.5 Brief Cases", "HALO 07 - Evolutions"). A range ("LAM05-06") reads as
// its first number. The number must end where a word does ("PH15x" is none).
var folderCode = regexp.MustCompile(`^([A-Za-z]{1,5}) ?(\d{1,3}(?:\.\d+)?)\b`)

// FolderCodePosition reads a book's position in series from the code that
// leads its folder (or file) name, name being that last path segment. The code's
// letters must be the series' initials, every word's or some of them in order
// ("DH" for "Dragon Heart", "BDM" for "Beneath the Dragoneye Moons", "FWM" for
// "He Who Fights with Monsters"): a code that isn't ("HALO 07" in series "Halo",
// a franchise's own numbering) gives none, as does a number of 0. ok is false
// when there is no such code.
func FolderCodePosition(name, series string) (float64, bool) {
	m := folderCode.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil || !initialsOf(m[1], series) {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[2], 64)
	if err != nil || v <= 0 || v > MaxSeriesPosition {
		return 0, false
	}
	return v, true
}

// initialsOf reports whether code's letters are, case aside, the initials of
// series' words, all or some of them in order. An apostrophe joins a word
// ("Emperor's Edge" is two words).
func initialsOf(code, series string) bool {
	clean := strings.NewReplacer("'", "", "’", "").Replace(series)
	words := strings.FieldsFunc(clean, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	want := []rune(strings.ToLower(code))
	i := 0
	for _, w := range words {
		if i == len(want) {
			break
		}
		first := []rune(strings.ToLower(w))[0]
		if first == want[i] {
			i++
		}
	}
	return len(want) > 0 && i == len(want)
}

package matchrun

import (
	"slices"
	"strings"
	"unicode"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Title tidy: a confident match's fill also replaces a title the files clutter
// ("Emergence: The Mystic Knight, Book 1 (Unabridged)") with the community's
// ("Emergence"), when what the files add is only clutter. Fill otherwise fills
// blanks only, so without it such a book kept its file title for good.

// clutterWords are the words a file title adds to a book's title that say
// nothing of which book it is: numbering, edition and format notes, genre
// labels a store puts in the title, and the joining words between them ("Book
// One of the Stormlight Archive"). A word naming another edition
// ("dramatized", "adaptation") is NOT clutter: that title stays.
var clutterWords = map[string]bool{
	"book": true, "books": true, "bk": true, "vol": true, "vols": true, "volume": true, "volumes": true,
	"part": true, "pt": true, "no": true, "nr": true, "number": true, "episode": true,
	"unabridged": true, "abridged": true, "novel": true, "novella": true, "edition": true,
	"series": true, "audiobook": true, "audio": true,
	"litrpg": true, "gamelit": true, "progression": true, "cultivation": true, "epic": true,
	"fantasy": true, "adventure": true,
	"of": true, "the": true, "a": true, "an": true,
	"one": true, "two": true, "three": true, "four": true, "five": true, "six": true, "seven": true,
	"eight": true, "nine": true, "ten": true, "eleven": true, "twelve": true, "thirteen": true,
	"fourteen": true, "fifteen": true, "sixteen": true, "seventeen": true, "eighteen": true,
	"nineteen": true, "twenty": true,
}

// tidyTitle is the community title (theirs) to write over the book's title in
// state st under fill, "" for none: the book has a title that is no admin
// edit, it differs from theirs, and once the community title, the series names
// (the community's and the book's own), the community subtitle and clutter
// (clutterWords, numbers, roman numerals) are taken out of it, nothing is left.
// Any other word left over is the files saying something the match doesn't
// (another book, another edition), so the title stays.
func tidyTitle(st *catalog.MatchState, p catalog.MatchProposal) string {
	theirs, mine := p.Values[catalog.FieldTitle], st.Fields[catalog.FieldTitle]
	if theirs == "" || mine.Source == catalog.SourceEdited || strings.TrimSpace(mine.Value) == "" ||
		catalog.CleanOverride(catalog.FieldTitle, mine.Value) == theirs {
		return ""
	}
	left, ok := cut(titleWords(mine.Value), titleWords(theirs))
	if !ok {
		return "" // the community title isn't in it at all
	}
	extras := []string{p.Values[catalog.FieldSubtitle], p.Values[catalog.FieldSeries],
		st.Fields[catalog.FieldSeries].Value, st.Fields[catalog.FieldSeries].Scanned}
	for _, more := range []string{p.Values[catalog.FieldMoreSeries], st.Fields[catalog.FieldMoreSeries].Value} {
		for _, s := range catalog.ParseMoreSeries(more) {
			extras = append(extras, s.Name)
		}
	}
	// The longest first, so a series named inside another ("Mystic Knight" in
	// "The Mystic Knight Saga") is taken out whole.
	slices.SortFunc(extras, func(a, b string) int { return len(b) - len(a) })
	for _, extra := range extras {
		ws := titleWords(extra)
		// Every occurrence, a series named twice ("X: Y (X Book 1)"), with or
		// without its article ("Mystic Knight" of "The Mystic Knight").
		for _, form := range [][]string{ws, withoutArticle(ws)} {
			for found := true; found; {
				left, found = cut(left, form)
			}
		}
	}
	for _, w := range left {
		if !clutterWords[w] && !isNumber(w) && !isRoman(w) {
			return ""
		}
	}
	return theirs
}

// titleWords is s's words, lowercased: runs of letters and digits in any
// script, an apostrophe inside a word kept out ("Sharpe's" is "sharpes").
func titleWords(s string) []string {
	s = apostrophes.Replace(strings.ToLower(s))
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

var apostrophes = strings.NewReplacer("'", "", "’", "")

// withoutArticle is ws less a leading "the", "a" or "an" (nil when that leaves
// nothing, or there is none).
func withoutArticle(ws []string) []string {
	if len(ws) > 1 && (ws[0] == "the" || ws[0] == "a" || ws[0] == "an") {
		return ws[1:]
	}
	return nil
}

// cut takes the first run of ws that equals sub out of ws (ok false when there
// is none, or sub is empty).
func cut(ws, sub []string) ([]string, bool) {
	if len(sub) == 0 {
		return ws, false
	}
	for i := 0; i+len(sub) <= len(ws); i++ {
		if slices.Equal(ws[i:i+len(sub)], sub) {
			return slices.Concat(ws[:i], ws[i+len(sub):]), true
		}
	}
	return ws, false
}

// isNumber reports whether w is a number: digits ("8", "03"), or digits with
// an ordinal ending ("1st", "2nd").
func isNumber(w string) bool {
	digits := strings.TrimRightFunc(w, unicode.IsLetter)
	if digits == "" || strings.TrimLeftFunc(digits, unicode.IsDigit) != "" {
		return false
	}
	suffix := w[len(digits):]
	return suffix == "" || suffix == "st" || suffix == "nd" || suffix == "rd" || suffix == "th"
}

// isRoman reports whether w is a roman numeral up to xxxix ("ii", "iv",
// "xii"): a book number. A lone "i" counts (Book I), a word such as "mix"
// doesn't.
func isRoman(w string) bool {
	tens := strings.TrimLeft(w, "x")
	if len(w)-len(tens) > 3 {
		return false
	}
	switch tens {
	case "", "i", "ii", "iii", "iv", "v", "vi", "vii", "viii", "ix":
		return w != ""
	}
	return false
}

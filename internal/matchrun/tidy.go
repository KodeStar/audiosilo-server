package matchrun

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// Title tidy: fill also replaces a title the files clutter ("Emergence: The
// Mystic Knight, Book 1 (Unabridged)") with the community's ("Emergence"), when
// what the files add is only clutter. Fill otherwise fills blanks only, so without
// it such a book kept its file title for good.

// clutterWords are the words a file title adds to a book's title that say
// nothing of which book it is: numbering, edition and format notes, genre
// labels a store puts in the title, and the joining words between them ("Book
// One of the Stormlight Archive"). A word naming another edition ("abridged",
// "dramatized", "adaptation") or a split of the book ("part") is NOT clutter:
// that title stays, as it does for a number no position of the book states
// (statedNumbers).
var clutterWords = map[string]bool{
	"book": true, "books": true, "bk": true, "vol": true, "vols": true, "volume": true, "volumes": true,
	"no": true, "nr": true, "number": true, "episode": true,
	"unabridged": true, "novel": true, "novella": true, "edition": true,
	"series": true, "audiobook": true, "audio": true,
	"litrpg": true, "gamelit": true, "progression": true, "cultivation": true, "epic": true,
	"fantasy": true, "adventure": true,
	"of": true, "the": true, "a": true, "an": true,
}

// tidyTitle is the community title (theirs) to write over the book's title in
// state st under fill, "" for none: the book has a title that is no admin
// edit, it differs from theirs, and once the community title, the series names
// (the community's and the book's own, each as match.SeriesForms spells it), the
// community subtitle, clutter (clutterWords) and the numbers the book's positions
// state (statedNumbers) are taken out of it, nothing is left. Any other word or
// number left over is the files saying something the match doesn't (another book,
// another edition, volume 2 of a work that is volume 1), so the title stays. A title
// of theirs' words and nothing more only differs by its style, so it is replaced
// only when it has none: in one case ("the martian"), not "Blood and Gold".
func tidyTitle(st *catalog.MatchState, p catalog.MatchProposal) string {
	theirs, mine := p.Values[catalog.FieldTitle], st.Fields[catalog.FieldTitle]
	if mine.Source == catalog.SourceEdited || catalog.CleanOverride(catalog.FieldTitle, mine.Value) == theirs {
		return ""
	}
	left, ok := cut(titleWords(mine.Value), titleWords(theirs))
	if !ok {
		return "" // the community title isn't in it at all
	}
	if len(left) == 0 && strings.ToLower(mine.Value) != mine.Value && strings.ToUpper(mine.Value) != mine.Value {
		return ""
	}
	extras := []string{p.Values[catalog.FieldSubtitle], p.Values[catalog.FieldSeries],
		st.Fields[catalog.FieldSeries].Value, st.Fields[catalog.FieldSeries].Scanned}
	for _, more := range []string{p.Values[catalog.FieldMoreSeries], st.Fields[catalog.FieldMoreSeries].Value} {
		for _, s := range catalog.ParseMoreSeries(more) {
			extras = append(extras, s.Name)
		}
	}
	var forms [][]string
	for _, extra := range extras {
		for _, form := range match.SeriesForms(extra) {
			forms = append(forms, titleWords(form))
		}
	}
	// The longest first, so a series named inside another ("Mystic Knight" in
	// "The Mystic Knight Saga") is taken out whole; every occurrence, a series
	// named twice ("X: Y (X Book 1)").
	slices.SortStableFunc(forms, func(a, b []string) int { return len(b) - len(a) })
	for _, form := range forms {
		for found := true; found; {
			left, found = cut(left, form)
		}
	}
	stated := statedNumbers(st, p)
	for _, w := range left {
		if clutterWords[w] {
			continue
		}
		if n, ok := numberOf(w); !ok || !stated[n] {
			return ""
		}
	}
	return theirs
}

// statedNumbers is the numbers a book's file title may carry as clutter: its
// positions, the community's and its own, in every series (a decimal or a range
// states both of its numbers), and any number in the community title itself
// ("Primal Hunter 3").
func statedNumbers(st *catalog.MatchState, p catalog.MatchProposal) map[int]bool {
	stated := map[int]bool{}
	add := func(s string) {
		for _, w := range titleWords(s) {
			if n, ok := numberOf(w); ok {
				stated[n] = true
			}
		}
	}
	index := st.Fields[catalog.FieldSeriesIndex]
	for _, s := range []string{p.Values[catalog.FieldTitle], p.Values[catalog.FieldSeriesIndex], index.Value, index.Scanned} {
		add(s)
	}
	for _, more := range []string{p.Values[catalog.FieldMoreSeries], st.Fields[catalog.FieldMoreSeries].Value} {
		for _, s := range catalog.ParseMoreSeries(more) {
			add(catalog.FormatSeriesRange(s.Position, s.PositionEnd))
		}
	}
	return stated
}

// titleWords is s's words, lowercased: runs of letters, digits and the marks that
// spell them in any script ("काम" is one word), an apostrophe inside a word kept
// out ("Sharpe's" is "sharpes").
func titleWords(s string) []string {
	s = apostrophes.Replace(strings.ToLower(s))
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	})
}

var apostrophes = strings.NewReplacer("'", "", "’", "")

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

// numberWords are the numbers a title spells out.
var numberWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17,
	"eighteen": 18, "nineteen": 19, "twenty": 20,
}

// romanUnits are the roman numerals below ten, each at its value.
var romanUnits = []string{"", "i", "ii", "iii", "iv", "v", "vi", "vii", "viii", "ix"}

// numberOf reads w as a number: spelled out ("three"), digits ("8", "03"), digits
// with an ordinal ending ("1st", "2nd"), or a roman numeral up to xxxix ("ii",
// "iv", "xii"; a lone "i" counts, a word such as "mix" doesn't).
func numberOf(w string) (int, bool) {
	if n, ok := numberWords[w]; ok {
		return n, true
	}
	digits := strings.TrimRightFunc(w, unicode.IsLetter)
	switch w[len(digits):] {
	case "", "st", "nd", "rd", "th":
		if n, err := strconv.Atoi(digits); err == nil {
			return n, true
		}
	}
	units := strings.TrimLeft(w, "x")
	if tens := len(w) - len(units); tens <= 3 {
		if n := slices.Index(romanUnits, units); n >= 0 {
			return 10*tens + n, true
		}
	}
	return 0, false
}

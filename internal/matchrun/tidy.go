package matchrun

import (
	"regexp"
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

// File ordering is separate from series numbering. Require the spaced dash
// in "20 - Hilldiggers" so a number in a title or a range keeps its meaning.
var fileOrder = regexp.MustCompile(`^\s*[0-9]{1,3}\s+[-–]\s+`)

var numberLabels = map[string]bool{
	"book": true, "books": true, "bk": true, "vol": true, "vols": true, "volume": true, "volumes": true,
	"no": true, "nr": true, "number": true, "episode": true,
}

var clutterWords = map[string]bool{
	"unabridged": true, "edition": true, "series": true, "audiobook": true, "audio": true,
}

// Genre words can describe the title itself. Only a separate subtitle made
// entirely of these words, joining words and format labels is disposable.
var genreFluff = map[string]bool{
	"litrpg": true, "gamelit": true, "progression": true, "cultivation": true, "epic": true,
	"fantasy": true, "adventure": true, "novel": true, "novella": true,
}

// A delimiter after the work title introduces a subtitle or a catalogue note.
var titleBreak = regexp.MustCompile(`[:,(\[]|\s+[-–]\s+`)

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
	target := titleWords(theirs)
	file := titleWords(mine.Value)
	if slices.Equal(file, target) && strings.ToLower(mine.Value) != mine.Value && strings.ToUpper(mine.Value) != mine.Value {
		return ""
	}
	raw := mine.Value
	if order := fileOrder.FindString(raw); order != "" && len(target) > 0 && file[0] != target[0] {
		raw = raw[len(order):]
		file = file[1:]
	}
	at := phraseAt(file, target)
	if at < 0 {
		return ""
	}
	end := at + len(target)
	note := genreNoteStart(raw, file, end)
	clear(file[at:end])
	// A repeated part is disposable only beside the community title ending in that part.
	if n := len(target); n >= 2 && target[n-2] == "part" && end+2 <= len(file) && slices.Equal(file[end:end+2], target[n-2:]) {
		if _, ok := numberOf(target[n-1]); ok {
			clear(file[end : end+2])
		}
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
		for at := phraseAt(file, form); at >= 0; at = phraseAt(file, form) {
			clear(file[at : at+len(form)])
		}
	}
	stated := statedNumbers(st, p)
	for i, w := range file {
		switch {
		case w == "", clutterWords[w]:
		case numberLabels[w]:
			if i+1 >= len(file) || file[i+1] == "" {
				return ""
			}
			if n, ok := numberOf(file[i+1]); !ok || !stated[n] {
				return ""
			}
		case i >= note && (genreFluff[w] || w == "a" || w == "an" || w == "the" || w == "of"):
		default:
			if n, ok := numberOf(w); !ok || !stated[n] {
				return ""
			}
		}
	}
	return theirs
}

// genreNoteStart is the first word of a decoration after the matched title.
// Some stores append "A Progression Fantasy Adventure" without a delimiter.
func genreNoteStart(raw string, words []string, titleEnd int) int {
	if titleEnd+1 < len(words) && (words[titleEnd] == "a" || words[titleEnd] == "an") && genreFluff[words[titleEnd+1]] {
		return titleEnd
	}
	count := 0
	for _, segment := range titleBreak.Split(raw, -1) {
		count += len(titleWords(segment))
		if count >= titleEnd {
			return count
		}
	}
	return len(words)
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

// phraseAt locates a whole run of title words, or -1 for an absent or empty phrase.
func phraseAt(words, phrase []string) int {
	if len(phrase) > 0 {
		for i := 0; i+len(phrase) <= len(words); i++ {
			if slices.Equal(words[i:i+len(phrase)], phrase) {
				return i
			}
		}
	}
	return -1
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

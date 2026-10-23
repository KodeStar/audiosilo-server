package metadata

import (
	"regexp"
	"strconv"
	"strings"
)

// SplitSeriesTag reads a series tag that carries its own position, as
// Audible-sourced tooling writes the iTunes grouping ("Discworld, book #40"),
// into the series' name and the position: "Series, Book 3", "Series Book 3",
// "Series #3", "Series, #3.5", "Series (Book 3)", "Series, Vol. 2" (book, bk,
// vol, volume and no. in any case). Only a trailing, explicitly marked number set
// off from the name counts, so a name that merely ends in one keeps it
// ("Warhammer 40,000", "Metro 2033", "Learning C# 2"), and so does a name with no
// position ("Book of the New Sun"). A tag naming several series is read by its
// first when that one carries a position ("Discworld #40; Moist von Lipwig #3",
// as Audiobookshelf writes them, is Discworld at 40, as its metadata.json's first
// series is), and is otherwise whole: the last number is the last series' own. ok
// is false, and the tag is the name, when it isn't so.
func SplitSeriesTag(tag string) (series string, position float64, ok bool) {
	tag = strings.TrimSpace(tag)
	if first, _, several := strings.Cut(tag, ";"); several {
		if series, position, ok = splitSeriesTag(first); ok {
			return series, position, true
		}
		return tag, 0, false
	}
	return splitSeriesTag(tag)
}

// splitSeriesTag is SplitSeriesTag of one series.
func splitSeriesTag(tag string) (series string, position float64, ok bool) {
	tag = strings.TrimSpace(tag)
	m := seriesTagPlain.FindStringSubmatch(tag)
	if m == nil {
		m = seriesTagParen.FindStringSubmatch(tag)
	}
	if m == nil {
		return tag, 0, false
	}
	name := strings.TrimRight(m[1], " ,;:-([\u2013\u2014") // "Series (Book 3": a cut-off tag
	n, err := strconv.ParseFloat(m[2], 64)
	if name == "" || seriesTagMarker.MatchString(name) || seriesTagListed.MatchString(tag) ||
		err != nil || n <= 0 || n > MaxSeriesPosition {
		return tag, 0, false
	}
	return name, n, true
}

// seriesTagWords are the words that mark a volume in a series tag.
const seriesTagWords = `book|bk|vol(?:ume)?|no`

// seriesTagMark is a word for a volume (maybe with "." and "#"; "no" only with its
// "."), or a bare "#".
const seriesTagMark = `(?i:\b(?:(?:book|bk|vol(?:ume)?)\b\.?|no\.)\s*#?\s*|#\s*)`

// seriesTagPos is the marked position at the end of a series tag: the mark, then
// the number.
const seriesTagPos = seriesTagMark + `(\d{1,5}(?:\.\d+)?)`

var (
	// The mark is set off from the name by a character that is no letter or digit
	// in any script: RE2's \b is ASCII's, so "víno 3" would otherwise read "no 3",
	// and a "#" against a letter is a name's ("C# 2").
	seriesTagPlain = regexp.MustCompile(`^(.+?)[^\p{L}\p{N}]+` + seriesTagPos + `$`)
	seriesTagParen = regexp.MustCompile(`^(.+?)\s*[(\[]\s*` + seriesTagPos + `\s*[)\]]$`)
	// seriesTagMarker is a name that is only the marker ("Book #3"): no series.
	seriesTagMarker = regexp.MustCompile(`(?i)^(?:` + seriesTagWords + `)\.?$`)
	// seriesTagListed is a marked position followed by another series ("Discworld,
	// book #40, Rincewind, book #8"): the tag lists several.
	seriesTagListed = regexp.MustCompile(`(?:^|[^\p{L}\p{N}])` + seriesTagMark + `\d+(?:\.\d+)?\s*[)\]]?\s*[,;]`)
)

// setSeriesTag sets m's series from a series tag, and its position when the tag
// carries one (SplitSeriesTag); a later series-part tag still overrides it.
func (m *Metadata) setSeriesTag(tag string) {
	series, pos, ok := SplitSeriesTag(tag)
	m.Series = series
	if ok {
		m.SeriesIndex, m.SeriesIndexEnd = pos, 0
	}
}

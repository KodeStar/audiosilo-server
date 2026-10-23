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
// vol, volume and no. in any case). Only a trailing, explicitly marked number
// counts, so a name that merely ends in one keeps it ("Warhammer 40,000",
// "Metro 2033"), and so does a name with no position ("Book of the New Sun").
// ok is false, and the tag is the name, when it isn't so.
func SplitSeriesTag(tag string) (series string, position float64, ok bool) {
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
	if name == "" || seriesTagMarker.MatchString(name) || err != nil || n <= 0 || n > MaxSeriesPosition {
		return tag, 0, false
	}
	return name, n, true
}

// seriesTagPos is the marked position at the end of a series tag: a word for a
// volume (maybe with "." and "#") or a bare "#", then the number.
const seriesTagPos = `(?i:\b(?:book|bk|vol(?:ume)?|no)\b\.?\s*#?\s*|#\s*)(\d{1,5}(?:\.\d+)?)`

var (
	seriesTagPlain = regexp.MustCompile(`^(.+?)[\s,]*` + seriesTagPos + `$`)
	seriesTagParen = regexp.MustCompile(`^(.+?)\s*[(\[]\s*` + seriesTagPos + `\s*[)\]]$`)
	// seriesTagMarker is a name that is only the marker ("Book #3"): no series.
	seriesTagMarker = regexp.MustCompile(`(?i)^(?:book|bk|vol(?:ume)?|no)\.?$`)
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

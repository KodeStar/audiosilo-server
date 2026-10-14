package metadata

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// A series position can be a range: an omnibus of books 1 to 3 is "1-3" in its
// series. Its start is the position every client knows (series_index); its end
// is where the series continues after it.

const (
	// MaxSeriesPosition is the largest series position (a range's end included).
	MaxSeriesPosition = 100000
	// MaxSeriesSpan bounds a range (end - start): a box set of the first 100
	// books is the most an omnibus plausibly holds, and the slots a range covers
	// are counted one by one (the series positions a book holds).
	MaxSeriesSpan = 100
)

// ParseSeriesRange reads a series position that is a range: two numbers apart by
// a hyphen or an en dash, spaces around it allowed ("1-3", "1 - 3", "1–3",
// "2.5-4"). ok only for 0 < start < end <= MaxSeriesPosition with end - start <=
// MaxSeriesSpan; a single number is no range.
func ParseSeriesRange(s string) (start, end float64, ok bool) {
	s = strings.TrimSpace(s)
	for i, r := range s {
		if i == 0 || (r != '-' && r != '–') {
			continue
		}
		a, errA := strconv.ParseFloat(strings.TrimSpace(s[:i]), 64)
		b, errB := strconv.ParseFloat(strings.TrimSpace(s[i+utf8.RuneLen(r):]), 64)
		if errA != nil || errB != nil {
			continue // "1e-3-4": the dash inside a number isn't the separator
		}
		if ValidSeriesRange(a, b) {
			return a, b, true
		}
		return 0, 0, false
	}
	return 0, 0, false
}

// ValidSeriesRange reports whether start-end is a range a series position may
// hold: 0 < start < end <= MaxSeriesPosition, end - start <= MaxSeriesSpan.
// (NaN and the infinities fail these comparisons too.)
func ValidSeriesRange(start, end float64) bool {
	return start > 0 && end > start && end <= MaxSeriesPosition && end-start <= MaxSeriesSpan
}

// ParseSeriesPosition reads a series position: a number (ParseFloat's, end 0, as
// a series-part tag and a stored series_index were always read: no spaces) or a
// range (ParseSeriesRange: its start and end). ok is false for anything else.
func ParseSeriesPosition(s string) (start, end float64, ok bool) {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v, 0, true
	}
	return ParseSeriesRange(s)
}

package match

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Options turns on rules beyond Best's default, for a caller matching a store's
// full catalog (an Audible library) onto a library's own books, where both
// sides are mostly right but spelled differently. Best uses the zero value, so
// every other caller keeps the rules it was tuned on. ImportOptions turns them
// all on.
type Options struct {
	// SeriesMarker lets a wordy candidate title with no position column confirm
	// a bare query's number when the title names its place right after the
	// series: "<series>[ Series][,:-( ]Book N" ("Iron Will: Dragon Heart Series,
	// Book 2" is Dragon Heart #2). Confirm-grade only, never a veto.
	SeriesMarker bool
	// ExactTitle scores the same title (cased, spaced and punctuated alike,
	// bracketed groups and edition words aside) by the same person as a match,
	// even when the two sides file it under differently named series ("God's
	// Eye: Awakening" in "God's Eye" and in "Labyrinth World"), unless a side's
	// title is just its series name (and number), which every volume shares. It
	// adds to the pair's score, so an agreeing series and number still rank
	// same-titled candidates.
	ExactTitle bool
	// WordlessFallback falls back to the uncleaned title when the cleaned one has
	// no word left and no number ("The Mayor of Noobtown" in series "Mayor of
	// Noobtown" leaves "The").
	WordlessFallback bool
	// DropUnabridged removes "(Unabridged)" before a genre tail is judged, so
	// "The Primal Hunter 15: A LitRPG Adventure (Unabridged)" is volume 15.
	DropUnabridged bool
}

// ImportOptions is every opt-in rule: what the Bridge's Audible matcher uses.
func ImportOptions() Options {
	return Options{SeriesMarker: true, ExactTitle: true, WordlessFallback: true, DropUnabridged: true}
}

// ScoreASIN is the score BestWith reports for an ASIN match, above any score the
// title rules can reach.
const ScoreASIN = 10.0

// scoreExactTitle is Options.ExactTitle's bonus, added to the pair's own score:
// enough on its own to clear the threshold, so the identical title wins over a
// near one, while the series and number still rank two same-titled candidates.
const scoreExactTitle = 3.0

// unabridgedGroup is a bracketed "(Unabridged)" (Options.DropUnabridged).
var unabridgedGroup = regexp.MustCompile(`(?i)[\(\[]\s*unabridged\s*[\)\]]`)

// exactTitleKey is a title as Options.ExactTitle compares it: bracketed groups
// and the edition words (fluffWords, through replaceFluff) dropped, then Fold,
// so case, spacing and punctuation never matter and two titles in another
// script stay apart.
func exactTitleKey(title string) string {
	return Fold(replaceFluff(stripParenGroups(title)))
}

// afterSeriesBook is what Options.SeriesMarker reads right after a series name:
// an optional " Series", one separator, then "Book N". What follows N is
// checked by numberEnds.
var afterSeriesBook = regexp.MustCompile(`^(?:\s+series)?\s*[,:\-–(]?\s*book\s+(\d+(?:\.\d+)?)`)

// numberEnds reports whether a number read greedily ends at s's start: no
// letter or digit runs on ("Book 2.5x" is none), and no range follows ("Book
// 1-3" is an omnibus, not volume 1).
func numberEnds(s string) bool {
	r, n := utf8.DecodeRuneInString(s)
	if r == '-' || r == '–' {
		next, _ := utf8.DecodeRuneInString(s[n:])
		return !unicode.IsDigit(next)
	}
	return !isAlnumRune(r) && r != '_'
}

// seriesBookMarker is the "Book N" that follows a spelling of series (on word
// boundaries) in title, if any.
func seriesBookMarker(title, series string) (float64, bool) {
	lt := strings.ToLower(title)
	var v float64
	for _, form := range SeriesForms(series) {
		if boundedMatch(lt, strings.ToLower(form), func(end int) bool {
			m := afterSeriesBook.FindStringSubmatch(lt[end:])
			if m == nil || !numberEnds(lt[end+len(m[0]):]) {
				return false
			}
			var err error
			v, err = strconv.ParseFloat(m[1], 64)
			return err == nil
		}) {
			return v, true
		}
	}
	return 0, false
}

// seriesOnly reports whether candidate i's title is just its own series name
// (and number), which every volume of the series shares: Options.ExactTitle
// never matches on it, and under Options.WordlessFallback only its number may
// agree.
func (x *Index) seriesOnly(i int) bool {
	return x.series(i).seriesTrim != "" && x.own(i).bare
}

package match

import (
	"regexp"
	"strconv"
	"strings"
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
	// title is just its series name (and number), which every volume shares.
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

// scoreExactTitle is Options.ExactTitle's score: above every title-and-series
// score (2.5 at most), so the identical title wins over a near one.
const scoreExactTitle = 3.0

// unabridgedGroup is a bracketed "(Unabridged)" (Options.DropUnabridged).
var unabridgedGroup = regexp.MustCompile(`(?i)[\(\[]\s*unabridged\s*[\)\]]`)

// exactTitleKey is a title as Options.ExactTitle compares it: bracketed groups
// and the edition words (fluffWords) dropped, then Fold, so case, spacing and
// punctuation never matter and two titles in another script stay apart.
func exactTitleKey(title string) string {
	return Fold(fluffWords.ReplaceAllString(stripParenGroups(title), " "))
}

// afterSeriesBook is what Options.SeriesMarker reads right after a series name:
// an optional " Series", one separator, then "Book N".
var afterSeriesBook = regexp.MustCompile(`^(?:\s+series)?\s*[,:\-–(]?\s*book\s+(\d+(?:\.\d+)?)\b`)

// seriesBookMarker is the "Book N" that follows a spelling of series (on word
// boundaries) in title, if any.
func seriesBookMarker(title, series string) (float64, bool) {
	lt := strings.ToLower(title)
	for _, form := range seriesForms(series) {
		lf := strings.ToLower(form)
		for from := 0; ; {
			i := strings.Index(lt[from:], lf)
			if i < 0 {
				break
			}
			i += from
			end := i + len(lf)
			if boundedAt(lt, i, end) {
				if m := afterSeriesBook.FindStringSubmatch(lt[end:]); m != nil {
					if v, err := strconv.ParseFloat(m[1], 64); err == nil {
						return v, true
					}
				}
			}
			from = i + 1
		}
	}
	return 0, false
}

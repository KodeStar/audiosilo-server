package meta

import (
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// SearchPrefill is the text the match dialog's search box opens with: the book's
// title and author ("Sharpe's Eagle Bernard Cornwell"), as it always was, unless
// the tags look swapped or junk, where the folders say it better. The search
// itself never reads the box until the admin searches (the book's own facts,
// path and tags alike, find the candidates: matchParams), so this only decides
// what the admin sees and searches for next.
//
// The path's facts are the folder-name reading a library that prefers its
// folders takes (metadata.FromPathLayout: "Bernard Cornwell/Richard Sharpe/
// 08 - Sharpe's Eagle" is "Sharpe's Eagle" by Bernard Cornwell; a leaf it can't
// split, "Sharpe - 08 - Sharpe's Eagle", goes in as named, numbering and all,
// which metaserve reads as the matcher's own path guess does). They replace:
//
//   - both tags when they are swapped: the title is the author folder's name, or
//     the author, cleaned of series and numbering (match.CleanTitle), is the
//     path's title ("Bernard Cornwell" by "Sharpe's Eagle (Sharpe 08)");
//   - a junk title ("Track 01", "03": metadata.IsGenericTitle; "Unknown",
//     "Untitled") or a junk author ("Unknown", "Various Artists"), each alone.
//
// A fact the path doesn't give keeps the tag's value.
func SearchPrefill(title, author, series, relPath string, isFolder bool) string {
	title, author = strings.TrimSpace(title), strings.TrimSpace(author)
	p := metadata.FromPathLayout(relPath, isFolder)
	pathTitle := strings.TrimSpace(p.Title)
	if pathTitle == "" && p.Series != "" && p.SeriesIndex > 0 {
		// "Brandon Sanderson/Stormlight Archive/03": the series and its volume.
		pathTitle = p.Series + " " + strconv.FormatFloat(p.SeriesIndex, 'f', -1, 64)
	}
	swapped := (p.Author != "" && title != "" && match.Fold(title) == match.Fold(p.Author)) ||
		(pathTitle != "" && author != "" && match.Fold(match.CleanTitle(author, series)) == match.Fold(pathTitle))
	if pathTitle != "" && (swapped || junkTitle(title)) {
		title = pathTitle
	}
	if p.Author != "" && (swapped || junkAuthor(author)) {
		author = p.Author
	}
	return strings.TrimSpace(title + " " + author)
}

// junkTitle reports whether a title tag names no book.
func junkTitle(t string) bool {
	switch strings.ToLower(t) {
	case "unknown", "untitled", "unknown title", "unknown album", "no title":
		return true
	}
	return metadata.IsGenericTitle(t)
}

// junkAuthor reports whether an author tag names nobody.
func junkAuthor(a string) bool {
	switch strings.ToLower(a) {
	case "", "unknown", "unknown artist", "unknown author", "various", "various artists", "va":
		return true
	}
	return false
}

package metadata

import (
	"path/filepath"
	"strconv"
	"strings"
)

// PathLayout is what a book's library path says about it by its LAYOUT. Tags
// are often garbage where the folder layout is good ("Bernard Cornwell/Richard
// Sharpe/Sharpe - 08 - Sharpe's Eagle" for a file tagged title "Bernard
// Cornwell"), so the match dialog sends it beside the tags, and a library that
// prefers its folders takes its metadata from it (FromPathLayout).
type PathLayout struct {
	// Author is the top folder of a book at least one folder deep.
	Author string
	// Series is the folder holding the book, for a book at least two deep.
	Series string
	// Title is the leaf as it is named, numbering and all ("" when the leaf is
	// a part label): metaserve reads the numbering ("Sharpe - 08 - ", "SW06 -
	// ", "02. ") and takes the volume from it, so it is the one place that
	// parses a title's numbering.
	Title string
	// Position is the volume a bare-number leaf gives the series folder holding
	// it ("Brandon Sanderson/Stormlight Archive/03" is volume 3), "" otherwise.
	Position string
}

// ReadPathLayout reads the LAYOUT of a book's rel_path - which folder is the
// author, which the series, which the book - and nothing inside the names:
//
//   - a disc or track folder ("CD1", "Disc 2", "Track 01") is a PART of the book,
//     never its title, so trailing ones are dropped and the folder above names
//     the book (isPartLabel) - but never down to the top folder alone, which is
//     the author: "Peter Clines/14" keeps its author and guesses no title,
//     rather than guessing the author's name as one;
//   - a plain number dropped that way ("03", not "CD1" or "Part 3") is a
//     VOLUME, and the folder holding it the series rather than the book:
//     "Brandon Sanderson/Stormlight Archive/03" is volume 3 of the series, with
//     no title guess (the series name is not the book's);
//   - the top folder is the author whenever the book is at least one folder
//     deep ("George Orwell/1984"), and the folder holding the book is the series
//     only when there is an author folder above it.
//
// DeriveFromPath (the scan's baseline under the tags) also reads a book one
// folder deep's folder as its author, though it keeps the leaf's number as a
// position with no series; deeper, it takes the two folders nearest the book,
// disc and track folders included.
func ReadPathLayout(relPath string, isFolder bool) PathLayout {
	segs := strings.Split(strings.Trim(filepath.ToSlash(relPath), "/"), "/")
	if !isFolder {
		last := segs[len(segs)-1]
		segs[len(segs)-1] = strings.TrimSuffix(last, filepath.Ext(last))
	}
	dropped := ""
	for len(segs) > 2 && isPartLabel(segs[len(segs)-1]) {
		dropped = segs[len(segs)-1]
		segs = segs[:len(segs)-1]
	}
	var f PathLayout
	if len(segs) >= 2 {
		f.Author = segs[0]
	}
	if vol := plainVolume(dropped); vol != "" {
		f.Series, f.Position = segs[len(segs)-1], vol
		return f
	}
	if leaf := strings.TrimSpace(segs[len(segs)-1]); !isPartLabel(leaf) {
		f.Title = leaf
	}
	if len(segs) >= 3 {
		f.Series = segs[len(segs)-2]
	}
	return f
}

// FromPathLayout is the metadata a library that prefers its folders over the
// tags takes from a book's path (ReadPathLayout): the leaf's leading volume
// number ("03 - Abaddon's Gate") split off the title, a series folder's own
// numbering ("03 - Tawny Man") off the series name, and the position only
// alongside a series. A field left empty is one the path doesn't say, which
// the tags then fill.
func FromPathLayout(relPath string, isFolder bool) *Metadata {
	l := ReadPathLayout(relPath, isFolder)
	m := &Metadata{Author: strings.TrimSpace(l.Author)}
	idx, title := splitSeriesIndex(l.Title)
	m.Title = title
	if l.Series != "" {
		_, m.Series = splitSeriesIndex(l.Series)
		m.SeriesIndex = idx
		if l.Position != "" {
			m.SeriesIndex, _ = strconv.ParseFloat(l.Position, 64)
		}
	}
	return m
}

// plainVolume reads a part label that is a plain number ("03") as the volume
// it names ("3"); a label with a disc word ("CD1", "Part 3"), or a zero, is
// not one and gives "".
func plainVolume(seg string) string {
	n, err := strconv.Atoi(strings.TrimSpace(seg))
	if err != nil || n <= 0 || strings.ContainsAny(seg, "+-") {
		return ""
	}
	return strconv.Itoa(n)
}

// isPartLabel reports whether a path segment names a part of a book rather
// than the book: IsGenericTitle ("CD1", "Disc 2", "Track 01", "03"), except a
// bare number of four or more digits, which is a title ("1984") rather than a
// volume, a name with no digit at all, and a name with a letter outside ASCII
// (NamesNothing: "Война и мир", "Метро 2033").
func isPartLabel(seg string) bool {
	n := strings.TrimSpace(seg)
	if !strings.ContainsAny(n, "0123456789") || (len(n) >= 4 && strings.Trim(n, "0123456789") == "") {
		return false
	}
	return NamesNothing(seg)
}

package metadata

import (
	"cmp"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// TitleNotSeries is the title of a book whose tags gave title (md.Title: the
// album tag over the title tag, bookTitle), unless title is only the name of the
// book's series. Many libraries tag the series as the album and the book as the
// title ("Indian Hill" and "From The Ashes", in "Mark Tufo/Mark Tufo; Indian
// Hill; Book 04; From The Ashes"; "Without Law" and "Without Law 10", in "Eric
// Vall/Without Law/Book 10"), and the album then names every book of the
// series alike. So when title is the album, and the album names a series the
// book is in (the series tag, the folder holding the book, or the series a leaf
// named "Author; Series; Book NN[; Title]" states: LeafSeries), equal by series
// key with any bracketed edition note ("(Unabridged)") left out, the title is
// one of these that names something other than the series:
//
//   - the title tag (unless the book has several files and the tag is the album
//     and a track marker, "Indian Hill 03": a file's name, not the book's);
//   - the path's title (the leaf's title part, for the leaf above; else the
//     leaf with its volume number split off), unless it is only a volume marker
//     ("Book 10") or a part label.
//
// Neither counts when it is only bracketed ("(Part 1 of 2)") or the series and
// the number 1 ("Arena Road 1", "01 Monster Core": the first book, named for its
// series). When the two agree, that is the title. Otherwise the path's title
// is, for a book of several files (its title tag is its first file's), with no
// title tag that counts, or beside one carrying the series' name ("002 -
// Spellmonger - Book 002 - Warmage" beside "Book 2 - Warmage"). Failing that, a
// book at position 1 (the tags' position, else the leaf's volume number) keeps
// the album: a series is often named for its first book, whose title tag may
// then be a chapter's ("End Credits"); any other book takes the title tag. With
// neither, title stays. A book whose own leaf names it as the album does
// ("Frank Herbert/Dune/Dune": a series named for its first book) keeps the
// album: it IS the title.
func TitleNotSeries(title string, md *Metadata, relPath string, isFolder, severalFiles bool) string {
	if md == nil || title == "" || title != md.AlbumTag {
		return title
	}
	p := readSeriesPath(relPath, isFolder)
	series := append([]string{md.Series}, p.series...)
	if !namesAny(title, series) || sameName(title, p.title) {
		return title
	}
	valid := func(c string) bool {
		return strings.TrimSpace(bracketed.ReplaceAllString(c, "")) != "" && !IsGenericTitle(c) &&
			!volumeMarker(c) && !namesAny(c, series) && !seriesFirst(c, series)
	}
	tag, path := md.TitleTag, p.title
	if severalFiles && trackTail(md.AlbumTag, tag) {
		tag = ""
	}
	okTag, okPath := valid(tag), valid(path)
	switch {
	case okTag && okPath && sameName(tag, path):
		return tag
	case okPath && (severalFiles || !okTag || carriesName(tag, series)):
		return path
	case cmp.Or(md.SeriesIndex, p.position) == 1:
		return title
	case okTag:
		return tag
	}
	return title
}

// MaybeSeriesTitle reports whether a stored title and series could be a book's
// reading that TitleNotSeries would change (title names its series, or a series
// its path states, and its leaf doesn't name it so): a scan reads such a book's
// tags again to know (catalog.Signature.TitleCheck). It never reads the tags, so
// it says "maybe".
func MaybeSeriesTitle(relPath string, isFolder bool, title, series string) bool {
	if strings.TrimSpace(title) == "" {
		return false
	}
	p := readSeriesPath(relPath, isFolder)
	return namesAny(title, append([]string{series}, p.series...)) && !sameName(title, p.title)
}

// seriesPath is what a book's path says of its series and title for
// TitleNotSeries: the series it states (the folder holding the book, and a
// LeafSeries leaf's series part) and its title (that leaf's title part, ""
// without one; else DeriveFromPath's).
type seriesPath struct {
	series []string
	title  string
	// position is the leaf's volume number (DeriveFromPath's, else a LeafSeries
	// leaf's), 0 for none.
	position float64
}

func readSeriesPath(relPath string, isFolder bool) seriesPath {
	n := DeriveFromPath(relPath, isFolder)
	p := seriesPath{title: n.Title, position: n.SeriesIndex}
	if n.Series != "" {
		p.series = append(p.series, n.Series)
	}
	if s, t, v, ok := LeafSeries(leafName(relPath, isFolder)); ok {
		p.series = append(p.series, s)
		p.title, p.position = t, v
	}
	return p
}

// leafName is a book's own name in its path: the folder's, or the file's
// without its extension.
func leafName(relPath string, isFolder bool) string {
	name := filepath.Base(filepath.ToSlash(strings.Trim(relPath, "/")))
	if !isFolder {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	return name
}

// LeafSeries reads a book's leaf named "Author; Series; Book NN; Title" (the
// title part optional, the volume any marker after "Book": "Book 01", "Book
// 01-03", "Book XX"), as some library tools name a book's folder and file:
// "Mark Tufo; Indian Hill; Book 04; From The Ashes" is the series "Indian Hill",
// the title "From The Ashes" ("" with no title part) and the volume 4 (the
// number the marker leads with, 0 for none). ok is false for any other name; a
// title holding a semicolon is still read as the part after the volume.
func LeafSeries(leaf string) (series, title string, volume float64, ok bool) {
	parts := strings.Split(leaf, ";")
	if len(parts) < 3 {
		return "", "", 0, false
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	m := leafVolume.FindStringSubmatch(parts[2])
	if parts[0] == "" || parts[1] == "" || m == nil {
		return "", "", 0, false
	}
	volume, _ = strconv.ParseFloat(m[1], 64) // "" (Book XX): 0
	return parts[1], strings.Join(parts[3:], "; "), volume, true
}

// leafVolume is LeafSeries' volume part: "Book" and a marker, the number it
// leads with, if any, in submatch 1.
var leafVolume = regexp.MustCompile(`^(?i:book)\s+(\d+(?:\.\d+)?)?\S*$`)

// volumeMarker reports whether a name is only a volume marker, "Book 10",
// "Vol. 2", "#3" (splitSeriesIndex's prefixes and a number): no title.
func volumeMarker(name string) bool {
	return volumeOnly.MatchString(strings.TrimSpace(name))
}

var volumeOnly = regexp.MustCompile(`^(?i:book|vol\.?|volume|#)\s*\d+(?:\.\d+)?$`)

// trackTail reports whether a title tag is the album and a track marker ("Indian
// Hill 03", "Indian Hill - Chapter 2 - Bran"), as bookTitle reads one: a file's
// name in a book of several files.
func trackTail(album, title string) bool {
	ca, ct := canonTag(album), canonTag(title)
	if len(ct) <= len(ca) || !strings.HasPrefix(ct, ca) || startsAlnum(ct[len(ca):]) {
		return false
	}
	tail := strings.Trim(ct[len(ca):], " -:,;|.")
	return tail != "" && (IsGenericTitle(tail) || trackMarkerLead(tail))
}

// seriesFirst reports whether a name is one of the series and the number 1,
// either way round ("Arena Road 1", "Arena Road, Book 1", "01 Monster Core"):
// the first book, named for its series.
func seriesFirst(name string, series []string) bool {
	for _, s := range series {
		if rest, ok := afterName(name, s); ok && firstAfter.MatchString(rest) {
			return true
		}
		if m := firstBefore.FindStringSubmatch(name); m != nil && sameName(m[1], s) {
			return true
		}
	}
	return false
}

var (
	firstAfter  = regexp.MustCompile(`^[\s,:#.-]*(?i:(?:book|vol(?:ume)?\.?|part|no\.)\s*)?#?0*1[\s.]*$`)
	firstBefore = regexp.MustCompile(`^\s*(?i:(?:book|vol(?:ume)?\.?)\s*)?#?0*1(?:\s*[-:.]\s*|\s+)(.+)$`)
)

// carriesName reports whether a name holds one of the series' names (by series
// key) as part of something longer: "002 - Spellmonger - Book 002 - Warmage".
func carriesName(name string, series []string) bool {
	k := match.SeriesKey(name)
	for _, s := range series {
		if ks := match.SeriesKey(s); ks != "" && len(k) > len(ks) && strings.Contains(k, ks) {
			return true
		}
	}
	return false
}

// namesAny reports whether name is one of names (sameName).
func namesAny(name string, names []string) bool {
	for _, n := range names {
		if sameName(name, n) {
			return true
		}
	}
	return false
}

// sameName reports whether two names are one by series key (match.SameSeries:
// case, accents, punctuation and spacing aside), with bracketed groups such as
// an edition note ("Warbreaker (Unabridged)") left out.
func sameName(a, b string) bool {
	return match.SameSeries(strings.TrimSpace(bracketed.ReplaceAllString(a, " ")),
		strings.TrimSpace(bracketed.ReplaceAllString(b, " ")))
}

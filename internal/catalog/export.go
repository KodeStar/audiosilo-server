package catalog

import (
	"context"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Library export: a portable, filesystem-free list of the books a library holds.
//
// The file is meant to LEAVE the server - a user downloads it and uploads it to
// the community metadata site (meta.audiosilo.app) to mark which entries of a
// series they own. So it carries bibliographic facts only: titles, contributors,
// series position, identifiers, runtime. Never a path, size, codec, format or
// anything else that describes the filesystem or the install.
//
// The envelope is the shape the metadata site's importer already parses; treat
// it as a wire contract (additive changes only, `omitempty` everywhere).

const (
	// exportFormat/exportVersion identify the envelope to the importer.
	exportFormat  = "audiosilo-books"
	exportVersion = 1
	// exportSource names the producer, so the importer can tell a server export
	// from one written by another tool.
	exportSource = "audiosilo-server"

	// exportPageSize is how many books are pulled per keyset page while composing
	// the export (ListBooks caps a page at 200).
	exportPageSize = 200
)

// LibraryExport is the downloadable envelope: a library's book list, ready to be
// imported elsewhere.
type LibraryExport struct {
	Format        string        `json:"format"`
	Version       int           `json:"version"`
	Source        string        `json:"source"`
	ServerVersion string        `json:"server_version,omitempty"`
	Library       ExportLibrary `json:"library"`
	ExportedAt    string        `json:"exported_at"`
	Books         []ExportBook  `json:"books"`
}

// ExportLibrary identifies the exported library. Deliberately name + id only -
// the library's root path never leaves the server.
type ExportLibrary struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// ExportBook is one book as the importer sees it: bibliographic facts only.
// Every field but the title is optional and omitted when unknown. (The importer
// also accepts a `subtitle`, which the index has no column for; the envelope is
// additive, so that field appears here the day the index grows one.)
type ExportBook struct {
	Title          string   `json:"title"`
	Authors        []string `json:"authors,omitempty"`
	Narrators      []string `json:"narrators,omitempty"`
	Series         string   `json:"series,omitempty"`
	SeriesPosition string   `json:"series_position,omitempty"`
	ASIN           string   `json:"asin,omitempty"`
	ISBN           string   `json:"isbn,omitempty"`
	RuntimeMin     int      `json:"runtime_min,omitempty"`
	Chapters       int      `json:"chapters,omitempty"`
}

// Filename is the file name to offer the download as:
// audiosilo-<library-slug>-<YYYY-MM-DD>.json.
func (e *LibraryExport) Filename() string {
	date, _, _ := strings.Cut(e.ExportedAt, "T") // RFC3339: the date is what precedes the T
	slug := slugify(e.Library.Name)
	if slug == "" {
		slug = "library"
	}
	return "audiosilo-" + slug + "-" + date + ".json"
}

// ExportLibraryBooks composes a library's export envelope. It pages over the
// index with the normal keyset cursor (never OFFSET, which degrades on large
// libraries) and returns ErrNotFound when the library does not exist.
//
// serverVersion is passed in rather than read from the api package: catalog must
// not depend on the transport layer.
func (c *Catalog) ExportLibraryBooks(ctx context.Context, libraryID int64, serverVersion string) (*LibraryExport, error) {
	lib, err := c.GetLibrary(ctx, libraryID)
	if err != nil {
		return nil, err
	}
	out := &LibraryExport{
		Format:        exportFormat,
		Version:       exportVersion,
		Source:        exportSource,
		ServerVersion: serverVersion,
		Library:       ExportLibrary{ID: lib.ID, Name: lib.Name},
		ExportedAt:    c.now().UTC().Format(time.RFC3339),
		Books:         []ExportBook{},
	}

	counts, err := c.chapterCounts(ctx, libraryID)
	if err != nil {
		return nil, err
	}

	// seen collapses copies of the same book within the library (the same grouping
	// key search/"recently added" de-duplicate on), mapping the key to the entry
	// already emitted for it. A blank key means the metadata is too weak to merge
	// on, so those books are always kept.
	seen := map[string]int{}
	cursor := ""
	for {
		page, err := c.ListBooks(ctx, ListOptions{
			LibraryID: libraryID,
			Sort:      "title",
			Limit:     exportPageSize,
			Cursor:    cursor,
		})
		if err != nil {
			return nil, err
		}
		for i := range page.Books {
			b := &page.Books[i]
			eb := exportBook(b, counts[b.ID])
			if key := exposedDedupKey(*b); key != "" {
				if at, ok := seen[key]; ok {
					mergeExportBook(&out.Books[at], eb)
					continue
				}
				seen[key] = len(out.Books)
			}
			out.Books = append(out.Books, eb)
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			// An unchanged cursor would loop forever; stop rather than spin.
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// exportBook projects an indexed book onto the export shape. A multi-file book is
// one book here, exactly as it is one row in the index.
func exportBook(b *Book, chapters int) ExportBook {
	return ExportBook{
		Title:          strings.TrimSpace(b.Title),
		Authors:        splitNames(b.Author),
		Narrators:      splitNames(b.Narrator),
		Series:         strings.TrimSpace(b.Series),
		SeriesPosition: formatSeriesPosition(b.SeriesIndex),
		ASIN:           strings.TrimSpace(b.ASIN),
		ISBN:           strings.TrimSpace(b.ISBN),
		RuntimeMin:     runtimeMinutes(b.Duration),
		Chapters:       chapters,
	}
}

// mergeExportBook folds another copy of an already-exported book into the entry
// kept for it, filling only the facts that entry lacks. Copies are indexed
// independently, so the one that happens to sort first can be the sparsely-tagged
// rip (same asin, but no narrator/series tags) while the fully-tagged copy sorts
// later; taking each fact from whichever copy has it keeps the export as complete
// as the library is, without ever overwriting a fact already recorded.
func mergeExportBook(dst *ExportBook, src ExportBook) {
	if dst.Title == "" {
		dst.Title = src.Title
	}
	if len(dst.Authors) == 0 {
		dst.Authors = src.Authors
	}
	if len(dst.Narrators) == 0 {
		dst.Narrators = src.Narrators
	}
	if dst.Series == "" {
		dst.Series = src.Series
	}
	if dst.SeriesPosition == "" {
		dst.SeriesPosition = src.SeriesPosition
	}
	if dst.ASIN == "" {
		dst.ASIN = src.ASIN
	}
	if dst.ISBN == "" {
		dst.ISBN = src.ISBN
	}
	if dst.RuntimeMin == 0 {
		dst.RuntimeMin = src.RuntimeMin
	}
	if dst.Chapters == 0 {
		dst.Chapters = src.Chapters
	}
}

// chapterCounts returns the number of indexed chapters per book id for a whole
// library - one grouped query instead of loading every book's chapter rows (and
// instead of one query per export page; the export already holds every book of
// the library in memory, so a count per book id is strictly cheaper than that).
func (c *Catalog) chapterCounts(ctx context.Context, libraryID int64) (map[int64]int, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT c.book_id, COUNT(*) FROM chapters c
		 JOIN books b ON b.id = c.book_id
		 WHERE b.library_id = ? GROUP BY c.book_id`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// runtimeMinutes converts an indexed duration in seconds to whole minutes,
// rounded. Zero (unknown duration) stays zero so the field is omitted.
func runtimeMinutes(seconds float64) int {
	if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0
	}
	return int(math.Round(seconds / 60))
}

// formatSeriesPosition renders the float series index the way a reader writes it:
// "2" for 2.0, "2.5" for a novella between books. Zero means "no position" and
// renders as "" so the field is omitted.
func formatSeriesPosition(idx float64) string {
	if idx == 0 || math.IsNaN(idx) || math.IsInf(idx, 0) {
		return ""
	}
	return strconv.FormatFloat(idx, 'f', -1, 64)
}

// nameSeparatorRE matches the joiners that unambiguously separate two
// contributors in the single Author/Narrator string the index stores. A comma is
// NOT here: it is ambiguous ("Alexandre Dumas, pere") and is handled separately
// by splitOnCommas.
var nameSeparatorRE = regexp.MustCompile(`;|\s+&\s+|\s+and\s+`)

// splitNames turns the catalogue's single Author (or Narrator) string into a list
// of names, splitting ONLY where the string clearly holds several. Tag data is
// messy and a wrong split invents a person, so the rule is deliberately shy:
// unambiguous joiners always split; a comma splits only when every resulting part
// still looks like a full name (at least two words), which keeps suffixed names
// such as "Alexandre Dumas, pere" whole. Returns nil for a blank string.
func splitNames(s string) []string {
	var out []string
	for _, chunk := range cleanNameParts(nameSeparatorRE.Split(s, -1)) {
		out = append(out, splitOnCommas(chunk)...)
	}
	return out // nil when the string named nobody
}

// splitOnCommas splits one already-cleaned chunk on commas, but only when every
// piece has at least two words - otherwise the comma is part of a single name
// ("Dumas, pere"; "Doe, John") and the chunk is returned whole.
func splitOnCommas(p string) []string {
	pieces := cleanNameParts(strings.Split(p, ","))
	if len(pieces) < 2 {
		return []string{p}
	}
	for _, piece := range pieces {
		if len(strings.Fields(piece)) < 2 {
			return []string{p}
		}
	}
	return pieces
}

// cleanNameParts trims whitespace and dangling separator punctuation from BOTH
// ends of each part and drops the empties. Trailing: an "A, B, and C" split
// leaves a trailing comma. Leading: a half-empty "Last, First" tag arrives as
// ", Jane Doe", and splitOnCommas keeps such a chunk whole, so an untrimmed
// leading comma would otherwise be exported as part of the name.
func cleanNameParts(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimFunc(p, func(r rune) bool {
			return unicode.IsSpace(r) || r == ';' || r == '&' || r == ','
		})
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// slugify reduces a library name to a filename-safe slug: lowercase ASCII words
// joined by hyphens, with everything else dropped.
func slugify(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool {
		return r >= unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	return strings.ToLower(strings.Join(words, "-"))
}

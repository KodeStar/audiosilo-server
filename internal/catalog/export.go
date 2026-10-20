package catalog

import (
	"cmp"
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/names"
)

// Library export: a portable, filesystem-free list of the books a library holds.
//
// The file is meant to LEAVE the server - a user downloads it and uploads it to
// the community metadata site (meta.audiosilo.app) to mark which entries of a
// series they own. So by default it carries bibliographic facts only: titles,
// contributors, series position, identifiers, runtime, genres. Never by default
// a path, size, codec, format or anything else that describes the filesystem or
// the install, nor the book's tags: the file is made to be shared, a path names
// folders on the server (people's names, a share layout) and a tag is the
// owner's own note ("Owned", "Read"), neither of which its owner may mean to
// publish. Only an admin asking for them (ExportOptions, for their own scripts,
// which tie a row back to a book) gets each row's paths or tags, and the
// console warns them the file is then theirs alone. The description is opt-in
// too, for size rather than privacy: it is long and usually a publisher's
// blurb, so the file to share stays lean, and asking for it alone does not make
// the file a private one.
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
	// private is set when the export holds paths or tags (ExportOptions): its
	// file is named apart from the shareable one (Filename). A description is
	// not personal, so it never sets it.
	private bool
}

// ExportLibrary identifies the exported library. Deliberately name + id only -
// the library's root path never leaves the server.
type ExportLibrary struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// ExportBook is one book as the importer sees it: bibliographic facts only,
// unless the export was asked for paths, tags or descriptions. Every field but
// the title is optional and omitted when unknown. The importer reads subtitle,
// publisher, language and abridged and ignores keys it doesn't know (genres,
// series_list, published, description, tags, paths), so the envelope's version
// stays 1.
type ExportBook struct {
	Title          string   `json:"title"`
	Subtitle       string   `json:"subtitle,omitempty"`
	Authors        []string `json:"authors,omitempty"`
	Narrators      []string `json:"narrators,omitempty"`
	Series         string   `json:"series,omitempty"`
	SeriesPosition string   `json:"series_position,omitempty"`
	// SeriesList is every series the book is in, the main one (Series) first,
	// present exactly when the player's Book.SeriesList is: the book is in a
	// series beyond its main one. Series/SeriesPosition stay the main series.
	SeriesList []ExportSeries `json:"series_list,omitempty"`
	// Published is the book's publication date, YYYY[-MM[-DD]].
	Published   string   `json:"published,omitempty"`
	Description string   `json:"description,omitempty"` // only with ExportOptions.IncludeDescription
	Publisher   string   `json:"publisher,omitempty"`
	Language    string   `json:"language,omitempty"`
	Abridged    *bool    `json:"abridged,omitempty"` // absent when unknown
	Genres      []string `json:"genres,omitempty"`
	Tags        []string `json:"tags,omitempty"` // only with ExportOptions.IncludeTags
	ASIN        string   `json:"asin,omitempty"`
	ISBN        string   `json:"isbn,omitempty"`
	RuntimeMin  int      `json:"runtime_min,omitempty"`
	Chapters    int      `json:"chapters,omitempty"`
	// Paths tie the row back to the books it stands for: the path of every copy
	// that collapsed into it (index order; its own alone with
	// ExportOptions.SeparateCopies), in the envelope's library. Only with
	// ExportOptions.IncludePaths; absent otherwise.
	Paths []string `json:"paths,omitempty"`
}

// ExportSeries is one series a book is in, its position in the export's
// string form ("2", "2.5", "1-3" for a range; FormatSeriesRange), omitted when
// the book has none in that series.
type ExportSeries struct {
	Name     string `json:"name"`
	Position string `json:"position,omitempty"`
}

// ExportOptions is what an export holds beyond the bibliographic facts, and
// how its rows are made: for the admin's own scripts. Paths and tags are never
// for a file meant to be shared (they make it private, Filename).
type ExportOptions struct {
	IncludePaths       bool // each row's paths (ExportBook.Paths)
	IncludeTags        bool // each row's tags (ExportBook.Tags)
	IncludeDescription bool // each row's description (ExportBook.Description)
	// SeparateCopies makes one row per indexed book: copies of the same book
	// are not merged, so each row has its own path, tags and facts.
	SeparateCopies bool
}

// Filename is the file name to offer the download as:
// audiosilo-<library-slug>-<YYYY-MM-DD>.json, with "-private" before the
// extension for an export holding paths or tags, so that file is never taken
// for the one to share.
func (e *LibraryExport) Filename() string {
	date, _, _ := strings.Cut(e.ExportedAt, "T") // RFC3339: the date is what precedes the T
	slug := slugify(e.Library.Name)
	if slug == "" {
		slug = "library"
	}
	suffix := ""
	if e.private {
		suffix = "-private"
	}
	return "audiosilo-" + slug + "-" + date + suffix + ".json"
}

// ExportLibraryBooks composes a library's export envelope. It pages over the
// index with the normal keyset cursor (never OFFSET, which degrades on large
// libraries) and returns ErrNotFound when the library does not exist.
//
// serverVersion is passed in rather than read from the api package: catalog must
// not depend on the transport layer.
func (c *Catalog) ExportLibraryBooks(ctx context.Context, libraryID int64, serverVersion string, opt ExportOptions) (*LibraryExport, error) {
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
		private:       opt.IncludePaths || opt.IncludeTags,
	}

	counts, err := c.chapterCounts(ctx, libraryID)
	if err != nil {
		return nil, err
	}
	var descriptions map[int64]string
	if opt.IncludeDescription {
		if descriptions, err = c.descriptions(ctx, libraryID); err != nil {
			return nil, err
		}
	}

	// seen collapses copies of the same book within the library (the same grouping
	// key search/"recently added" de-duplicate on), mapping the key to the entry
	// already emitted for it. A blank key means the metadata is too weak to merge
	// on, so those books are always kept; with SeparateCopies nothing merges.
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
			if opt.IncludePaths {
				eb.Paths = []string{b.RelPath}
			}
			if opt.IncludeTags {
				eb.Tags = b.Tags
			}
			eb.Description = descriptions[b.ID] // nil map: ""
			if key := exposedDedupKey(*b); key != "" && !opt.SeparateCopies {
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
		Subtitle:       strings.TrimSpace(b.Subtitle),
		Authors:        names.Split(b.Author),
		Narrators:      names.Split(b.Narrator),
		Series:         strings.TrimSpace(b.Series),
		SeriesPosition: FormatSeriesRange(b.SeriesIndex, b.SeriesIndexEnd),
		SeriesList:     exportSeriesList(b.SeriesList),
		Published:      strings.TrimSpace(b.Published),
		Publisher:      strings.TrimSpace(b.Publisher),
		Language:       strings.TrimSpace(b.Language),
		Abridged:       b.Abridged,
		Genres:         b.Genres,
		ASIN:           strings.TrimSpace(b.ASIN),
		ISBN:           strings.TrimSpace(b.ISBN),
		RuntimeMin:     runtimeMinutes(b.Duration),
		Chapters:       chapters,
	}
}

// exportSeriesList projects a book's series list onto the export shape; nil
// (omitted) for a book with none.
func exportSeriesList(list []SeriesRef) []ExportSeries {
	if len(list) == 0 {
		return nil
	}
	out := make([]ExportSeries, len(list))
	for i, s := range list {
		out[i] = ExportSeries{Name: strings.TrimSpace(s.Name), Position: FormatSeriesRange(s.Position, s.PositionEnd)}
	}
	return out
}

// mergeExportBook folds another copy of an already-exported book into the entry
// kept for it, filling only the facts that entry lacks. Copies are indexed
// independently, so the one that happens to sort first can be the sparsely-tagged
// rip (same asin, but no narrator/series tags) while the fully-tagged copy sorts
// later; taking each fact from whichever copy has it keeps the export as complete
// as the library is, without ever overwriting a fact already recorded. The
// copy's paths are added to the entry's (an export with paths names every copy
// a row stands for), and likewise its tags (each copy's own notes; each label
// once, uniqueLabels), so a row holds what every copy it names was tagged.
func mergeExportBook(dst *ExportBook, src ExportBook) {
	dst.Paths = append(dst.Paths, src.Paths...)
	if len(src.Tags) > 0 {
		dst.Tags = uniqueLabels(slices.Concat(dst.Tags, src.Tags))
	}
	fillEmpty(&dst.Authors, src.Authors)
	fillEmpty(&dst.Narrators, src.Narrators)
	fillEmpty(&dst.Genres, src.Genres)
	fillEmpty(&dst.SeriesList, src.SeriesList)
	dst.Title = cmp.Or(dst.Title, src.Title)
	dst.Subtitle = cmp.Or(dst.Subtitle, src.Subtitle)
	dst.Series = cmp.Or(dst.Series, src.Series)
	dst.SeriesPosition = cmp.Or(dst.SeriesPosition, src.SeriesPosition)
	dst.Published = cmp.Or(dst.Published, src.Published)
	dst.Description = cmp.Or(dst.Description, src.Description)
	dst.Publisher = cmp.Or(dst.Publisher, src.Publisher)
	dst.Language = cmp.Or(dst.Language, src.Language)
	dst.Abridged = cmp.Or(dst.Abridged, src.Abridged)
	dst.ASIN = cmp.Or(dst.ASIN, src.ASIN)
	dst.ISBN = cmp.Or(dst.ISBN, src.ISBN)
	dst.RuntimeMin = cmp.Or(dst.RuntimeMin, src.RuntimeMin)
	dst.Chapters = cmp.Or(dst.Chapters, src.Chapters)
}

// fillEmpty is cmp.Or for a list: src fills *dst only when *dst is empty.
func fillEmpty[T any](dst *[]T, src []T) {
	if len(*dst) == 0 {
		*dst = src
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

// descriptions returns the trimmed description per described book id for a whole
// library in one query: ListBooks leaves the (long) description out, and a
// GetBook per book would be one query per row.
func (c *Catalog) descriptions(ctx context.Context, libraryID int64) (map[int64]string, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, description FROM books WHERE library_id = ? AND description != ''`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var d string
		if err := rows.Scan(&id, &d); err != nil {
			return nil, err
		}
		out[id] = strings.TrimSpace(d)
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

// FormatSeriesPosition renders the float series index the way a reader writes it:
// "2" for 2.0, "2.5" for a novella between books. Zero means "no position" and
// renders as "" so the field is omitted.
func FormatSeriesPosition(idx float64) string {
	if idx == 0 || math.IsNaN(idx) || math.IsInf(idx, 0) {
		return ""
	}
	return strconv.FormatFloat(idx, 'f', -1, 64)
}

// FormatSeriesRange renders a series position that may be a range: "1-3" for an
// omnibus of books 1 to 3 (end above start, metadata.ValidSeriesRange), else the
// start alone as FormatSeriesPosition renders it. The stored form of a range in
// the series_index field, and the export's series_position (the community
// importer's grammar takes a range).
func FormatSeriesRange(start, end float64) string {
	if !metadata.ValidSeriesRange(start, end) {
		return FormatSeriesPosition(start)
	}
	return FormatSeriesPosition(start) + "-" + FormatSeriesPosition(end)
}

// slugify reduces a library name to a filename-safe slug: lowercase ASCII words
// joined by hyphens, with everything else dropped.
func slugify(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool {
		return r >= unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	return strings.ToLower(strings.Join(words, "-"))
}

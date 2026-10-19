package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// Metadata overrides: an admin's edits to a book, stored path-keyed in
// book_overrides / chapter_overrides (no FK to the rebuildable index) and layered
// onto the books row. A books row always holds the EFFECTIVE values - what the scan
// found (the `scanned` column, then an Audiobookshelf metadata.json's values:
// abs_file), then any enrichment (asin/isbn), then any overrides -
// so every reader (players, search, export) sees edited values with no join.
// refreshEffective is the one place that layering happens; UpsertBook runs it in the
// same transaction as the scan's write, which is what makes an override a lock: a
// rescan rewrites the scanned values and immediately re-applies the edit.

// Overridable book fields (the wire names the console uses).
const (
	FieldTitle       = "title"
	FieldSubtitle    = "subtitle"
	FieldAuthor      = "author"
	FieldNarrator    = "narrator"
	FieldSeries      = "series"
	FieldSeriesIndex = "series_index"
	FieldPublished   = "published"
	FieldPublisher   = "publisher"
	FieldLanguage    = "language"
	// FieldAbridged is "true", "false", or "" when unknown (books.abridged NULL).
	FieldAbridged    = "abridged"
	FieldDescription = "description"
	FieldASIN        = "asin"
	FieldISBN        = "isbn"
)

// OverrideFields lists every overridable field, in display order.
var OverrideFields = []string{
	FieldTitle, FieldSubtitle, FieldAuthor, FieldNarrator, FieldSeries, FieldSeriesIndex, FieldMoreSeries,
	FieldPublished, FieldPublisher, FieldLanguage, FieldAbridged, FieldGenres, FieldTags,
	FieldDescription, FieldASIN, FieldISBN,
}

// Where a field's value came from. A scanned value is SourcePath when it is what
// the path yields (metadata.DeriveFromPath, or in a library that prefers its
// folders metadata.FromPathLayout) and SourceTag otherwise; an override is
// SourceEdited (typed by an admin) or SourceCommunity (accepted from a
// community-metadata match); an ASIN/ISBN attached by enrichment reads as
// SourceCommunity too (it was matched to an external record). A value an
// Audiobookshelf metadata.json in the book's folder gives is SourceABS (see
// overABS). "" means no value.
const (
	SourceTag       = "tag"
	SourcePath      = "path"
	SourceABS       = "abs"
	SourceEdited    = "edited"
	SourceCommunity = "community"
)

// ErrInvalidOverride marks an edit the catalog refuses (unknown field, a value that
// fails its field's rules, a chapter the book doesn't have). The transport layer
// maps it to 400; errors.As on *OverrideError gives the field and reason.
var ErrInvalidOverride = errors.New("invalid override")

// OverrideError is an ErrInvalidOverride naming the offending field.
type OverrideError struct {
	Field  string
	Reason string
}

func (e *OverrideError) Error() string { return e.Field + ": " + e.Reason }

// Is makes errors.Is(err, ErrInvalidOverride) true.
func (e *OverrideError) Is(target error) bool { return target == ErrInvalidOverride }

func invalid(field, reason string) error { return &OverrideError{Field: field, Reason: reason} }

// Value bounds. Generous: they exist so a single PATCH can't store megabytes, not
// to second-guess real titles.
const (
	maxShortField   = 500
	maxLanguage     = 64
	maxDescription  = 20000
	maxSeriesIndex  = metadata.MaxSeriesPosition
	maxChapterTitle = 500
)

var (
	asinRE      = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	isbnRE      = regexp.MustCompile(`^(\d{9}[\dX]|\d{13})$`)
	publishedRE = regexp.MustCompile(`^\d{4}(-\d{2}(-\d{2})?)?$`)
)

// normalizeOverride validates value for field and returns its stored form.
func normalizeOverride(field, value string) (string, error) {
	v := strings.TrimSpace(value)
	switch field {
	case FieldTitle, FieldAuthor, FieldNarrator, FieldSeries:
		if err := checkText(field, v, maxShortField); err != nil {
			return "", err
		}
		if field == FieldTitle && v == "" {
			return "", invalid(field, "a book needs a title")
		}
		return v, nil
	case FieldSubtitle, FieldPublisher:
		return v, checkText(field, v, maxShortField)
	case FieldLanguage:
		return v, checkText(field, v, maxLanguage)
	case FieldAbridged:
		switch v = strings.ToLower(v); v {
		case "", "true", "false":
			return v, nil
		}
		return "", invalid(field, `must be "true", "false" or empty`)
	case FieldGenres, FieldTags:
		return normalizeLabels(field, v)
	case FieldDescription:
		// A description keeps its line breaks (normalized to \n); any other
		// control character is refused.
		v = strings.ReplaceAll(v, "\r", "")
		if utf8.RuneCountInString(v) > maxDescription {
			return "", invalid(field, "too long")
		}
		if strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0 {
			return "", invalid(field, "contains control characters")
		}
		return v, nil
	case FieldSeriesIndex:
		if v == "" {
			return "", nil
		}
		if start, end, ok := metadata.ParseSeriesRange(v); ok {
			return FormatSeriesRange(start, end), nil // an omnibus: "1-3"
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || f < 0 || f > maxSeriesIndex {
			return "", invalid(field, "must be a number from 0 to 100000, or a range like 1-3")
		}
		return FormatSeriesPosition(f), nil
	case FieldMoreSeries:
		return normalizeMoreSeries(v)
	case FieldPublished:
		if v == "" {
			return "", nil
		}
		if !publishedRE.MatchString(v) || !validDatePrefix(v) {
			return "", invalid(field, "must be YYYY, YYYY-MM or YYYY-MM-DD")
		}
		return v, nil
	case FieldASIN:
		v = strings.ToUpper(v)
		if v != "" && !asinRE.MatchString(v) {
			return "", invalid(field, "must be 10 letters or digits")
		}
		return v, nil
	case FieldISBN:
		v = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(v))
		if v != "" && !isbnRE.MatchString(v) {
			return "", invalid(field, "must be an ISBN-10 or ISBN-13")
		}
		return v, nil
	default:
		return "", invalid(field, "not an editable field")
	}
}

func checkText(field, v string, limit int) error {
	if utf8.RuneCountInString(v) > limit {
		return invalid(field, "too long")
	}
	if strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return invalid(field, "contains control characters")
	}
	return nil
}

// validDatePrefix reports whether a YYYY[-MM[-DD]] string names a real date.
func validDatePrefix(v string) bool {
	layout := map[int]string{4: "2006", 7: "2006-01", 10: "2006-01-02"}[len(v)]
	_, err := time.Parse(layout, v)
	return err == nil
}

// bookFields is the overridable slice of a book, keyed by field name, with every
// value in its wire form (series_index included) so one map serves every field.
// A series index is rendered by FormatSeriesRange ("" for none, and for a
// non-finite value a tag can carry, which is no position either; "1-3" for a
// range).
type bookFields map[string]string

// fieldsOf reads a book's overridable fields. A label list (genres, tags) is
// cleaned as a file's is (CleanLabels): this is what a scan records.
func fieldsOf(b *Book) bookFields {
	return bookFields{
		FieldTitle: b.Title, FieldSubtitle: b.Subtitle, FieldAuthor: b.Author, FieldNarrator: b.Narrator,
		FieldSeries: b.Series, FieldSeriesIndex: FormatSeriesRange(b.SeriesIndex, b.SeriesIndexEnd),
		FieldPublished: b.Published, FieldPublisher: b.Publisher, FieldLanguage: b.Language,
		FieldAbridged: formatAbridged(b.Abridged), FieldGenres: EncodeLabels(CleanLabels(b.Genres)),
		FieldTags: EncodeLabels(CleanLabels(b.Tags)), FieldDescription: b.Description,
		FieldASIN: b.ASIN, FieldISBN: b.ISBN,
	}
}

// formatAbridged is an abridged flag in its field form: "true", "false", or ""
// when unknown (nil).
func formatAbridged(b *bool) string {
	switch {
	case b == nil:
		return ""
	case *b:
		return "true"
	}
	return "false"
}

// AbridgedClaim is a source's abridged flag (an Audiobookshelf metadata.json's,
// a community recording's) as the abridged field value it gives: "true", or ""
// (says nothing) for false. Both write false for every book nobody marked, so
// their false is also "not said", and taking it would claim a book unabridged.
func AbridgedClaim(abridged bool) string {
	if abridged {
		return "true"
	}
	return ""
}

// abridgedColumn is an abridged field value as books.abridged stores it: 1, 0,
// or NULL when unknown.
func abridgedColumn(v string) any {
	switch v {
	case "true":
		return 1
	case "false":
		return 0
	}
	return nil
}

// ParseSeriesIndex reads a stored series_index value as the position a client
// knows: the number, or a range's start ("1-3" is 1); 0 for none.
func ParseSeriesIndex(s string) float64 {
	start, _ := ParseSeriesPosition(s)
	return start
}

// ParseSeriesPosition reads a stored series_index value: its start (0 for none)
// and, for a range ("1-3", metadata.ParseSeriesPosition), its end (else 0).
func ParseSeriesPosition(s string) (start, end float64) {
	start, end, _ = metadata.ParseSeriesPosition(s)
	return start, end
}

// scannedStampKey is the `scanned` key holding the indexed_at of the upsert that
// wrote the snapshot (no field is named like it). A snapshot whose stamp is not the
// row's indexed_at was not written with the row's values: the row was indexed by a
// server that predates the column (an older release run against this database),
// which writes the scanned values to the row itself and leaves `scanned` blank or
// stale. loadLayers then takes the snapshot from the row, as migration 0016 did.
const scannedStampKey = "@indexed_at"

// scannedRevKey is the `scanned` key holding the revision of the path baseline
// (metadata.DeriveFromPath) the snapshot was read with, scannedRev today; a
// snapshot without it predates revision 1, when a lone folder above the book was
// read as its series rather than its author (see pathCheckExpr).
const (
	scannedRevKey = "@rev"
	scannedRev    = "1"
)

// scannedJSON encodes what the scan found for b (its fields before anything is
// layered on top) for the `scanned` column (encodeScanned), read with today's path
// baseline.
func scannedJSON(b *Book, indexedAt string) (string, error) {
	vals := fieldsOf(b)
	vals[scannedRevKey] = scannedRev
	return encodeScanned(vals, indexedAt)
}

// encodeScanned is a `scanned` value: field -> value, blanks left out, plus the
// stamp of the upsert it records (scannedStampKey).
func encodeScanned(fields bookFields, stamp string) (string, error) {
	vals := maps.Clone(fields)
	maps.DeleteFunc(vals, func(_, v string) bool { return v == "" })
	vals[scannedStampKey] = stamp
	raw, err := json.Marshal(vals)
	return string(raw), err
}

// parseScanned decodes a `scanned` value into its fields, its stamp and its path
// baseline's revision (scannedRevKey, "" before revision 1).
func parseScanned(raw string) (fields bookFields, stamp, rev string, err error) {
	out := bookFields{}
	if raw == "" {
		return out, "", "", nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, "", "", fmt.Errorf("decode scanned fields: %w", err)
	}
	stamp, rev = out[scannedStampKey], out[scannedRevKey]
	delete(out, scannedStampKey)
	delete(out, scannedRevKey)
	// Rows backfilled by migration 0016 cast series_index with SQL ("2.0", "0.0");
	// normalize so a revert writes the same form a scan would.
	if si, ok := out[FieldSeriesIndex]; ok {
		out[FieldSeriesIndex] = FormatSeriesRange(ParseSeriesPosition(si))
	}
	return out, stamp, rev, nil
}

// FieldValue is one overridable field as the console shows it: the effective
// value, where it came from, what the scan found (the revert target), and who
// last edited it.
type FieldValue struct {
	Value    string `json:"value"`
	Source   string `json:"source"`
	Scanned  string `json:"scanned"`
	Locked   bool   `json:"locked"`
	EditedBy string `json:"edited_by,omitempty"`
	EditedAt string `json:"edited_at,omitempty"`
}

// chapterOverrideMatch pairs a chapter_overrides row `co` with the chapters row
// `chapters` it renames: the same file (relative to the book, so a moved book keeps
// its renames) and the same start to the millisecond, never the same position, so a
// chapter list that shifts can't move a rename onto another chapter.
const chapterOverrideMatch = `co.file = (CASE WHEN chapters.file_path = co.path THEN ''
		ELSE substr(chapters.file_path, length(co.path) + 2) END)
	AND co.start_ms = CAST(ROUND(chapters.start * 1000) AS INTEGER)`

// chapterIdentity is what a chapter override is keyed on: the chapter at index idx
// of the book at bookPath, by its book-relative file and start. ok is false when the
// book has no such chapter.
func chapterIdentity(ctx context.Context, tx *sql.Tx, bookID int64, bookPath string, idx int) (file string, startMS int64, ok bool, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT file_path, CAST(ROUND(start * 1000) AS INTEGER) FROM chapters WHERE book_id = ? AND idx = ?`,
		bookID, idx).Scan(&file, &startMS)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	if file == bookPath {
		return "", startMS, true, nil
	}
	return strings.TrimPrefix(file, bookPath+"/"), startMS, true, nil
}

// storedOverride is one book_overrides row.
type storedOverride struct {
	Value     string
	Source    string
	UpdatedBy string // username; "" when the editor's account is gone
	UpdatedAt string
}

type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// querier is what both *store.DB and *sql.Tx offer for reads.
type querier interface {
	rowQuerier
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// pageBefore ends a newest-first keyset page read with LIMIT limit+1: the first
// limit items, and the id the next page reads below (0 when this is the last).
func pageBefore[T any](items []T, limit int, id func(T) int64) ([]T, int64) {
	if len(items) <= limit {
		return items, 0
	}
	items = items[:limit]
	return items, id(items[len(items)-1])
}

// queryRows runs query and scans every row into a T with scan. Never nil on
// success, so an empty result marshals as [].
func queryRows[T any](ctx context.Context, q rowQuerier, scan func(*sql.Rows, *T) error, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var v T
		if err := scan(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// bookIDByPath resolves a book's internal id from its identity. ErrNotFound when
// nothing is indexed at the path.
func bookIDByPath(ctx context.Context, q querier, libraryID int64, relPath string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT id FROM books WHERE library_id = ? AND rel_path = ?`, libraryID, relPath).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// nullableID is a user id for an updated_by column: NULL when there is no user (a
// system write), so the FK's SET NULL and "no editor" read the same.
func nullableID(id int64) any {
	if id > 0 {
		return id
	}
	return nil
}

// bookLayers is everything that decides a book's metadata: what the scan found,
// the enrichment attached to its path and its overrides.
type bookLayers struct {
	libID    int64
	path     string
	isFolder bool
	// preferPath is the library's MetadataFromPath: the folder layout's values
	// come before the scanned ones (see resolve).
	preferPath bool
	scanned    bookFields
	enrichment bookFields // asin/isbn only, blanks left out
	abs        bookFields // what an ABS metadata.json says (absFields), blanks left out
	overrides  map[string]storedOverride
	indexedAt  string // the row's
	// pathRev is the revision of the path baseline scanned was read with
	// (scannedRevKey): "" before revision 1, as for a snapshot read off the row.
	pathRev string
	// fromRow is set when the stored snapshot wasn't this row's (see
	// scannedStampKey), so scanned was read off the row; it holds the row's
	// indexed_at, for refreshEffective to stamp the snapshot it then records.
	fromRow string
}

func loadLayers(ctx context.Context, q querier, bookID int64) (*bookLayers, error) {
	l := &bookLayers{enrichment: bookFields{}}
	var raw, indexedAt, source, absRaw string
	var row Book
	if err := q.QueryRowContext(ctx,
		`SELECT b.library_id, b.rel_path, b.is_folder, b.scanned, b.indexed_at,
		        b.title, b.author, b.narrator, b.series, b.series_index, b.series_index_end, l.metadata_source,
		        b.abs_file
		   FROM books b JOIN libraries l ON l.id = b.library_id WHERE b.id = ?`, bookID).
		Scan(&l.libID, &l.path, &l.isFolder, &raw, &indexedAt,
			&row.Title, &row.Author, &row.Narrator, &row.Series, &row.SeriesIndex, &row.SeriesIndexEnd, &source,
			&absRaw); err != nil {
		return nil, err
	}
	l.preferPath = source == MetadataFromPath
	l.abs = parseABSFile(absRaw)
	scanned, stamp, rev, err := parseScanned(raw)
	if err != nil {
		return nil, err
	}
	if raw == "" || stamp != indexedAt {
		// Written by a server that predates the snapshot: the row holds what that
		// scan found (it applies no overrides; asin/isbn only ever come from
		// enrichment, so they stay out, as in migration 0016).
		scanned = fieldsOf(&row)
		maps.DeleteFunc(scanned, func(_, v string) bool { return v == "" })
		l.fromRow, rev = indexedAt, ""
	}
	l.pathRev = rev
	l.scanned, l.indexedAt = scanned, indexedAt
	var asin, isbn string
	err = q.QueryRowContext(ctx,
		`SELECT asin, isbn FROM book_enrichment WHERE library_id = ? AND path = ?`, l.libID, l.path).
		Scan(&asin, &isbn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	for field, v := range map[string]string{FieldASIN: asin, FieldISBN: isbn} {
		if v != "" {
			l.enrichment[field] = v
		}
	}
	if l.overrides, err = queryOverrides(ctx, q, l.libID, l.path); err != nil {
		return nil, err
	}
	return l, nil
}

// resolve layers the fields - what the scan found (the folder layout first in a
// library that prefers it, then an ABS metadata.json; see scannedFields), then
// enrichment, then the overrides -
// each with where its value came from. It is the single statement of that
// precedence: refreshEffective writes its values to the books row and the book page
// shows it as provenance, so the two can never disagree.
func (l *bookLayers) resolve() map[string]FieldValue {
	scanned, baseline, layout := l.scannedFields()
	out := make(map[string]FieldValue, len(OverrideFields))
	for _, field := range OverrideFields {
		fv := FieldValue{Value: scanned[field], Scanned: scanned[field]}
		switch fv.Value {
		case "":
		case l.abs[field]:
			fv.Source = SourceABS
		case baseline[field], layout[field]:
			fv.Source = SourcePath
		default:
			fv.Source = SourceTag
		}
		if v, ok := l.enrichment[field]; ok {
			fv.Value, fv.Source = v, SourceCommunity
		}
		if o, ok := l.overrides[field]; ok {
			fv.Value, fv.Source, fv.Locked, fv.EditedBy, fv.EditedAt = o.Value, o.Source, true, o.UpdatedBy, o.UpdatedAt
		}
		out[field] = fv
	}
	return out
}

// scannedFields is what the scan says for each field, with the path readings a
// value is told apart by (one equal to either is SourcePath, else a tag's; layout
// is nil outside a library that prefers its folders). The stored snapshot is the
// tags over the scan's path baseline (metadata.DeriveFromPath). A library that
// prefers its folders reads the path with the layout instead
// (metadata.FromPathLayout): its value goes over a tag's wherever it says anything,
// and replaces the baseline's own reading even with nothing ("Frank Herbert/Dune/CD1"
// names no series, where the baseline takes the book's folder for one); the title
// always keeps a value. The position goes with the series: the layout's
// own, else the scanned one only while the series it numbers stays (the same
// series, by any case), so a tag's position stays beside the tag's series even
// when it equals the leaf's number ("Brandon Sanderson/01 - The Way of Kings"
// tagged The Stormlight Archive #1), and goes with a series the layout replaced
// or dropped. An Audiobookshelf metadata.json's values then go over all of it
// (overABS).
//
// A series with no position takes the one its title gives
// (titlePosition), so the untagged copies of a numbered series still sit in it
// by number.
func (l *bookLayers) scannedFields() (scanned, baseline, layout bookFields) {
	scanned, baseline, layout = l.readScanned()
	scanned = l.overABS(scanned)
	scanned, baseline = titlePosition(scanned, baseline, layout)
	return scanned, baseline, layout
}

// titlePosition is scanned with the position its title gives
// (metadata.PositionInTitle: "He Who Fights with Monsters 4: A LitRPG Adventure"
// is #4 of He Who Fights With Monsters) when it has a series and no position,
// and baseline with it too when the title is the path's (so the position reads
// as the path's, else a tag's); both unchanged otherwise.
func titlePosition(scanned, baseline, layout bookFields) (bookFields, bookFields) {
	if scanned[FieldSeries] == "" || ParseSeriesIndex(scanned[FieldSeriesIndex]) != 0 {
		return scanned, baseline
	}
	p, ok := metadata.PositionInTitle(scanned[FieldTitle], scanned[FieldSeries])
	if !ok {
		return scanned, baseline
	}
	scanned = maps.Clone(scanned)
	scanned[FieldSeriesIndex] = FormatSeriesPosition(p)
	if t := scanned[FieldTitle]; t == baseline[FieldTitle] || (layout != nil && t == layout[FieldTitle]) {
		baseline = maps.Clone(baseline)
		baseline[FieldSeriesIndex] = scanned[FieldSeriesIndex]
	}
	return scanned, baseline
}

// readScanned is scannedFields before the title's position.
func (l *bookLayers) readScanned() (scanned, baseline, layout bookFields) {
	baseline = l.pathBaseline()
	if !l.preferPath {
		return l.scanned, baseline, nil
	}
	layout = pathFields(metadata.FromPathLayout(l.path, l.isFolder))
	if raw := metadata.ReadPathLayout(l.path, l.isFolder); raw.Title != "" &&
		strings.EqualFold(strings.TrimSpace(l.scanned[FieldTitle]), raw.Title) {
		// The tag title IS the folder's name, number and all ("13 Reasons Why"):
		// the number is part of the title, not a volume the layout can split off.
		layout[FieldTitle], layout[FieldSeriesIndex] = l.scanned[FieldTitle], raw.Position
	}
	scanned = maps.Clone(l.scanned)
	for _, field := range []string{FieldTitle, FieldAuthor, FieldSeries} {
		if v := layout[field]; v != "" || (field != FieldTitle && l.scanned[field] == baseline[field]) {
			scanned[field] = v
		}
	}
	if v := layout[FieldSeriesIndex]; v != "" {
		scanned[FieldSeriesIndex] = v
	} else if scanned[FieldSeries] == "" || !strings.EqualFold(scanned[FieldSeries], l.scanned[FieldSeries]) {
		scanned[FieldSeriesIndex] = ""
	}
	return scanned, baseline, layout
}

// pathBaseline is the scan's path baseline (metadata.DeriveFromPath) as the
// snapshot was read with it: before revision 1 (pathRev) a lone folder above the
// book was its series, not its author. So a snapshot the next scan hasn't put
// right yet (SetPathReading) still tells the path's values from a tag's, and a
// library that prefers its folders still drops that folder as a series.
func (l *bookLayers) pathBaseline() bookFields {
	b := pathFields(metadata.DeriveFromPath(l.path, l.isFolder))
	if l.pathRev == "" && strings.Count(l.path, "/") == 1 {
		b[FieldSeries], b[FieldAuthor] = b[FieldAuthor], ""
	}
	return b
}

// pathFields is the fields a path reading gives, in their stored form.
func pathFields(m *metadata.Metadata) bookFields {
	return bookFields{
		FieldTitle: m.Title, FieldAuthor: m.Author, FieldSeries: m.Series,
		FieldSeriesIndex: FormatSeriesRange(m.SeriesIndex, m.SeriesIndexEnd),
	}
}

// refreshEffective rebuilds a book's effective metadata (bookLayers.resolve) into
// its books row (with its series keys: series_key, book_more_series; and its
// labels: book_labels), its chapters (their source: applyChapterSource; then their
// titles), then refreshes its FTS row. UpsertBook,
// SetEnrichment and every edit call it, so the books row can never disagree with
// what the durable tables say.
func refreshEffective(ctx context.Context, tx *sql.Tx, bookID int64) error {
	l, err := loadLayers(ctx, tx, bookID)
	if err != nil {
		return err
	}
	if l.fromRow != "" {
		// Record the snapshot read off the row (recordScanned keeps the chapter
		// titles, which the reset below would otherwise blank). It was read with
		// the older baseline, so it gets no scannedRevKey.
		if err := recordScanned(ctx, tx, bookID, l, l.scanned); err != nil {
			return err
		}
	}
	fields := l.resolve()
	v := func(field string) string { return fields[field].Value }
	start, end := ParseSeriesPosition(v(FieldSeriesIndex))
	if _, err := tx.ExecContext(ctx,
		`UPDATE books SET title = ?, subtitle = ?, author = ?, narrator = ?, series = ?, series_key = ?, series_index = ?,
		     series_index_end = ?, more_series = ?, published = ?, publisher = ?, language = ?, abridged = ?,
		     genres = ?, tags = ?, description = ?, asin = ?, isbn = ?
		 WHERE id = ?`,
		v(FieldTitle), v(FieldSubtitle), v(FieldAuthor), v(FieldNarrator), v(FieldSeries), seriesKey(v(FieldSeries)), start,
		end, cmp.Or(v(FieldMoreSeries), "[]"), v(FieldPublished), v(FieldPublisher), v(FieldLanguage), abridgedColumn(v(FieldAbridged)),
		v(FieldGenres), v(FieldTags), v(FieldDescription), v(FieldASIN), v(FieldISBN), bookID); err != nil {
		return err
	}
	if err := keyOtherSeries(ctx, tx, bookID, l.libID, v(FieldSeries), v(FieldMoreSeries)); err != nil {
		return err
	}
	if err := writeLabels(ctx, tx, bookID, l.libID, v); err != nil {
		return err
	}

	if err := applyChapterSource(ctx, tx, bookID, l.libID, l.path); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE chapters SET title = scanned_title WHERE book_id = ? AND title <> scanned_title`, bookID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE chapters SET title = (SELECT co.title FROM chapter_overrides co
		     WHERE co.library_id = ?1 AND co.path = ?2 AND `+chapterOverrideMatch+`)
		 WHERE book_id = ?3 AND EXISTS(SELECT 1 FROM chapter_overrides co
		     WHERE co.library_id = ?1 AND co.path = ?2 AND `+chapterOverrideMatch+`)`,
		l.libID, l.path, bookID); err != nil {
		return err
	}

	// Refresh FTS: delete-then-insert keyed by rowid = book id, from the effective
	// values just written. The series column holds every series the book is in,
	// so a search finds it by any (the series= filter goes by key instead), and
	// the genres and tags columns a label a line, so a search finds a book by its
	// labels too (the genre= and tag= filters go by book_labels).
	if _, err := tx.ExecContext(ctx, `DELETE FROM books_fts WHERE rowid = ?`, bookID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO books_fts(rowid, title, author, series, narrator, genres, tags) VALUES(?,?,?,?,?,?,?)`,
		bookID, v(FieldTitle), v(FieldAuthor), ftsSeries(v(FieldSeries), v(FieldMoreSeries)), v(FieldNarrator),
		ftsLabels(v(FieldGenres)), ftsLabels(v(FieldTags)))
	return err
}

// recordScanned writes snap as book id's scanned snapshot, stamped with the
// row's indexed_at (l, its loaded layers), for refreshEffective to resolve
// next. When l's snapshot was read off an older server's row (l.fromRow), that
// server wrote the book's chapters too, with titles as scanned and no
// scanned_title: their titles are kept as the scanned ones, since a snapshot
// recorded here is the row's own and is never read off the row again.
func recordScanned(ctx context.Context, tx *sql.Tx, id int64, l *bookLayers, snap bookFields) error {
	if l.fromRow != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE chapters SET scanned_title = title WHERE book_id = ?`, id); err != nil {
			return err
		}
	}
	raw, err := encodeScanned(snap, l.indexedAt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE books SET scanned = ? WHERE id = ?`, raw, id)
	return err
}

func queryOverrides(ctx context.Context, q rowQuerier, libID int64, path string) (map[string]storedOverride, error) {
	type row struct {
		field string
		storedOverride
	}
	rows, err := queryRows(ctx, q, func(r *sql.Rows, o *row) error {
		return r.Scan(&o.field, &o.Value, &o.Source, &o.UpdatedBy, &o.UpdatedAt)
	}, `SELECT o.field, o.value, o.source, COALESCE(u.username, ''), o.updated_at
	      FROM book_overrides o LEFT JOIN users u ON u.id = o.updated_by
	     WHERE o.library_id = ? AND o.path = ?`, libID, path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]storedOverride, len(rows))
	for _, r := range rows {
		out[r.field] = r.storedOverride
	}
	return out, nil
}

// BookEdit is one change to a book's metadata: fields to override, fields to
// revert to what the scan found, and the same for chapter titles (by index).
// Source says where the set values came from (SourceEdited or SourceCommunity).
type BookEdit struct {
	Set    map[string]string
	Revert []string
	// Add and Remove change a label list (genres, tags) rather than set it: each
	// book's own effective list gains the labels Add names that it lacks and
	// loses those Remove names (any case), written as an override of the field
	// when that changes it (labelSets). A field is never both set or reverted and
	// added to or removed from.
	Add           map[string][]string
	Remove        map[string][]string
	ChapterSet    map[int]string
	ChapterRevert []int
	// ChapterSource picks where the chapters come from: ChaptersFromFiles,
	// ChaptersFromCommunity, or ChapterSourceAuto to leave it to the default;
	// "" changes nothing.
	ChapterSource string
	Source        string
	UserID        int64
}

// normalizeSource is where an edit or a custom cover came from, as stored:
// SourceEdited (also for "") or SourceCommunity.
func normalizeSource(source string) (string, error) {
	switch source {
	case "", SourceEdited:
		return SourceEdited, nil
	case SourceCommunity:
		return source, nil
	}
	return "", invalid("source", `must be "edited" or "community"`)
}

// normalize validates the edit and returns it with stored-form values.
func (e BookEdit) normalize() (BookEdit, error) {
	src, err := normalizeSource(e.Source)
	if err != nil {
		return e, err
	}
	e.Source = src
	set := make(map[string]string, len(e.Set))
	for field, v := range e.Set {
		nv, err := normalizeOverride(field, v)
		if err != nil {
			return e, err
		}
		set[field] = nv
	}
	for _, field := range e.Revert {
		if !slices.Contains(OverrideFields, field) {
			return e, invalid(field, "not an editable field")
		}
		if _, both := set[field]; both {
			return e, invalid(field, "cannot be set and reverted at once")
		}
	}
	chSet := make(map[int]string, len(e.ChapterSet))
	for idx, title := range e.ChapterSet {
		t := strings.TrimSpace(title)
		if t == "" {
			return e, invalid("chapters", "a chapter title cannot be empty (revert it instead)")
		}
		if err := checkText("chapters", t, maxChapterTitle); err != nil {
			return e, err
		}
		chSet[idx] = t
	}
	for _, idx := range e.ChapterRevert {
		if _, both := chSet[idx]; both {
			return e, invalid("chapters", "a chapter cannot be renamed and reverted at once")
		}
	}
	switch e.ChapterSource {
	case "", ChapterSourceAuto, ChaptersFromFiles, ChaptersFromCommunity:
	default:
		return e, invalid("chapter_source", `must be "auto", "files" or "community"`)
	}
	add, remove, err := normalizeLabelEdits(e)
	if err != nil {
		return e, err
	}
	e.Set, e.ChapterSet, e.Add, e.Remove = set, chSet, add, remove
	return e, nil
}

// EditBook applies an edit to one indexed book: overrides are written (or deleted,
// for a revert) and the books row, its chapters and its FTS row are rebuilt, all in
// one transaction. ErrNotFound when no book is indexed at the path.
func (c *Catalog) EditBook(ctx context.Context, libraryID int64, path string, edit BookEdit) error {
	edit, err := edit.normalize()
	if err != nil {
		return err
	}
	err = c.db.WithTx(ctx, "EditBook", func(tx *sql.Tx) error {
		return c.editTx(ctx, tx, Ref{LibraryID: libraryID, Path: path}, edit, c.ts())
	})
	if err == nil {
		c.changed()
	}
	return err
}

// EditBooks applies the same field edit to many books in one transaction: every
// book is edited or none is. Chapter edits are refused (chapter indexes are per
// book). ErrNotFound (wrapped with the path) when any of the books isn't indexed;
// an edit one of the books refuses (a series swap that can't be made) is an
// invalid override naming that book's path, so the admin can find it.
func (c *Catalog) EditBooks(ctx context.Context, refs []Ref, edit BookEdit) error {
	if len(edit.ChapterSet) > 0 || len(edit.ChapterRevert) > 0 {
		return invalid("chapters", "chapter titles can only be edited one book at a time")
	}
	edit, err := edit.normalize()
	if err != nil {
		return err
	}
	err = c.db.WithTx(ctx, "EditBooks", func(tx *sql.Tx) error {
		now := c.ts()
		for _, ref := range refs {
			if err := c.editTx(ctx, tx, ref, edit, now); err != nil {
				if oe, ok := errors.AsType[*OverrideError](err); ok {
					return invalid(oe.Field, oe.Reason+" ("+CleanRelPath(ref.Path)+")")
				}
				return err
			}
		}
		return nil
	})
	if err == nil {
		c.changed()
	}
	return err
}

// editTx writes one book's share of a normalized edit and rebuilds its row.
func (c *Catalog) editTx(ctx context.Context, tx *sql.Tx, ref Ref, edit BookEdit, now string) error {
	path := CleanRelPath(ref.Path)
	bookID, err := bookIDByPath(ctx, tx, ref.LibraryID, path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return err
	}
	editor := nullableID(edit.UserID)
	set, revert := edit.Set, edit.Revert
	swapSet, swapRevert, err := seriesSwap(ctx, tx, bookID, edit)
	if err != nil {
		return err
	}
	labels, err := labelSets(ctx, tx, bookID, edit)
	if err != nil {
		return err
	}
	// This book's own copies: EditBooks shares the edit across books. The swap
	// and the label changes write only fields the edit leaves alone, each either
	// set or reverted, so no field is both.
	if len(swapSet) > 0 || len(labels) > 0 {
		set = maps.Clone(edit.Set)
		maps.Copy(set, swapSet)
		maps.Copy(set, labels)
	}
	if len(swapRevert) > 0 {
		revert = slices.Concat(edit.Revert, swapRevert)
	}
	if len(set) == 0 && len(revert) == 0 && edit.ChapterSource == "" && len(edit.ChapterSet) == 0 && len(edit.ChapterRevert) == 0 {
		return nil // nothing for this book (a bulk add of a tag it already has): its row stays as it is
	}
	for field, v := range set {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO book_overrides(library_id, path, field, value, source, updated_by, updated_at)
			 VALUES(?,?,?,?,?,?,?)
			 ON CONFLICT(library_id, path, field) DO UPDATE SET
			     value = excluded.value, source = excluded.source,
			     updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			ref.LibraryID, path, field, v, edit.Source, editor, now); err != nil {
			return err
		}
	}
	// A community edit reverts only community values (a match clearing a series it
	// wrote earlier), never an admin's own edit made since it looked.
	del := `DELETE FROM book_overrides WHERE library_id = ? AND path = ? AND field = ?`
	if edit.Source == SourceCommunity {
		del += ` AND source = '` + SourceCommunity + `'`
	}
	for _, field := range revert {
		if _, err := tx.ExecContext(ctx, del, ref.LibraryID, path, field); err != nil {
			return err
		}
	}
	if edit.ChapterSource != "" {
		if err := setChapterChoice(ctx, tx, Ref{LibraryID: ref.LibraryID, Path: path}, edit.ChapterSource, editor, now); err != nil {
			return err
		}
	}
	for idx, title := range edit.ChapterSet {
		file, startMS, ok, err := chapterIdentity(ctx, tx, bookID, path, idx)
		if err != nil {
			return err
		}
		if !ok {
			return invalid("chapters", fmt.Sprintf("the book has no chapter %d", idx))
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chapter_overrides(library_id, path, file, start_ms, title, updated_by, updated_at)
			 VALUES(?,?,?,?,?,?,?)
			 ON CONFLICT(library_id, path, file, start_ms) DO UPDATE SET
			     title = excluded.title, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			ref.LibraryID, path, file, startMS, title, editor, now); err != nil {
			return err
		}
	}
	for _, idx := range edit.ChapterRevert {
		file, startMS, ok, err := chapterIdentity(ctx, tx, bookID, path, idx)
		if err != nil {
			return err
		}
		if !ok {
			continue // nothing at that index to revert
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM chapter_overrides WHERE library_id = ? AND path = ? AND file = ? AND start_ms = ?`,
			ref.LibraryID, path, file, startMS); err != nil {
			return err
		}
	}
	return refreshEffective(ctx, tx, bookID)
}

// seriesSwap is what an admin edit making one of a book's other series its main
// one (by its exact name) writes besides, so the old main series isn't lost and
// the new one isn't listed twice: the old main series takes the entry's place in
// more_series, at its position, and series_index becomes the entry's position
// unless the edit names series_index. The new main series is the one the edit
// sets, or, for an edit reverting series, the one the revert leaves (newMainSeries),
// so reverting a swap swaps back. The writes are ordinary overrides of the edit,
// so provenance, revert and undo treat them like any other; but a derived value
// equal to what its field resolves to without an override (the scan's or the
// folder layout's value; for series_index, only when the new main series is the
// series that value numbers) is a revert of that field, not an edit, so swapping
// back to a path-derived series #5 leaves series_index read off the path again
// (unlocked, source path) rather than an edited 5, while making a listed Other #5
// the main series of a book the path numbers Sherlock Holmes #5 still edits it.
// set and revert never share a field and never name one the edit sets or reverts
// itself. An edit naming more_series swaps nothing, and neither does a community
// edit: the match plan (matchrun.planSeries) lays out series itself. Both nil
// when there is nothing to swap; an invalid(FieldSeries) error when the old main
// series can't be listed (a name too long for an entry, a position out of range),
// so the edit is refused rather than losing it.
func seriesSwap(ctx context.Context, tx *sql.Tx, bookID int64, edit BookEdit) (set map[string]string, revert []string, err error) {
	if edit.Source == SourceCommunity || touches(edit, FieldMoreSeries) || !touches(edit, FieldSeries) {
		return nil, nil, nil
	}
	var main SeriesRef
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT series, series_index, series_index_end, more_series FROM books WHERE id = ?`, bookID).
		Scan(&main.Name, &main.Position, &main.PositionEnd, &stored); err != nil {
		return nil, nil, err
	}
	old := main.Name
	list := ParseMoreSeries(stored)
	if len(list) == 0 {
		return nil, nil, nil // nothing listed to swap in: no need to resolve the layers
	}
	var layers *bookLayers // loaded once, and only when needed
	load := func() (*bookLayers, error) {
		var err error
		if layers == nil {
			layers, err = loadLayers(ctx, tx, bookID)
		}
		return layers, err
	}
	name, err := newMainSeries(edit, load)
	if err != nil {
		return nil, nil, err
	}
	// The listed series the new main one is, in any spelling (seriesKey).
	at := slices.IndexFunc(list, func(s SeriesRef) bool { return match.SameSeries(s.Name, name) })
	if name == "" || match.SameSeries(name, old) || at < 0 {
		return nil, nil, nil
	}
	swapped := make([]SeriesRef, 0, len(list))
	for i, s := range list {
		switch {
		case i == at && old != "":
			swapped = append(swapped, main)
		case i == at, match.SameSeries(s.Name, old):
		default:
			swapped = append(swapped, s)
		}
	}
	more, err := normalizeOverride(FieldMoreSeries, EncodeMoreSeries(swapped))
	if err != nil {
		reason := err.Error()
		if oe, ok := errors.AsType[*OverrideError](err); ok {
			reason = oe.Reason
		}
		return nil, nil, invalid(FieldSeries, "the current series can't move to the other series: "+reason)
	}
	derived := map[string]string{FieldMoreSeries: more}
	if !touches(edit, FieldSeriesIndex) {
		derived[FieldSeriesIndex] = FormatSeriesRange(list[at].Position, list[at].PositionEnd)
	}
	l, err := load()
	if err != nil {
		return nil, nil, err
	}
	unedited := l.unedited(FieldSeries, FieldMoreSeries, FieldSeriesIndex)
	set = map[string]string{}
	for field, v := range derived {
		same := sameFieldValue(field, v, unedited[field].Value)
		if field == FieldSeriesIndex {
			// The unedited position numbers the unedited series: it is the
			// file's own for the new main series only when that is the same
			// series (in any spelling), not another series that happens to
			// share the number.
			same = same && match.SameSeries(name, unedited[FieldSeries].Value)
		}
		if same {
			revert = append(revert, field)
		} else {
			set[field] = v
		}
	}
	slices.Sort(revert) // map order: keep the writes deterministic
	return set, revert, nil
}

// sameFieldValue reports whether a and b, two values of field, say the same: a
// series position (a range's start and end) or an other series list compared as
// read (so "" and an empty list, or "5" and "5.0", agree), anything else exactly.
func sameFieldValue(field, a, b string) bool {
	switch field {
	case FieldSeriesIndex:
		as, ae := ParseSeriesPosition(a)
		bs, be := ParseSeriesPosition(b)
		return as == bs && ae == be
	case FieldMoreSeries:
		return slices.Equal(ParseMoreSeries(a), ParseMoreSeries(b))
	}
	return a == b
}

// SameFieldValue reports whether a and b, two values of field, say the same
// (sameFieldValue): the Bridge's meta.correct check compares an admin's
// corrected value with the book's edited one by it.
func SameFieldValue(field, a, b string) bool { return sameFieldValue(field, a, b) }

// newMainSeries is the main series edit leaves the book with when it sets or
// reverts series ("" when it does neither): the value it sets, or the series the
// book's layers (load) resolve to without its series override (what the scan or
// the folder layout found).
func newMainSeries(edit BookEdit, load func() (*bookLayers, error)) (string, error) {
	if name, ok := edit.Set[FieldSeries]; ok {
		return name, nil
	}
	if !slices.Contains(edit.Revert, FieldSeries) {
		return "", nil
	}
	l, err := load()
	if err != nil {
		return "", err
	}
	return l.unedited(FieldSeries)[FieldSeries].Value, nil
}

// unedited is what l resolves to with the overrides of fields left out: each
// such field's value as the scan, the folder layout or the enrichment has it.
// l itself is not changed. resolve reads each field's override on its own, so
// leaving several out at once gives each the value it has without its own.
func (l *bookLayers) unedited(fields ...string) map[string]FieldValue {
	c := *l
	c.overrides = maps.Clone(l.overrides)
	for _, f := range fields {
		delete(c.overrides, f)
	}
	return c.resolve()
}

// touches reports whether an edit sets or reverts field.
func touches(edit BookEdit, field string) bool {
	_, set := edit.Set[field]
	return set || slices.Contains(edit.Revert, field)
}

// titlePositionsKey is the server_state row saying PlaceByTitles has run.
const titlePositionsKey = "title_positions"

// PlaceByTitles re-resolves, once per database, the books whose title gives them
// a position in their series (titlePosition) while their row has none: books
// indexed before that rule, which a scan leaves alone while their files don't
// change (a book indexed since gets it from its own resolve). The rule reads the
// scanned title and series, which a match or an edit may have replaced on the
// row, so each book with a series, no position and a digit in its row's, its
// snapshot's or its path's title is resolved (read only) to see whether it now
// has one. It returns how many it placed; 0 once it has run (server_state
// titlePositionsKey).
func (c *Catalog) PlaceByTitles(ctx context.Context) (int, error) {
	if done, err := getServerState[bool](ctx, c.db, titlePositionsKey); err != nil || done {
		return 0, err
	}
	type row struct {
		id                    int64
		title, path, snapshot string
	}
	rows, err := queryRows(ctx, c.db, func(r *sql.Rows, b *row) error { return r.Scan(&b.id, &b.title, &b.path, &b.snapshot) },
		`SELECT id, title, rel_path, scanned FROM books WHERE series <> '' AND series_index = 0`)
	if err != nil {
		return 0, err
	}
	const digits = "0123456789"
	var ids []int64
	for _, b := range rows {
		scanned, _, _, _ := parseScanned(b.snapshot) // unreadable: the row's title and path still count
		if !strings.ContainsAny(b.title, digits) && !strings.ContainsAny(b.path, digits) &&
			!strings.ContainsAny(scanned[FieldTitle], digits) {
			continue
		}
		l, err := loadLayers(ctx, c.db, b.id)
		if errors.Is(err, sql.ErrNoRows) {
			continue // removed by a scan meanwhile
		} else if err != nil {
			return 0, err
		}
		if ParseSeriesIndex(l.resolve()[FieldSeriesIndex].Value) > 0 {
			ids = append(ids, b.id)
		}
	}
	placed := 0
	for len(ids) > 0 {
		batch := ids[:min(len(ids), 250)]
		ids = ids[len(batch):]
		if err := c.db.WithTx(ctx, "PlaceByTitles", func(tx *sql.Tx) error {
			for _, id := range batch {
				// A book a scan removed meanwhile has nothing to resolve.
				if err := refreshEffective(ctx, tx, id); errors.Is(err, sql.ErrNoRows) {
					continue
				} else if err != nil {
					return err
				}
				var idx float64
				if err := tx.QueryRowContext(ctx, `SELECT series_index FROM books WHERE id = ?`, id).Scan(&idx); err != nil {
					return err
				}
				if idx > 0 {
					placed++
				}
			}
			return nil
		}); err != nil {
			return 0, err
		}
	}
	return placed, putServerState(ctx, c.db, titlePositionsKey, true, c.stamp())
}

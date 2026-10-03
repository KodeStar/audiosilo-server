package catalog

import (
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
)

// Metadata overrides: an admin's edits to a book, stored path-keyed in
// book_overrides / chapter_overrides (no FK to the rebuildable index) and layered
// onto the books row. A books row always holds the EFFECTIVE values - what the scan
// found (the `scanned` column), then any enrichment (asin/isbn), then any overrides -
// so every reader (players, search, export) sees edited values with no join.
// refreshEffective is the one place that layering happens; UpsertBook runs it in the
// same transaction as the scan's write, which is what makes an override a lock: a
// rescan rewrites the scanned values and immediately re-applies the edit.

// Overridable book fields (the wire names the console uses).
const (
	FieldTitle       = "title"
	FieldAuthor      = "author"
	FieldNarrator    = "narrator"
	FieldSeries      = "series"
	FieldSeriesIndex = "series_index"
	FieldPublished   = "published"
	FieldDescription = "description"
	FieldASIN        = "asin"
	FieldISBN        = "isbn"
)

// OverrideFields lists every overridable field, in display order.
var OverrideFields = []string{
	FieldTitle, FieldAuthor, FieldNarrator, FieldSeries, FieldSeriesIndex,
	FieldPublished, FieldDescription, FieldASIN, FieldISBN,
}

// Where a field's value came from. A scanned value is SourcePath when it is what
// the path yields (metadata.DeriveFromPath) and SourceTag otherwise; an override is
// SourceEdited (typed by an admin) or SourceCommunity (accepted from a
// community-metadata match); an ASIN/ISBN attached by enrichment reads as
// SourceCommunity too (it was matched to an external record). "" means no value.
const (
	SourceTag       = "tag"
	SourcePath      = "path"
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
	maxDescription  = 20000
	maxSeriesIndex  = 100000
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
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || f < 0 || f > maxSeriesIndex {
			return "", invalid(field, "must be a number from 0 to 100000")
		}
		return formatSeriesPosition(f), nil
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
// A series index is rendered by formatSeriesPosition ("" for none, and for a
// non-finite value a tag can carry, which is no position either).
type bookFields map[string]string

// fieldsOf reads a book's overridable fields.
func fieldsOf(b *Book) bookFields {
	return bookFields{
		FieldTitle: b.Title, FieldAuthor: b.Author, FieldNarrator: b.Narrator,
		FieldSeries: b.Series, FieldSeriesIndex: formatSeriesPosition(b.SeriesIndex),
		FieldPublished: b.Published, FieldDescription: b.Description,
		FieldASIN: b.ASIN, FieldISBN: b.ISBN,
	}
}

func parseSeriesIndex(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// scannedStampKey is the `scanned` key holding the indexed_at of the upsert that
// wrote the snapshot (no field is named like it). A snapshot whose stamp is not the
// row's indexed_at was not written with the row's values: the row was indexed by a
// server that predates the column (an older release run against this database),
// which writes the scanned values to the row itself and leaves `scanned` blank or
// stale. loadLayers then takes the snapshot from the row, as migration 0016 did.
const scannedStampKey = "@indexed_at"

// scannedJSON encodes what the scan found for b (its fields before anything is
// layered on top) for the `scanned` column: field -> value, blanks left out, plus
// the upsert's indexedAt stamp.
func scannedJSON(b *Book, indexedAt string) (string, error) {
	vals := fieldsOf(b)
	maps.DeleteFunc(vals, func(_, v string) bool { return v == "" })
	vals[scannedStampKey] = indexedAt
	raw, err := json.Marshal(vals)
	return string(raw), err
}

// parseScanned decodes a `scanned` value into its fields and its stamp.
func parseScanned(raw string) (bookFields, string, error) {
	out := bookFields{}
	if raw == "" {
		return out, "", nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, "", fmt.Errorf("decode scanned fields: %w", err)
	}
	stamp := out[scannedStampKey]
	delete(out, scannedStampKey)
	// Rows backfilled by migration 0016 cast series_index with SQL ("2.0", "0.0");
	// normalize so a revert writes the same form a scan would.
	if si, ok := out[FieldSeriesIndex]; ok {
		out[FieldSeriesIndex] = formatSeriesPosition(parseSeriesIndex(si))
	}
	return out, stamp, nil
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
	libID      int64
	path       string
	isFolder   bool
	scanned    bookFields
	enrichment bookFields // asin/isbn only, blanks left out
	overrides  map[string]storedOverride
	// fromRow is set when the stored snapshot wasn't this row's (see
	// scannedStampKey), so scanned was read off the row; it holds the row's
	// indexed_at, for refreshEffective to stamp the snapshot it then records.
	fromRow string
}

func loadLayers(ctx context.Context, q querier, bookID int64) (*bookLayers, error) {
	l := &bookLayers{enrichment: bookFields{}}
	var raw, indexedAt string
	var row Book
	if err := q.QueryRowContext(ctx,
		`SELECT library_id, rel_path, is_folder, scanned, indexed_at,
		        title, author, narrator, series, series_index
		   FROM books WHERE id = ?`, bookID).
		Scan(&l.libID, &l.path, &l.isFolder, &raw, &indexedAt,
			&row.Title, &row.Author, &row.Narrator, &row.Series, &row.SeriesIndex); err != nil {
		return nil, err
	}
	scanned, stamp, err := parseScanned(raw)
	if err != nil {
		return nil, err
	}
	if raw == "" || stamp != indexedAt {
		// Written by a server that predates the snapshot: the row holds what that
		// scan found (it applies no overrides; asin/isbn only ever come from
		// enrichment, so they stay out, as in migration 0016).
		scanned = fieldsOf(&row)
		maps.DeleteFunc(scanned, func(_, v string) bool { return v == "" })
		l.fromRow = indexedAt
	}
	l.scanned = scanned
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

// resolve layers the fields - what the scan found, then enrichment, then the
// overrides - each with where its value came from. It is the single statement of
// that precedence: refreshEffective writes its values to the books row and the book
// page shows it as provenance, so the two can never disagree.
func (l *bookLayers) resolve() map[string]FieldValue {
	derived := metadata.DeriveFromPath(l.path, l.isFolder)
	fromPath := bookFields{
		FieldTitle: derived.Title, FieldAuthor: derived.Author, FieldSeries: derived.Series,
		FieldSeriesIndex: formatSeriesPosition(derived.SeriesIndex),
	}
	out := make(map[string]FieldValue, len(OverrideFields))
	for _, field := range OverrideFields {
		fv := FieldValue{Value: l.scanned[field], Scanned: l.scanned[field]}
		switch fv.Value {
		case "":
		case fromPath[field]:
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

// refreshEffective rebuilds a book's effective metadata (bookLayers.resolve) into
// its books row and chapter titles, then refreshes its FTS row. UpsertBook,
// SetEnrichment and every edit call it, so the books row can never disagree with
// what the durable tables say.
func refreshEffective(ctx context.Context, tx *sql.Tx, bookID int64) error {
	l, err := loadLayers(ctx, tx, bookID)
	if err != nil {
		return err
	}
	if l.fromRow != "" {
		// Record the snapshot read off the row, and likewise the chapter titles: the
		// older server that indexed the row also wrote its chapters, with titles as
		// scanned and no scanned_title, so the reset below would blank them.
		snap := maps.Clone(l.scanned)
		snap[scannedStampKey] = l.fromRow
		raw, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE books SET scanned = ? WHERE id = ?`, string(raw), bookID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE chapters SET scanned_title = title WHERE book_id = ?`, bookID); err != nil {
			return err
		}
	}
	fields := l.resolve()
	v := func(field string) string { return fields[field].Value }
	if _, err := tx.ExecContext(ctx,
		`UPDATE books SET title = ?, author = ?, narrator = ?, series = ?, series_index = ?,
		     published = ?, description = ?, asin = ?, isbn = ?
		 WHERE id = ?`,
		v(FieldTitle), v(FieldAuthor), v(FieldNarrator), v(FieldSeries), parseSeriesIndex(v(FieldSeriesIndex)),
		v(FieldPublished), v(FieldDescription), v(FieldASIN), v(FieldISBN), bookID); err != nil {
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
	// values just written.
	if _, err := tx.ExecContext(ctx, `DELETE FROM books_fts WHERE rowid = ?`, bookID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO books_fts(rowid, title, author, series, narrator) VALUES(?,?,?,?,?)`,
		bookID, v(FieldTitle), v(FieldAuthor), v(FieldSeries), v(FieldNarrator))
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
	Set           map[string]string
	Revert        []string
	ChapterSet    map[int]string
	ChapterRevert []int
	Source        string
	UserID        int64
}

// normalize validates the edit and returns it with stored-form values.
func (e BookEdit) normalize() (BookEdit, error) {
	if e.Source == "" {
		e.Source = SourceEdited
	}
	if e.Source != SourceEdited && e.Source != SourceCommunity {
		return e, invalid("source", `must be "edited" or "community"`)
	}
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
	e.Set, e.ChapterSet = set, chSet
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
	return c.db.WithTx(ctx, "EditBook", func(tx *sql.Tx) error {
		return c.editTx(ctx, tx, Ref{LibraryID: libraryID, Path: path}, edit, c.ts())
	})
}

// EditBooks applies the same field edit to many books in one transaction: every
// book is edited or none is. Chapter edits are refused (chapter indexes are per
// book). ErrNotFound (wrapped with the path) when any of the books isn't indexed.
func (c *Catalog) EditBooks(ctx context.Context, refs []Ref, edit BookEdit) error {
	if len(edit.ChapterSet) > 0 || len(edit.ChapterRevert) > 0 {
		return invalid("chapters", "chapter titles can only be edited one book at a time")
	}
	edit, err := edit.normalize()
	if err != nil {
		return err
	}
	return c.db.WithTx(ctx, "EditBooks", func(tx *sql.Tx) error {
		now := c.ts()
		for _, ref := range refs {
			if err := c.editTx(ctx, tx, ref, edit, now); err != nil {
				return err
			}
		}
		return nil
	})
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
	for field, v := range edit.Set {
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
	for _, field := range edit.Revert {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM book_overrides WHERE library_id = ? AND path = ? AND field = ?`,
			ref.LibraryID, path, field); err != nil {
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

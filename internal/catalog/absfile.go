package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// An Audiobookshelf metadata.json in a folder book's folder (metadata.ABSFile) is
// a layer of the book's metadata over its tags and path: the record ABS keeps
// for the book, its matches and its users' corrections included, which ABS
// itself ranks above the tags. books.abs_file holds what it says, normalized
// (absFields), beside the file's signature (abs_sig) and the indexed_at it was
// read with (abs_indexed_at); bookLayers.overABS lays it over the scanned values,
// and an edit or a community match still goes over it.

// ABSReading is a folder book's ABS file as a scan found it: the file's signature
// ("" for none; metadata.ABSSignature) and what it says (nil: no file, or it
// couldn't be read).
type ABSReading struct {
	Sig  string
	File *metadata.ABSFile
}

// absFields is what an ABS file says as stored field values: each as an edit of
// that field would store it, a value the field refuses left out (and a label the
// list refuses, CleanLabels). The first series is the book's, with its
// position; the others are its more_series. abridged is AbridgedClaim's.
func absFields(f *metadata.ABSFile) bookFields {
	out := bookFields{}
	if f == nil {
		return out
	}
	set := func(field, v string) {
		if v = CleanOverride(field, v); v != "" {
			out[field] = v
		}
	}
	set(FieldTitle, f.Title)
	set(FieldSubtitle, f.Subtitle)
	set(FieldAuthor, f.Author)
	set(FieldNarrator, f.Narrator)
	set(FieldPublished, f.Published)
	set(FieldPublisher, f.Publisher)
	set(FieldLanguage, f.Language)
	set(FieldAbridged, AbridgedClaim(f.Abridged))
	set(FieldGenres, EncodeLabels(CleanLabels(f.Genres)))
	set(FieldTags, EncodeLabels(CleanLabels(f.Tags)))
	set(FieldDescription, f.Description)
	set(FieldASIN, f.ASIN)
	set(FieldISBN, f.ISBN)
	if len(f.Series) > 0 {
		set(FieldSeries, f.Series[0].Name)
	}
	if out[FieldSeries] == "" {
		return out
	}
	set(FieldSeriesIndex, f.Series[0].Position)
	more := make([]SeriesRef, 0, len(f.Series)-1)
	for _, s := range f.Series[1:] {
		more = append(more, SeriesAt(s.Name, CleanOverride(FieldSeriesIndex, s.Position)))
	}
	set(FieldMoreSeries, EncodeMoreSeries(more))
	return out
}

// encodeABSFields is a books.abs_file value ("" for no fields).
func encodeABSFields(fields bookFields) (string, error) {
	if len(fields) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(fields)
	return string(raw), err
}

// parseABSFile decodes a books.abs_file value; one that doesn't decode says
// nothing.
func parseABSFile(raw string) bookFields {
	out := bookFields{}
	if raw == "" || json.Unmarshal([]byte(raw), &out) != nil {
		return bookFields{}
	}
	return out
}

// absColumns is what UpsertBook writes to books.abs_sig, abs_indexed_at and
// abs_file for b: its ABS reading for a folder book; nothing, and nothing to read,
// for a file book (ABS keeps a metadata.json only in a book's own folder).
func absColumns(b *Book, indexedAt string) (sig string, readAt any, file string, err error) {
	if !b.IsFolder {
		return "", nil, "", nil
	}
	file, err = encodeABSFields(absFields(b.ABS.File))
	return b.ABS.Sig, indexedAt, file, err
}

// absUncheckedExpr is a books row's Signature.ABSUnchecked: a folder book whose
// metadata.json wasn't read since the columns existed (NULL), or was read for
// another index of the row (an older server re-indexed it, keeping them as
// they were).
const absUncheckedExpr = `(is_folder AND abs_indexed_at IS NOT indexed_at)`

// SetABSFiles records the ABS files of the books at these paths (the scanner's
// read of a book whose audio is unchanged but whose metadata.json is new,
// changed, gone or not read for this index), and re-resolves each book whose
// effective row may lack what the file says. It commits every pathReadingBatch
// books.
func (c *Catalog) SetABSFiles(ctx context.Context, libraryID int64, readings map[string]ABSReading) error {
	return c.eachPathBatch(ctx, "SetABSFiles", slices.Collect(maps.Keys(readings)),
		func(tx *sql.Tx, relPath string) (bool, error) {
			changed, err := setABSFile(ctx, tx, libraryID, relPath, readings[relPath])
			if err != nil {
				return false, fmt.Errorf("%s: %w", relPath, err)
			}
			return changed, nil
		})
}

// setABSFile records one book's reading and reports whether it re-resolved the
// book: unless the reading says what the one recorded for this very index said,
// since only then did the effective row's last resolve have it (an older server's
// re-index resolved the row without the file).
func setABSFile(ctx context.Context, tx *sql.Tx, libraryID int64, relPath string, r ABSReading) (bool, error) {
	var id int64
	var old, indexedAt string
	var oldAt sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT id, abs_file, abs_indexed_at, indexed_at FROM books
		 WHERE library_id = ? AND rel_path = ? AND is_folder`,
		libraryID, relPath).Scan(&id, &old, &oldAt, &indexedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // gone, or a file book now
	}
	if err != nil {
		return false, err
	}
	fields := absFields(r.File)
	raw, err := encodeABSFields(fields)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE books SET abs_sig = ?, abs_indexed_at = indexed_at, abs_file = ? WHERE id = ?`,
		r.Sig, raw, id); err != nil {
		return false, err
	}
	if oldAt.Valid && oldAt.String == indexedAt && maps.Equal(parseABSFile(old), fields) {
		return false, nil
	}
	return true, refreshEffective(ctx, tx, id)
}

// overABS is scanned with the ABS file's values over it: every field the file
// names. A series the file names without a position keeps the scanned one only
// while it is the same series (seriesKey, any spelling).
func (l *bookLayers) overABS(scanned bookFields) bookFields {
	if len(l.abs) == 0 {
		return scanned
	}
	out := maps.Clone(scanned)
	maps.Copy(out, l.abs)
	if s, ok := l.abs[FieldSeries]; ok && l.abs[FieldSeriesIndex] == "" && seriesKey(s) != seriesKey(scanned[FieldSeries]) {
		out[FieldSeriesIndex] = ""
	}
	return out
}

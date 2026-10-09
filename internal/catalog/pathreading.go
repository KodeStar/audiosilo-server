package catalog

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// PathCheck is what a book's `scanned` snapshot needs from the next scan to match
// today's path baseline (scannedRevKey).
type PathCheck int

const (
	// PathOK: read with today's baseline, or no different under it.
	PathOK PathCheck = iota
	// PathFromSnapshot: the snapshot alone says the new reading, no file read.
	PathFromSnapshot
	// PathRead: only a read of the book's tags can tell.
	PathRead
)

// pathCheckExpr is a books row's PathCheck. Before revision 1 a book one folder
// deep ("Charles Dickens/Great Expectations") had that folder as its series and
// no author from the path, with the tags over both. So its snapshot (none, or
// stale: the row is an older server's reading, whatever revision the stale
// snapshot names, so read the tags):
//   - with a series other than the folder holds a tag's, and is right but for an
//     author the tags left blank, which is the folder (from the snapshot; blank
//     is missing or "", as migration 0016 wrote it);
//   - with the folder as series and as author (any ASCII case) took the series
//     from the path: a series tag naming the book's own author isn't one (from
//     the snapshot);
//   - with the folder as series and another author, or none, can't tell a series
//     read off the folder ("Discworld/Mort") from a tag naming the same one: read
//     the tags.
//
// A book at the root or deeper was read the same by both, and stays PathOK.
const pathCheckExpr = `CASE
	WHEN length(rel_path) - length(replace(rel_path, '/', '')) <> 1 THEN 0
	WHEN scanned = '' THEN 2
	WHEN NOT json_valid(scanned) THEN 0
	WHEN json_extract(scanned, '$."@indexed_at"') IS NOT indexed_at THEN 2
	WHEN json_extract(scanned, '$."@rev"') IS NOT NULL THEN 0
	WHEN json_extract(scanned, '$.series') IS NOT substr(rel_path, 1, instr(rel_path, '/') - 1)
		THEN CASE WHEN IFNULL(json_extract(scanned, '$.author'), '') = '' THEN 1 ELSE 0 END
	WHEN lower(json_extract(scanned, '$.author')) = lower(substr(rel_path, 1, instr(rel_path, '/') - 1)) THEN 1
	ELSE 2 END`

// pathReadingBatch is how many books one SetPathReading transaction rewrites, so
// a large backfill never holds the writer for long.
const pathReadingBatch = 250

// SetPathReading rewrites the scanned title, author and series of the books at
// these paths to today's path baseline (the scanner's backfill of PathCheck), and
// re-resolves their effective values. A reading is what a fresh read of the
// book's path and tags gives (PathRead); nil takes it from the snapshot itself
// (PathFromSnapshot). It commits every pathReadingBatch books.
func (c *Catalog) SetPathReading(ctx context.Context, libraryID int64, readings map[string]*metadata.Metadata) error {
	paths := make([]string, 0, len(readings))
	for p := range readings {
		paths = append(paths, p)
	}
	for len(paths) > 0 {
		batch := paths[:min(pathReadingBatch, len(paths))]
		paths = paths[len(batch):]
		if err := c.db.WithTx(ctx, "SetPathReading", func(tx *sql.Tx) error {
			for _, relPath := range batch {
				if err := setPathReading(ctx, tx, libraryID, relPath, readings[relPath]); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		c.changed()
	}
	return nil
}

func setPathReading(ctx context.Context, tx *sql.Tx, libraryID int64, relPath string, m *metadata.Metadata) error {
	id, err := bookIDByPath(ctx, tx, libraryID, relPath)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	l, err := loadLayers(ctx, tx, id)
	if err != nil {
		return err
	}
	if l.pathRev != "" {
		// Re-indexed with today's baseline since the scan read its signature.
		return nil
	}
	if l.fromRow != "" {
		// The snapshot is read off an older server's row, which wrote its chapters
		// with no scanned_title: keep their titles, as refreshEffective does when it
		// records such a snapshot (it won't here, the one written below being the
		// row's own).
		if _, err := tx.ExecContext(ctx, `UPDATE chapters SET scanned_title = title WHERE book_id = ?`, id); err != nil {
			return err
		}
	}
	snap := maps.Clone(l.scanned)
	if m != nil {
		maps.Copy(snap, pathFields(m))
	} else {
		folder, _, _ := strings.Cut(relPath, "/")
		if snap[FieldSeries] == folder {
			delete(snap, FieldSeries)
		}
		if snap[FieldAuthor] == "" {
			snap[FieldAuthor] = folder
		}
	}
	snap[scannedRevKey] = scannedRev
	raw, err := encodeScanned(snap, l.indexedAt)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE books SET scanned = ? WHERE id = ?`, raw, id); err != nil {
		return err
	}
	return refreshEffective(ctx, tx, id)
}

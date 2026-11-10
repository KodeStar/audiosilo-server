package catalog

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strings"
	"unicode"

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

// snapshotNamesExpr is a books row's `scanned` revision (NULL for none), title,
// series and narrator, for Signature.NamesCheck: the snapshot's (a field it
// leaves out is blank, as encodeScanned writes it), else, with no snapshot
// that is JSON, the row's own (as loadLayers reads it), never the row's
// effective values over a snapshot.
//
// The fifth column is revOlderOwn, so an older snapshot a scan finds needs no
// read can be stamped with today's revision (StampScannedRev).
const snapshotNamesExpr = `CASE WHEN json_valid(scanned) THEN json_extract(scanned, '$."@rev"') END,
	CASE WHEN json_valid(scanned) THEN IFNULL(json_extract(scanned, '$.title'), '') ELSE title END,
	CASE WHEN json_valid(scanned) THEN IFNULL(json_extract(scanned, '$.series'), '') ELSE series END,
	CASE WHEN json_valid(scanned) THEN IFNULL(json_extract(scanned, '$.narrator'), '') ELSE narrator END,
	` + revOlderOwn

// revOlderOwn is whether a books row's snapshot is of revision 1 to 3 (older
// than today's, scannedRev, but read with a lone folder as the author) and its
// own (its stamp the row's indexed_at, scannedStampKey): Signature.RevStamp,
// and StampScannedRev's compare-and-set.
const revOlderOwn = `CASE WHEN json_valid(scanned)
	THEN IFNULL(json_extract(scanned, '$."` + scannedRevKey + `"'), '') IN ('1', '2', '3')
	 AND json_extract(scanned, '$."` + scannedStampKey + `"') IS indexed_at
	ELSE 0 END`

// namesCheck is Signature.NamesCheck: whether a snapshot of revision rev (a
// book at rel, a folder or not, its primary file of format, its snapshot's
// title, series and narrator) may read otherwise under a newer revision, so
// the next scan reads its tags once: before revision 2 a title that may be the
// album naming its series (MaybeSeriesTitle), before 3 a leaf that states a
// series (PathLeafSeries), before 4 a book the tag reading of 2.4.0 and 2.5.0
// may read otherwise (tagReadingMayDiffer).
func namesCheck(rev int, rel string, isFolder bool, format, title, series, narrator string) bool {
	return (rev < revLeafSeries && metadata.PathLeafSeries(rel, isFolder)) ||
		(rev < revTitleNotSeries && metadata.MaybeSeriesTitle(rel, isFolder, title, series)) ||
		(rev < revTagReading && tagReadingMayDiffer(rel, format, title, series, narrator))
}

// tagReadingMayDiffer reports whether revision 4's tag reading may read a book
// otherwise than the snapshot (server#175): an MP4 (its \xa9nrt narrator item,
// read since 2.5.0), a narrator (a NARRATOR tag now beats composer), a series
// SplitSeriesTag splits ("Discworld, book #40", since 2.4.0), or a letter
// outside ASCII in its title or path (IsGenericTitle reads every script since
// 2.4.0, so a non-Latin title tag no longer gives way to the path's). A book
// none of these fits read the same then.
func tagReadingMayDiffer(rel, format, title, series, narrator string) bool {
	if metadata.IsMP4Format(format) || narrator != "" {
		return true
	}
	if _, _, ok := metadata.SplitSeriesTag(series); ok {
		return true
	}
	nonASCII := func(r rune) bool { return r > unicode.MaxASCII && unicode.IsLetter(r) }
	return strings.ContainsFunc(title, nonASCII) || strings.ContainsFunc(rel, nonASCII)
}

// StampScannedRev records today's revision (scannedRev) on the snapshots of the
// books at these paths, which a scan found need nothing of the newer revisions
// (Signature.RevStamp: no NamesCheck), so later scans don't test them again.
// Their values are unchanged, so nothing is re-resolved or announced; a row
// re-indexed or re-read since the scan read its signature (no longer a current
// revision 1 or 2 snapshot) is left as it is. It commits every
// pathReadingBatch books.
func (c *Catalog) StampScannedRev(ctx context.Context, libraryID int64, paths []string) error {
	return c.eachPathBatch(ctx, "StampScannedRev", paths, func(tx *sql.Tx, relPath string) (bool, error) {
		_, err := tx.ExecContext(ctx,
			`UPDATE books SET scanned = json_set(scanned, '$."`+scannedRevKey+`"', ?)
			  WHERE library_id = ? AND rel_path = ? AND `+revOlderOwn,
			scannedRev, libraryID, relPath)
		return false, err
	})
}

// pathReadingBatch is how many books one SetPathReading transaction rewrites, so
// a large backfill never holds the writer for long.
const pathReadingBatch = 250

// SetPathReading rewrites the scanned title, author, series and narrator of the
// books at these paths to today's reading (the scanner's backfill of PathCheck and
// Signature.NamesCheck), and
// re-resolves their effective values. A reading is what a fresh read of the
// book's path and tags gives (PathRead); nil takes it from the snapshot itself
// (PathFromSnapshot). It commits every pathReadingBatch books.
func (c *Catalog) SetPathReading(ctx context.Context, libraryID int64, readings map[string]*metadata.Metadata) error {
	return c.eachPathBatch(ctx, "SetPathReading", slices.Collect(maps.Keys(readings)),
		func(tx *sql.Tx, relPath string) (bool, error) {
			return true, setPathReading(ctx, tx, libraryID, relPath, readings[relPath])
		})
}

// eachPathBatch runs fn on each of paths, pathReadingBatch to a transaction so a
// large backfill never holds the writer for long, and announces a change
// (c.changed) after each batch in which fn reported one.
func (c *Catalog) eachPathBatch(ctx context.Context, name string, paths []string,
	fn func(tx *sql.Tx, relPath string) (changed bool, err error)) error {
	for batch := range slices.Chunk(paths, pathReadingBatch) {
		changed := false
		if err := c.db.WithTx(ctx, name, func(tx *sql.Tx) error {
			for _, relPath := range batch {
				ch, err := fn(tx, relPath)
				if err != nil {
					return err
				}
				changed = changed || ch
			}
			return nil
		}); err != nil {
			return err
		}
		if changed {
			c.changed()
		}
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
	if l.pathRev == scannedRev {
		// Re-indexed with today's reading since the scan read its signature.
		return nil
	}
	snap := maps.Clone(l.scanned)
	if m != nil {
		maps.Copy(snap, pathFields(m))
		snap[FieldNarrator] = m.Narrator // revision 4's reading of it (tagReadingMayDiffer)
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
	if err := recordScanned(ctx, tx, id, l, snap); err != nil {
		return err
	}
	return refreshEffective(ctx, tx, id)
}

package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// JoinPart is one book being joined into another (a disc book, when a folder's
// `book` override joins its disc folders into one book): where its timeline starts
// on the joined book's (Offset, seconds), its length, and whether it ends the
// joined book (Last: finishing it finishes the whole book). Unplaced marks a part
// whose Offset is unknown (the length of a part before it is): its listening state
// is not carried but stays on its own path (path-keyed, so it survives, and is
// there again if the join is undone), since placing it at a guessed offset would
// land its progress and bookmarks in an earlier part. Its config is still copied,
// which needs no offset.
type JoinPart struct {
	Path     string
	Offset   float64
	Duration float64
	Last     bool
	Unplaced bool
}

// at is a progress row's position and finished flag on the joined book's timeline:
// the part's position, clamped to its length when known (a finished part counts as
// its end, wherever its row was left), plus its offset; finished only when the
// part ends the joined book. A part with no offset or length that is Last (a
// move) leaves the row as it was.
func (p JoinPart) at(pos float64, finished bool) (float64, bool) {
	if p.Duration > 0 {
		pos = min(max(pos, 0), p.Duration)
		if finished {
			pos = p.Duration
		}
	}
	return p.Offset + pos, finished && p.Last
}

// end is a carried row's length when the joined book's is unknown: a lower bound
// on it, where the part ends on its timeline (its offset plus its length, or the
// length the row recorded when the part's is unknown too), never short of the row's
// carried position (pos, from at). Without it the row would keep the part's own
// length under a position on the joined timeline, and read as past 100%. A part
// with no offset or length (a move) keeps the row's own.
func (p JoinPart) end(pos, rowDuration float64) float64 {
	switch {
	case p.Duration > 0:
		return p.Offset + p.Duration // at clamps pos to it
	case p.Offset > 0:
		return max(p.Offset+rowDuration, pos)
	default:
		return rowDuration
	}
}

// JoinDurableState carries the path-keyed state of books joined into one (parts,
// in timeline order) to the joined book at into, whose length is total. The
// scanner calls it on the scan where the join takes effect, before the prune drops
// the parts' index rows.
//
// Listening state MOVES, as in MoveDurableState (carryListeningState), translated
// onto the joined book's whole-book timeline (a part's position p becomes its
// Offset + p; JoinPart.at), for every part but an Unplaced one (its offset is
// unknown; its state stays on its own path):
//   - progress: each listener keeps the furthest position among the parts and any
//     progress already on the joined book (mergeFurthest: the parts are one book,
//     unlike a move's collision, where the newer save wins). A part finished counts
//     as its end; the joined book is finished only when the furthest is the last
//     part, finished.
//   - bookmarks, notes, listening history and sessions: offset the same way.
//   - favourites and daily roll-ups: re-keyed (a listener's favourite once).
//   - ratings: re-keyed; where a listener rated more than one part (or the joined
//     book), the newest updated_at wins (carryRatings).
//
// The book's own config is COPIED, not moved, so the disc books keep theirs if the
// override is removed: an admin's metadata edits, attached ASIN/ISBN and custom
// cover field by field with the earliest part winning, and only where the joined
// book has none of its own (so a first disc's edits carry over); chapter renames
// all carry over (re-keyed to the file under the joined folder, so they never
// collide). Ignored Health issues stay with the parts: they were about those books.
//
// Removing the override later splits the folder back into its disc books again.
// Nothing is destroyed: the joined book's state stays keyed on the folder's path
// (it comes back if the folder is joined again), and the disc books start from
// whatever their own paths still hold (their copied config, no listening state).
//
// Two transactions, each all or nothing, for the reason MoveDurableState gives.
func (c *Catalog) JoinDurableState(ctx context.Context, libraryID int64, into string, parts []JoinPart, total float64) error {
	if len(parts) == 0 {
		return nil
	}
	for _, p := range parts {
		if !strings.HasPrefix(p.Path, into+"/") {
			return fmt.Errorf("join state: %q is not inside %q", p.Path, into)
		}
	}
	if err := c.db.WithTx(ctx, "JoinDurableState", func(tx *sql.Tx) error {
		return joinBookState(ctx, tx, libraryID, into, parts)
	}); err != nil {
		return err
	}
	return c.db.WithTx(ctx, "JoinDurableState", func(tx *sql.Tx) error {
		for _, p := range parts {
			if p.Unplaced {
				continue
			}
			if err := carryListeningState(ctx, tx, libraryID, p, into, total, mergeFurthest); err != nil {
				return fmt.Errorf("join listening state: %w", err)
			}
		}
		return nil
	})
}

// joinBookState copies the parts' own config onto the joined book (see
// JoinDurableState), then refreshes the joined book's effective metadata when it is
// indexed, so a copied edit shows at once rather than at its next scan.
func joinBookState(ctx context.Context, tx *sql.Tx, libraryID int64, into string, parts []JoinPart) error {
	for _, p := range parts {
		sub := strings.TrimPrefix(p.Path, into+"/") // the part's path inside the joined folder
		stmts := []struct {
			q    string
			args []any
		}{
			{`INSERT OR IGNORE INTO book_overrides(library_id, path, field, value, source, updated_by, updated_at)
			  SELECT library_id, ?1, field, value, source, updated_by, updated_at
			    FROM book_overrides WHERE library_id = ?2 AND path = ?3`,
				[]any{into, libraryID, p.Path}},
			// A rename is keyed by its file relative to the book: "" (a single-file
			// book) or "01.mp3" in the part is "CD1" or "CD1/01.mp3" in the joined book.
			{`INSERT OR IGNORE INTO chapter_overrides(library_id, path, file, start_ms, title, updated_by, updated_at)
			  SELECT library_id, ?1, CASE WHEN file = '' THEN ?2 ELSE ?2 || '/' || file END,
			         start_ms, title, updated_by, updated_at
			    FROM chapter_overrides WHERE library_id = ?3 AND path = ?4`,
				[]any{into, sub, libraryID, p.Path}},
			{`INSERT OR IGNORE INTO book_covers(library_id, path, mime, updated_by, updated_at, data)
			  SELECT library_id, ?1, mime, updated_by, updated_at, data
			    FROM book_covers WHERE library_id = ?2 AND path = ?3`,
				[]any{into, libraryID, p.Path}},
			// ASIN and ISBN each fill only where the joined book has none yet, so one
			// disc's ASIN doesn't keep a later disc's ISBN out.
			{`INSERT INTO book_enrichment(library_id, path, asin, isbn, updated_at)
			  SELECT library_id, ?1, asin, isbn, updated_at
			    FROM book_enrichment WHERE library_id = ?2 AND path = ?3
			  ON CONFLICT(library_id, path) DO UPDATE SET
			      asin = CASE WHEN book_enrichment.asin = '' THEN excluded.asin ELSE book_enrichment.asin END,
			      isbn = CASE WHEN book_enrichment.isbn = '' THEN excluded.isbn ELSE book_enrichment.isbn END`,
				[]any{into, libraryID, p.Path}},
		}
		for _, st := range stmts {
			if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
				return fmt.Errorf("join book state: %w", err)
			}
		}
	}
	// A copied custom cover is the joined book's art from now on.
	if err := refreshCoverArt(ctx, tx, libraryID, into); err != nil {
		return err
	}
	id, err := bookIDByPath(ctx, tx, libraryID, into)
	if errors.Is(err, ErrNotFound) {
		return nil // indexed later; UpsertBook layers the copies on then
	}
	if err != nil {
		return err
	}
	return refreshEffective(ctx, tx, id)
}

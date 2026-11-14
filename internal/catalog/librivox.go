package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

// LibriVox L6: a book added from LibriVox (bridge.KindLibrivoxFetch) is
// recorded as its LibriVox recording through book_matches, so /meta answers it
// with no console match. Only a stored, non-empty recording id is a LibriVox
// identity here: never a work-level match, never /meta's guess of a work's
// first recording.

// ErrFetchNotIndexed is a done librivox.fetch whose result names no book
// indexed in its own books library, or disagrees with its params (another
// library or project): nothing is recorded.
var ErrFetchNotIndexed = errors.New("the LibriVox fetch's book is not indexed")

// FoldLibrivoxFetch records a done librivox.fetch's book as the job's LibriVox
// recording: its community match (how accepted, set by the admin who asked),
// with the work and recording the server resolved when it queued the job. What
// it does at the book's path depends on how the Bridge got the folder
// (LibrivoxFetchResult.Folder):
//   - placed (bridge.FolderPlaced): the folder was vacant, so every row already at that path
//     is a deleted book's leftover: the match is replaced, and a leftover ASIN
//     or ISBN (book_overrides, book_enrichment), which /meta would ask before
//     the match, goes;
//   - adopted (bridge.FolderAdopted): the folder is an earlier fetch's book, so a match already
//     recorded there (an admin's rematch) stays and nothing else is touched; a
//     book with none (a fold that was lost) is recorded;
//   - not said (bridge.FolderUnsaid, an older Bridge): the match is replaced and nothing else is
//     touched.
//
// No other path is touched. The result must name the job's own library and
// project and a book indexed there (the Bridge rescans before it reports done),
// else ErrFetchNotIndexed. A job that isn't a done librivox.fetch, or whose
// params name no recording, changes nothing. A content write (matches feed the
// admin lists and Health), and the OnBookChange listeners hear of it: the
// rescan woke them before the match was there.
func (c *Catalog) FoldLibrivoxFetch(ctx context.Context, j bridge.Job) error {
	if j.Kind != bridge.KindLibrivoxFetch || j.State != bridge.StateDone || j.LibraryID == nil {
		return nil
	}
	p, r, err := bridge.LibrivoxFetchOf(j)
	if err != nil {
		return fmt.Errorf("read LibriVox fetch %d: %w", j.ID, err)
	}
	if strings.TrimSpace(p.WorkID) == "" || strings.TrimSpace(p.RecordingID) == "" {
		return nil
	}
	if r.LibraryID != *j.LibraryID || !fetchResultFits(p, r) {
		return ErrFetchNotIndexed
	}
	var editor any
	if j.RequestedBy != nil {
		editor = j.RequestedBy.ID
	}
	folder := r.Folder()
	err = c.db.WithTx(ctx, "FoldLibrivoxFetch", func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT b.id FROM books b JOIN libraries l ON l.id = b.library_id
			 WHERE b.library_id = ? AND b.rel_path = ? AND l.kind = ?`, r.LibraryID, r.Path, KindBooks).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFetchNotIndexed
		}
		if err != nil {
			return err
		}
		ref := Ref{LibraryID: r.LibraryID, Path: r.Path}
		if folder == bridge.FolderPlaced {
			if err := dropLeftoverIdentity(ctx, tx, ref, id); err != nil {
				return err
			}
		}
		var was *MatchAnchor
		if folder == bridge.FolderAdopted {
			was = &MatchAnchor{} // insert only: a match recorded since stays
		}
		a := &MatchAnchor{WorkID: p.WorkID, RecordingID: p.RecordingID, How: MatchHowAccepted}
		return writeAnchor(ctx, tx, ref, a, was, editor, c.ts())
	})
	if err == nil {
		c.changed()
	}
	return err
}

// How a finished librivox.fetch's book is matched, as LibrivoxFetchMatch says
// it (the bridge_job_finished event's librivox_match).
const (
	// LibrivoxMatched: to the LibriVox recording it was added as.
	LibrivoxMatched = "matched"
	// LibrivoxKept: the Bridge adopted a book already in the library, which
	// keeps the match it had (an admin's).
	LibrivoxKept = "kept"
	// LibrivoxUnmatched: to nothing it was added as (recording its match
	// failed); adding it again matches it.
	LibrivoxUnmatched = "unmatched"
)

// LibrivoxFetchMatch is how the book of a done librivox.fetch (params p,
// result r) is matched now, once FoldLibrivoxFetch has run, which the job's
// notification says: LibrivoxMatched as the job's recording, LibrivoxKept when
// the Bridge adopted a book matched to something else (an admin's rematch,
// which the fold keeps), LibrivoxUnmatched when it is neither (the fold
// failed), "" when the match can't be read. A result the fold refused (it
// names another library or project than the job's, or no book indexed in a
// books library) is LibrivoxUnmatched whatever match is recorded at the path
// it names: a deleted book's stays there.
func (c *Catalog) LibrivoxFetchMatch(ctx context.Context, p bridge.LibrivoxFetchParams, r bridge.LibrivoxFetchResult) string {
	if !fetchResultFits(p, r) {
		return LibrivoxUnmatched
	}
	var work, recording sql.NullString
	err := c.db.QueryRowContext(ctx, `SELECT m.work_id, m.recording_id FROM books b
		  JOIN libraries l ON l.id = b.library_id
		  LEFT JOIN book_matches m ON m.library_id = b.library_id AND m.path = b.rel_path
		 WHERE b.library_id = ? AND b.rel_path = ? AND l.kind = ?`, r.LibraryID, r.Path, KindBooks).Scan(&work, &recording)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return LibrivoxUnmatched
	case err != nil:
		return ""
	case work.Valid && work.String == p.WorkID && recording.String == p.RecordingID:
		return LibrivoxMatched
	case work.Valid && r.Folder() == bridge.FolderAdopted:
		return LibrivoxKept
	}
	return LibrivoxUnmatched
}

// fetchResultFits is whether a librivox.fetch's result names its params' own
// library and project and a clean path: one that doesn't is never folded.
func fetchResultFits(p bridge.LibrivoxFetchParams, r bridge.LibrivoxFetchResult) bool {
	return r.LibraryID == p.LibraryID && r.LibrivoxID == p.LibrivoxID && cleanLibPath(r.Path)
}

// dropLeftoverIdentity removes the ASIN and ISBN a deleted book left at ref
// (its book_overrides rows for them, whoever set them, and its
// book_enrichment row) and rebuilds the effective metadata of book id, the new
// book there, in tx: a newly placed LibriVox book is its recording, never the
// book that was in its folder before.
func dropLeftoverIdentity(ctx context.Context, tx *sql.Tx, ref Ref, id int64) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM book_overrides WHERE library_id = ? AND path = ? AND field IN (?, ?)`,
		ref.LibraryID, ref.Path, FieldASIN, FieldISBN)
	if err != nil {
		return err
	}
	fields, err := res.RowsAffected()
	if err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM book_enrichment WHERE library_id = ? AND path = ?`, ref.LibraryID, ref.Path)
	if err != nil {
		return err
	}
	enriched, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if fields+enriched == 0 {
		return nil
	}
	return refreshEffective(ctx, tx, id)
}

// RecordingBooks are the indexed books recorded as recording recordingID of
// work workID (book_matches with that exact, non-empty recording), by library
// then path: where a LibriVox recording already is on this server. A
// work-level match (no recording) is never one.
func (c *Catalog) RecordingBooks(ctx context.Context, workID, recordingID string) ([]Ref, error) {
	if recordingID == "" {
		return []Ref{}, nil
	}
	by, err := c.WorkRecordingBooks(ctx, workID)
	if err != nil {
		return nil, err
	}
	if in := by[recordingID]; in != nil {
		return in, nil
	}
	return []Ref{}, nil
}

// WorkRecordingBooks are RecordingBooks for every recording of work workID,
// by recording id, in one read (book_matches has no index by work, so a work's
// recordings are not asked for one at a time). A recording with no book has no
// entry.
func (c *Catalog) WorkRecordingBooks(ctx context.Context, workID string) (map[string][]Ref, error) {
	out := map[string][]Ref{}
	if workID == "" {
		return out, nil
	}
	type row struct {
		recording string
		ref       Ref
	}
	rows, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *row) error {
		return rows.Scan(&r.recording, &r.ref.LibraryID, &r.ref.Path)
	}, `SELECT m.recording_id, m.library_id, m.path FROM book_matches m
		   JOIN books b ON b.library_id = m.library_id AND b.rel_path = m.path
		  WHERE m.work_id = ? AND m.recording_id <> ''
		  ORDER BY m.library_id, m.path`, workID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.recording] = append(out[r.recording], r.ref)
	}
	return out, nil
}

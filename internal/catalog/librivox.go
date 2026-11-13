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
// with the work and recording the server resolved when it queued the job. The
// Bridge placed the book in a folder that was vacant, so a row already at that
// path (a deleted book's leftover) is replaced; no other path is touched. The
// result must name the job's own library and project and a book indexed there
// (the Bridge rescans before it reports done), else ErrFetchNotIndexed. A job
// that isn't a done librivox.fetch, or whose params name no recording, changes
// nothing. A content write (matches feed the admin lists and Health).
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
	if r.LibraryID != *j.LibraryID || r.LibraryID != p.LibraryID || r.LibrivoxID != p.LibrivoxID ||
		r.Path == "." || !bridgeBookPath(r.Path) {
		return ErrFetchNotIndexed
	}
	var editor any
	if j.RequestedBy != nil {
		editor = j.RequestedBy.ID
	}
	return c.db.WithTx(ctx, "FoldLibrivoxFetch", func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM books b JOIN libraries l ON l.id = b.library_id
			 WHERE b.library_id = ? AND b.rel_path = ? AND l.kind = ?`, r.LibraryID, r.Path, KindBooks).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFetchNotIndexed
		}
		if err != nil {
			return err
		}
		a := &MatchAnchor{WorkID: p.WorkID, RecordingID: p.RecordingID, How: MatchHowAccepted}
		return writeAnchor(ctx, tx, Ref{LibraryID: r.LibraryID, Path: r.Path}, a, nil, editor, c.ts())
	})
}

// RecordingBooks are the indexed books recorded as recording recordingID of
// work workID (book_matches with that exact, non-empty recording), by library
// then path: where a LibriVox recording already is on this server. A
// work-level match (no recording) is never one.
func (c *Catalog) RecordingBooks(ctx context.Context, workID, recordingID string) ([]Ref, error) {
	if workID == "" || recordingID == "" {
		return []Ref{}, nil
	}
	return queryRows(ctx, c.db, func(rows *sql.Rows, r *Ref) error { return rows.Scan(&r.LibraryID, &r.Path) },
		`SELECT m.library_id, m.path FROM book_matches m
		   JOIN books b ON b.library_id = m.library_id AND b.rel_path = m.path
		  WHERE m.work_id = ? AND m.recording_id = ? AND m.recording_id <> ''
		  ORDER BY m.library_id, m.path`, workID, recordingID)
}

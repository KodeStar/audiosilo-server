package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/chapteralign"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Where a book's chapters come from (books.chapters_source, and an admin's choice
// of it in chapter_choices).
const (
	// ChaptersFromFiles is the scan's own chapters (the files' embedded ones, or
	// one per file). Stored as "" in books.chapters_source.
	ChaptersFromFiles = "files"
	// ChaptersFromCommunity is a community chapter list fitted onto the audio.
	ChaptersFromCommunity = "community"
)

// ChapterSourceAuto is BookEdit.ChapterSource's "remove the choice": the source
// is then automatic, community when the book has no chapters of its own and a
// fill fitted (see applyChapterSource).
const ChapterSourceAuto = "auto"

// Community chapter statuses beyond chapteralign's (see community_chapters).
const (
	// CommunityNoMatch: the community has no recording for the book's
	// identifiers.
	CommunityNoMatch = "no_match"
	// CommunityUnavailable: the recording has no chapter list.
	CommunityUnavailable = "unavailable"
)

// chapterBasisExpr is what a community chapter check depends on, for a books row
// b: its identifiers, the length of its audio, the scan's own chapters (their
// hash: chapters tagged since change it) and its files (relative to the book, in
// play order; ” for a single-file book). A check whose basis differs is stale.
// The paths are the book's own, not the library's, so a moved book (the same
// audio) keeps its check, while a file renamed in place does not. (indexed_at
// would not do: an unchanged book is re-indexed whenever its folder is.)
const chapterBasisExpr = `(b.asin || '|' || b.isbn || '|' || CAST(ROUND(b.duration * 1000) AS INTEGER)
	|| '|' || b.chapters_hash
	|| '|' || COALESCE((SELECT group_concat(substr(rel_path, length(b.rel_path) + 2), char(31)) FROM
	       (SELECT bf.rel_path FROM book_files bf WHERE bf.book_id = b.id ORDER BY bf.seq)), ''))`

// chaptersHash names a chapter list (its JSON): books.chapters_hash, and the fit
// in a book's rows (books.chapters_fit).
func chaptersHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

// bookRelative and libraryRelative turn a chapter's file between library-relative
// (the chapters rows) and relative to the book at bookPath (” for the book's own
// file), as a check stores its fit, so the fit moves with the book.
func bookRelative(chs []metadata.Chapter, bookPath string) []metadata.Chapter {
	out := make([]metadata.Chapter, len(chs))
	for i, ch := range chs {
		if ch.FilePath == bookPath {
			ch.FilePath = ""
		} else {
			ch.FilePath = strings.TrimPrefix(ch.FilePath, bookPath+"/")
		}
		out[i] = ch
	}
	return out
}

func libraryRelative(chs []metadata.Chapter, bookPath string) {
	for i := range chs {
		if chs[i].FilePath == "" {
			chs[i].FilePath = bookPath
		} else {
			chs[i].FilePath = bookPath + "/" + chs[i].FilePath
		}
	}
}

// CommunityChapters is a book's last community chapter check.
type CommunityChapters struct {
	LibraryID   int64                `json:"-"`
	Path        string               `json:"-"`
	Basis       string               `json:"-"`
	Status      string               `json:"status"`
	WorkID      string               `json:"work_id,omitempty"`
	RecordingID string               `json:"recording_id,omitempty"`
	ListHash    string               `json:"-"`
	Detail      *chapteralign.Detail `json:"detail,omitempty"`
	// Chapters are the fitted chapters (a fitted status only); the book page
	// shows the counts in Detail, not the list.
	Chapters  []metadata.Chapter `json:"-"`
	CheckedAt string             `json:"checked_at"`
	// Stale is set (by GetCommunityChapters) when the book has changed since the
	// check (other audio, other identifiers): it is not used until checked again.
	Stale bool `json:"stale,omitempty"`
}

// fitted reports whether a stored status carries chapters.
func fitted(status string) bool { return chapteralign.Status(status).Fitted() }

// DueChapterChecks lists up to limit books with an identifier whose community
// chapters were never checked, were checked against a different basis, or were
// checked before staleBefore (RFC3339), oldest check first.
func (c *Catalog) DueChapterChecks(ctx context.Context, staleBefore string, limit int) ([]Ref, error) {
	return queryRows(ctx, c.db, func(r *sql.Rows, b *Ref) error {
		return r.Scan(&b.LibraryID, &b.Path)
	}, `SELECT b.library_id, b.rel_path FROM books b
	     LEFT JOIN community_chapters cc ON cc.library_id = b.library_id AND cc.path = b.rel_path
	    WHERE (b.asin <> '' OR b.isbn <> '') AND b.duration > 0
	      AND (cc.path IS NULL OR cc.basis <> `+chapterBasisExpr+` OR cc.checked_at < ?)
	    ORDER BY cc.checked_at IS NOT NULL, cc.checked_at, b.id
	    LIMIT ?`, staleBefore, limit)
}

// ChapterInputs is what a community chapter check fits: the book's identifiers,
// its files in play order (one for a single-file book) and the scan's own
// chapters, with the basis to record.
type ChapterInputs struct {
	ASIN     string
	ISBN     string
	Basis    string
	Files    []BookFile
	Chapters []metadata.Chapter
}

// ChapterInputs reads a book's ChapterInputs. ErrNotFound when no book is indexed
// at the path. The chapters are the scan's even while community ones stand in.
func (c *Catalog) ChapterInputs(ctx context.Context, libraryID int64, path string) (*ChapterInputs, error) {
	path = CleanRelPath(path)
	in := &ChapterInputs{}
	var (
		id       int64
		duration float64
		snapshot string
		isFolder bool
	)
	err := c.db.QueryRowContext(ctx,
		`SELECT b.id, b.asin, b.isbn, b.duration, b.is_folder, b.scanned_chapters, `+chapterBasisExpr+`
		   FROM books b WHERE b.library_id = ? AND b.rel_path = ?`, libraryID, path).
		Scan(&id, &in.ASIN, &in.ISBN, &duration, &isFolder, &snapshot, &in.Basis)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b := Book{ID: id}
	if err := c.loadFiles(ctx, &b); err != nil {
		return nil, err
	}
	in.Files = b.Files
	if len(in.Files) == 0 && !isFolder {
		in.Files = []BookFile{{RelPath: path, Duration: duration}}
	}
	in.Chapters, err = scannedChapters(ctx, c.db, id, snapshot)
	return in, err
}

// scannedChapters is a book's own chapters as the scan found them: its
// scanned_chapters snapshot, or, on a row indexed before there was one (whose
// rows are the scan's), its chapter rows with their scanned titles.
func scannedChapters(ctx context.Context, q rowQuerier, bookID int64, snapshot string) ([]metadata.Chapter, error) {
	if snapshot != "" {
		var chs []metadata.Chapter
		if err := json.Unmarshal([]byte(snapshot), &chs); err != nil {
			return nil, fmt.Errorf("scanned chapters: %w", err)
		}
		return chs, nil
	}
	return queryRows(ctx, q, func(r *sql.Rows, ch *metadata.Chapter) error {
		return r.Scan(&ch.Index, &ch.Title, &ch.FileIndex, &ch.FilePath, &ch.Start, &ch.End, &ch.BookOffset)
	}, `SELECT idx, scanned_title, file_index, file_path, start, "end", book_offset
	      FROM chapters WHERE book_id = ? ORDER BY idx`, bookID)
}

// SaveCommunityChapters records a book's check and, when the book is indexed,
// rebuilds its chapters from it (refreshEffective) in the same transaction.
func (c *Catalog) SaveCommunityChapters(ctx context.Context, cc CommunityChapters) error {
	detail, err := json.Marshal(cc.Detail)
	if err != nil {
		return err
	}
	path := CleanRelPath(cc.Path)
	chapters, err := json.Marshal(bookRelative(cc.Chapters, path))
	if err != nil {
		return err
	}
	return c.db.WithTx(ctx, "SaveCommunityChapters", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO community_chapters(library_id, path, basis, status, work_id, recording_id, list_hash, detail, chapters, checked_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(library_id, path) DO UPDATE SET
			     basis = excluded.basis, status = excluded.status, work_id = excluded.work_id,
			     recording_id = excluded.recording_id, list_hash = excluded.list_hash,
			     detail = excluded.detail, chapters = excluded.chapters, checked_at = excluded.checked_at`,
			cc.LibraryID, path, cc.Basis, cc.Status, cc.WorkID, cc.RecordingID, cc.ListHash,
			string(detail), string(chapters), c.ts()); err != nil {
			return err
		}
		id, err := bookIDByPath(ctx, tx, cc.LibraryID, path)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return refreshEffective(ctx, tx, id)
	})
}

// TouchCommunityChapters renews a check that found what it found before (the same
// list for the same audio): only its checked_at moves, nothing is fitted or
// rewritten.
func (c *Catalog) TouchCommunityChapters(ctx context.Context, libraryID int64, path string) error {
	_, err := c.db.ExecContext(ctx, `UPDATE community_chapters SET checked_at = ? WHERE library_id = ? AND path = ?`,
		c.ts(), libraryID, CleanRelPath(path))
	return err
}

// GetCommunityChapters is a book's last check, nil when it has none.
func (c *Catalog) GetCommunityChapters(ctx context.Context, libraryID int64, path string) (*CommunityChapters, error) {
	cc := CommunityChapters{LibraryID: libraryID, Path: CleanRelPath(path)}
	var detail string
	var current sql.NullBool // NULL: no book indexed at the path
	err := c.db.QueryRowContext(ctx,
		`SELECT cc.basis, cc.status, cc.work_id, cc.recording_id, cc.list_hash, cc.detail, cc.checked_at,
		        (SELECT cc.basis = `+chapterBasisExpr+` FROM books b WHERE b.library_id = cc.library_id AND b.rel_path = cc.path)
		   FROM community_chapters cc WHERE cc.library_id = ? AND cc.path = ?`, libraryID, cc.Path).
		Scan(&cc.Basis, &cc.Status, &cc.WorkID, &cc.RecordingID, &cc.ListHash, &detail, &cc.CheckedAt, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(detail), &cc.Detail); err != nil {
		return nil, fmt.Errorf("community chapters detail: %w", err)
	}
	cc.Stale = current.Valid && !current.Bool
	return &cc, nil
}

// applyChapterSource puts the book's chapters from the source it should have
// now: the community's when a check of the book as it is now fitted and either
// the admin chose them (chapter_choices) or, with no choice, the book has no
// chapters of its own (a fill); else the scan's own (books.scanned_chapters).
// A check of other audio or identifiers is stale and never used: it may name
// files no longer there. Titles are the scanned ones; refreshEffective applies
// the renames after. A fit already in the rows is not written again.
func applyChapterSource(ctx context.Context, tx *sql.Tx, bookID, libID int64, path string) error {
	var current, fit, snapshot string
	if err := tx.QueryRowContext(ctx,
		`SELECT chapters_source, chapters_fit, scanned_chapters FROM books WHERE id = ?`, bookID).
		Scan(&current, &fit, &snapshot); err != nil {
		return err
	}
	var choice, status, fittedJSON string
	var upToDate bool
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT source FROM chapter_choices WHERE library_id = ?1 AND path = ?2), ''),
		        COALESCE(cc.status, ''), COALESCE(cc.chapters, ''),
		        COALESCE(cc.basis = `+chapterBasisExpr+`, 0)
		   FROM books b LEFT JOIN community_chapters cc ON cc.library_id = ?1 AND cc.path = ?2
		  WHERE b.id = ?3`, libID, path, bookID).
		Scan(&choice, &status, &fittedJSON, &upToDate)
	if err != nil {
		return err
	}
	community := upToDate && fitted(status) &&
		(choice == ChaptersFromCommunity || (choice == "" && status == string(chapteralign.Fill)))
	switch {
	case community:
		key := chaptersHash(fittedJSON)
		if current == ChaptersFromCommunity && fit == key {
			return nil
		}
		legacy := snapshot == ""
		if legacy {
			// A row from before the snapshot: its rows are the scan's, so they
			// are kept before the community's replace them.
			own, err := scannedChapters(ctx, tx, bookID, "")
			if err != nil {
				return err
			}
			raw, err := json.Marshal(own)
			if err != nil {
				return err
			}
			snapshot = string(raw)
		}
		var chs []metadata.Chapter
		if err := json.Unmarshal([]byte(fittedJSON), &chs); err != nil {
			return fmt.Errorf("community chapters: %w", err)
		}
		libraryRelative(chs, path)
		if err := replaceChapters(ctx, tx, bookID, chs); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE books SET chapters_source = ?, chapters_fit = ?, scanned_chapters = ?, chapters_hash = ? WHERE id = ?`,
			ChaptersFromCommunity, key, snapshot, chaptersHash(snapshot), bookID); err != nil {
			return err
		}
		if legacy {
			// The snapshot is the chapters the check fitted (the rows it read), so
			// the check stays current under the basis the snapshot changes.
			_, err := tx.ExecContext(ctx,
				`UPDATE community_chapters SET basis = (SELECT `+chapterBasisExpr+` FROM books b WHERE b.id = ?)
				  WHERE library_id = ? AND path = ?`, bookID, libID, path)
			return err
		}
		return nil
	case current == ChaptersFromCommunity:
		own, err := scannedChapters(ctx, tx, bookID, snapshot)
		if err != nil {
			return err
		}
		if err := replaceChapters(ctx, tx, bookID, own); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE books SET chapters_source = '', chapters_fit = '' WHERE id = ?`, bookID)
		return err
	}
	return nil
}

// replaceChapters swaps a book's chapter rows for chs (their titles as scanned).
func replaceChapters(ctx context.Context, tx *sql.Tx, bookID int64, chs []metadata.Chapter) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM chapters WHERE book_id = ?`, bookID); err != nil {
		return err
	}
	for _, ch := range chs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chapters(book_id, idx, title, scanned_title, file_index, file_path, start, "end", book_offset)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			bookID, ch.Index, ch.Title, ch.Title, ch.FileIndex, ch.FilePath, ch.Start, ch.End, ch.BookOffset); err != nil {
			return err
		}
	}
	return nil
}

// chapterChoice is the admin's choice of chapter source for a book ("" =
// automatic).
func chapterChoice(ctx context.Context, q querier, libID int64, path string) (string, error) {
	var source string
	err := q.QueryRowContext(ctx,
		`SELECT source FROM chapter_choices WHERE library_id = ? AND path = ?`, libID, path).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return source, err
}

// setChapterChoice writes (or, for ChapterSourceAuto, removes) a book's choice of
// chapter source.
func setChapterChoice(ctx context.Context, tx *sql.Tx, ref Ref, source string, editor any, now string) error {
	if source == ChapterSourceAuto {
		_, err := tx.ExecContext(ctx, `DELETE FROM chapter_choices WHERE library_id = ? AND path = ?`, ref.LibraryID, ref.Path)
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO chapter_choices(library_id, path, source, updated_by, updated_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(library_id, path) DO UPDATE SET
		     source = excluded.source, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		ref.LibraryID, ref.Path, source, editor, now)
	return err
}

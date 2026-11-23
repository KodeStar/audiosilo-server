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

	"github.com/kodestar/audiosilo-server/internal/bridge"
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
	// ChaptersFromGenerated is the chapters the Bridge found in the narration of
	// a book with none of its own (its .audiosilo/chapters.json), fitted onto
	// the same files. Never an admin's choice: it stands in only when nothing
	// was chosen and the community's don't.
	ChaptersFromGenerated = "generated"
)

// ChapterSourceAuto is BookEdit.ChapterSource's "remove the choice": the source
// is then automatic, community when the book has no chapters of its own and a
// fill fitted, else the generated ones when they fit (see applyChapterSource).
const ChapterSourceAuto = "auto"

// Community chapter statuses beyond chapteralign's (see community_chapters).
const (
	// CommunityNoMatch: the community has no recording for the book's
	// identifiers, or, for a book whose recorded match names another work
	// than they do, none for that match (it names no recording, or one its
	// work doesn't list): chaptercheck's chapters.
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
// would not do: an unchanged book is re-indexed whenever its folder is.) A book
// with a recorded match (book_matches) adds its work and recording, which the
// check follows over the identifiers (ChapterInputs.Match), so a match made,
// changed or unlinked since makes the check stale; a book without one keeps
// the basis it had before matches counted. A match that names the recording
// the check already came from changes nothing a check would find, so the
// check is carried over it (checkIsOf, stampChapterBasis; migration 0090 for
// the checks made before matches counted) rather than fitted again.
const chapterBasisExpr = `(b.asin || '|' || b.isbn || '|' || CAST(ROUND(b.duration * 1000) AS INTEGER)
	|| '|' || b.chapters_hash
	|| '|' || COALESCE((SELECT group_concat(substr(rel_path, length(b.rel_path) + 2), char(31)) FROM
	       (SELECT bf.rel_path FROM book_files bf WHERE bf.book_id = b.id ORDER BY bf.seq)), '')
	|| COALESCE((SELECT char(30) || cm.work_id || '/' || cm.recording_id FROM book_matches cm
	       WHERE cm.library_id = b.library_id AND cm.path = b.rel_path), ''))`

// chaptersHash names a chapter list (its JSON): books.chapters_hash, and the fit
// in a book's rows (books.chapters_fit).
func chaptersHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

// checkIsOf reports whether the community chapter check of the book at ref is
// of the book as it is now (its basis is the book's) and came from recording
// recordingID of work workID. A match about to name that recording changes
// nothing a check would find: the match's recording, which its work lists, is
// that same one (chaptercheck's chapters). writeAnchor asks before it writes and stamps the check with the
// basis after (stampChapterBasis), so the commonest match, a book's ASIN's own
// recording accepted in the console, doesn't take the book's community
// chapters away until they are fitted again.
func checkIsOf(ctx context.Context, tx *sql.Tx, ref Ref, workID, recordingID string) (bool, error) {
	var is bool
	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM community_chapters cc
		     JOIN books b ON b.library_id = cc.library_id AND b.rel_path = cc.path
		    WHERE cc.library_id = +?1 AND cc.path = +?2 AND cc.recording_id <> ''
		      AND cc.work_id = +?3 AND cc.recording_id = +?4 AND cc.basis = `+chapterBasisExpr+`)`,
		ref.LibraryID, ref.Path, workID, recordingID).Scan(&is)
	return is, err
}

// stampChapterBasis makes the community chapter check of the book at ref one
// of the book as it is now: its basis becomes the book's. Only for an indexed
// book's check known to still hold (checkIsOf before a match write;
// useChapters' snapshot of the rows a current check read).
func stampChapterBasis(ctx context.Context, tx *sql.Tx, ref Ref) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE community_chapters SET basis = (SELECT `+chapterBasisExpr+` FROM books b
		     WHERE b.library_id = ?1 AND b.rel_path = ?2)
		  WHERE library_id = ?1 AND path = ?2`, ref.LibraryID, ref.Path)
	return err
}

// bookRelative and libraryRelative turn a chapter's file between library-relative
// (the chapters rows) and relative to the book at bookPath (” for the book's own
// file), as a check stores its fit, so the fit moves with the book.
func bookRelative(chs []metadata.Chapter, bookPath string) []metadata.Chapter {
	out := make([]metadata.Chapter, len(chs))
	for i, ch := range chs {
		ch.FilePath = inBook(ch.FilePath, bookPath)
		out[i] = ch
	}
	return out
}

// inBook is a library-relative file relative to the book at bookPath, "" for
// the book's own file.
func inBook(file, bookPath string) string {
	if file == bookPath {
		return ""
	}
	return strings.TrimPrefix(file, bookPath+"/")
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
	    WHERE `+inBookLibrary+` AND (b.asin <> '' OR b.isbn <> '') AND b.duration > 0
	      AND (cc.path IS NULL OR cc.basis <> `+chapterBasisExpr+` OR cc.checked_at < ?)
	    ORDER BY cc.checked_at IS NOT NULL, cc.checked_at, b.id
	    LIMIT ?`, staleBefore, limit)
}

// ChapterInputs is what a community chapter check fits: the book's identifiers
// and its recorded match (nil for none), which names the recording over them
// (chaptercheck's chapters),
// its files in play order (one for a single-file book) and the scan's own
// chapters, with the basis to record.
type ChapterInputs struct {
	ASIN     string
	ISBN     string
	Match    *BookMatch
	Basis    string
	Files    []BookFile
	Chapters []metadata.Chapter
}

// ChapterInputs reads a book's ChapterInputs. ErrNotFound when no book is indexed
// at the path. The chapters are the scan's even while community ones stand in.
func (c *Catalog) ChapterInputs(ctx context.Context, libraryID int64, path string) (*ChapterInputs, error) {
	path = CleanRelPath(path)
	in := &ChapterInputs{}
	b := Book{RelPath: path}
	var snapshot string
	// The match in the same statement as the basis, so the check records the
	// basis of the match it follows (a join: its parameters as +?, see SQLite
	// in CONVENTIONS.md).
	var work, recording, how, setAt sql.NullString
	err := c.db.QueryRowContext(ctx,
		`SELECT b.id, b.asin, b.isbn, b.duration, b.size, b.is_folder, b.scanned_chapters, `+chapterBasisExpr+`,
		        bm.work_id, bm.recording_id, bm.how, bm.set_at
		   FROM books b LEFT JOIN book_matches bm ON bm.library_id = b.library_id AND bm.path = b.rel_path
		  WHERE b.library_id = +? AND b.rel_path = +?`, libraryID, path).
		Scan(&b.ID, &in.ASIN, &in.ISBN, &b.Duration, &b.Size, &b.IsFolder, &snapshot, &in.Basis,
			&work, &recording, &how, &setAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if work.Valid {
		in.Match = &BookMatch{WorkID: work.String, RecordingID: recording.String, How: how.String, MatchedAt: setAt.String}
	}
	if in.Files, err = playFiles(ctx, c.db, &b); err != nil {
		return nil, err
	}
	in.Chapters, err = scannedChapters(ctx, c.db, b.ID, snapshot)
	return in, err
}

// playFiles is book b's files in play order (library relative, with size and
// length): its book_files, or the book's own file for a single-file book. b
// carries the books row's id, rel_path, is_folder, size and duration.
func playFiles(ctx context.Context, q rowQuerier, b *Book) ([]BookFile, error) {
	files, err := queryRows(ctx, q, func(r *sql.Rows, f *BookFile) error {
		return r.Scan(&f.RelPath, &f.Seq, &f.Size, &f.Duration)
	}, `SELECT rel_path, seq, size, duration FROM book_files WHERE book_id = ? ORDER BY seq`, b.ID)
	if err != nil {
		return nil, err
	}
	return orOwnFile(files, b), nil
}

// orOwnFile is a book's book_files rows (in seq order), or for a single-file
// book with none its own file: its files in play order (playFiles).
func orOwnFile(files []BookFile, b *Book) []BookFile {
	if len(files) == 0 && !b.IsFolder {
		return []BookFile{{RelPath: b.RelPath, Size: b.Size, Duration: b.Duration}}
	}
	return files
}

// AlignFiles is a book's files as chapteralign fits chapters onto them.
func AlignFiles(files []BookFile) []chapteralign.File {
	out := make([]chapteralign.File, len(files))
	for i, f := range files {
		out[i] = chapteralign.File{Path: f.RelPath, Duration: f.Duration}
	}
	return out
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

// chapterRow is what applyChapterSource reads off a book's row: the book
// (id, library, rel_path, is_folder, size, duration: enough for playFiles), the
// source and fit its chapter rows hold, its scanned chapters, whether a
// chapters.json is stored for it, and whether its community chapter check was
// made of the book as it is now (checkCurrent: its basis is the book's). files
// is the book's playFiles once read (filesOf), so a dormant edit and the
// generated list after it read them once.
type chapterRow struct {
	book                   Book
	current, fit, snapshot string
	generated              bool
	checkCurrent           bool
	files                  []BookFile
	filesRead              bool
}

// filesOf is the book's files in play order (playFiles), read on the first
// call only.
func (r *chapterRow) filesOf(ctx context.Context, tx *sql.Tx) ([]BookFile, error) {
	if !r.filesRead {
		files, err := playFiles(ctx, tx, &r.book)
		if err != nil {
			return nil, err
		}
		r.files, r.filesRead = files, true
	}
	return r.files, nil
}

// holds reports whether the book's chapter rows are already source's fit named
// key.
func (r *chapterRow) holds(source, key string) bool { return r.current == source && r.fit == key }

// applyChapterSource puts the book's chapters from the source it should have
// now: an admin's own list (chapter_edits) while it applies to the book's
// files (applyEdited; else it is dormant); else the community's when a check
// of the book as it is now fitted and either the admin chose them
// (chapter_choices) or, with no choice, the book has no chapters of its own (a
// fill); else, with no choice, the generated ones when they fit the book
// (applyGenerated); else the scan's own (books.scanned_chapters). A check of
// other audio or identifiers is stale and never used: it may name files no
// longer there. Titles are the scanned ones (an edit's own); refreshEffective
// applies the renames after, except over an edit. A fit already in the rows is
// not written again, nor decoded.
func applyChapterSource(ctx context.Context, tx *sql.Tx, bookID, libID int64, path string, scan *Book) error {
	r := chapterRow{book: Book{ID: bookID, LibraryID: libID, RelPath: path}}
	// Whether a chapters.json is stored, from the record header alone
	// (octet_length, as BookAttachmentParts: the column is the row's last, past
	// the characters and recaps, so reading it would page through them). These
	// joins take their parameters as +?, which keeps SQLite from re-parsing them
	// on every run (see SQLite in CONVENTIONS.md).
	if err := tx.QueryRowContext(ctx,
		`SELECT b.is_folder, b.size, b.duration, b.chapters_source, b.chapters_fit, b.scanned_chapters,
		        COALESCE(octet_length(a.chapters), 0) > 0
		   FROM books b LEFT JOIN book_attachments a ON a.book_id = b.id WHERE b.id = +?`, bookID).
		Scan(&r.book.IsFolder, &r.book.Size, &r.book.Duration, &r.current, &r.fit, &r.snapshot, &r.generated); err != nil {
		return err
	}
	var choice, status, fittedJSON, edit string
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT source FROM chapter_choices WHERE library_id = +?1 AND path = +?2), ''),
		        COALESCE(cc.status, ''), COALESCE(cc.chapters, ''),
		        COALESCE(cc.basis = `+chapterBasisExpr+`, 0),
		        COALESCE((SELECT list FROM chapter_edits WHERE library_id = +?1 AND path = +?2), '')
		   FROM books b LEFT JOIN community_chapters cc ON cc.library_id = +?1 AND cc.path = +?2
		  WHERE b.id = +?3`, libID, path, bookID).
		Scan(&choice, &status, &fittedJSON, &r.checkCurrent, &edit)
	if err != nil {
		return err
	}
	if edit != "" {
		if applied, err := applyEdited(ctx, tx, &r, edit); err != nil || applied {
			return err
		}
	}
	community := r.checkCurrent && fitted(status) &&
		(choice == ChaptersFromCommunity || (choice == "" && status == string(chapteralign.Fill)))
	if community {
		key := chaptersHash(fittedJSON)
		if r.holds(ChaptersFromCommunity, key) {
			return nil
		}
		var chs []metadata.Chapter
		if err := json.Unmarshal([]byte(fittedJSON), &chs); err != nil {
			return fmt.Errorf("community chapters: %w", err)
		}
		libraryRelative(chs, path)
		return useChapters(ctx, tx, &r, ChaptersFromCommunity, key, chs)
	}
	if choice == "" && r.generated {
		if applied, err := applyGenerated(ctx, tx, &r); err != nil || applied {
			return err
		}
	}
	if r.current == "" {
		if scan != nil {
			return replaceChapters(ctx, tx, bookID, scan.Chapters)
		}
		return nil
	}
	// Leaving an edited, community or generated list: the scan's own come back.
	own, err := scannedChapters(ctx, tx, bookID, r.snapshot)
	if err != nil {
		return err
	}
	if err := replaceChapters(ctx, tx, bookID, own); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE books SET chapters_source = '', chapters_fit = '' WHERE id = ?`, bookID)
	return err
}

// useChapters puts chs (library-relative, titles as found) in the book's rows
// as source, its fit named key, unless the rows already hold that fit. On a
// row from before the snapshot (r.snapshot "") the rows are the scan's, so they
// are kept as the snapshot before chs replace them; the snapshot is then the
// chapters a check of the book as it is now fitted (the rows it read), so such
// a check (r.checkCurrent) stays current under the basis the snapshot changes,
// whichever source stands in. A check of other audio or identifiers stays
// stale.
func useChapters(ctx context.Context, tx *sql.Tx, r *chapterRow, source, key string, chs []metadata.Chapter) error {
	if r.holds(source, key) {
		return nil
	}
	legacy := r.snapshot == ""
	snapshot := r.snapshot
	if legacy {
		own, err := scannedChapters(ctx, tx, r.book.ID, "")
		if err != nil {
			return err
		}
		raw, err := json.Marshal(own)
		if err != nil {
			return err
		}
		snapshot = string(raw)
	}
	if err := replaceChapters(ctx, tx, r.book.ID, chs); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE books SET chapters_source = ?, chapters_fit = ?, scanned_chapters = ?, chapters_hash = ? WHERE id = ?`,
		source, key, snapshot, chaptersHash(snapshot), r.book.ID); err != nil || !legacy || !r.checkCurrent {
		return err
	}
	return stampChapterBasis(ctx, tx, Ref{LibraryID: r.book.LibraryID, Path: r.book.RelPath})
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

// DetectRefusal is the check of a chapters.detect job for the book at
// (libraryID, path) that bridge.NewJob.Check runs in the job's enqueue
// transaction (detectRefused): an edit or a choice saved after the request
// read the book refuses the job, and one saved after the job is queued cancels
// it (bridge.CancelWaitingBookJob in the same writer), so no detection slips
// in between (server#471).
func DetectRefusal(libraryID int64, path string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error { return detectRefused(ctx, tx, libraryID, path) }
}

// detectRefused is why a chapters.detect job for the book at (libraryID,
// path) would find a list never used, read through q: bridge.ErrChaptersChosen
// when an admin chose its chapter source, bridge.ErrChaptersEdited while an
// edit of its chapters applies (its chapters_source; a dormant one, made on
// other files, is none: the book's chapters come from the next source, a found
// list among them); nil when neither, and for a book no longer indexed there.
func detectRefused(ctx context.Context, q querier, libraryID int64, path string) error {
	var (
		chosen bool
		source string
	)
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM chapter_choices WHERE library_id = ?1 AND path = ?2),
		        COALESCE((SELECT chapters_source FROM books WHERE library_id = ?1 AND rel_path = ?2), '')`,
		libraryID, path).Scan(&chosen, &source); err != nil {
		return err
	}
	switch {
	case chosen:
		return bridge.ErrChaptersChosen
	case source == ChaptersFromEdited:
		return bridge.ErrChaptersEdited
	}
	return nil
}

// setChapterChoice writes (or, for ChapterSourceAuto, removes) a book's choice of
// chapter source. A choice cancels the book's chapters.detect job while it is
// queued or parked (bridge.CancelChaptersChosen): the list it would find is
// not used under the choice, as a new one is refused then (DetectRefusal). A
// claimed or running one finishes: its list stands in again if the choice is
// undone.
func (c *Catalog) setChapterChoice(ctx context.Context, tx *sql.Tx, ref Ref, source string, editor any, now string) error {
	if source == ChapterSourceAuto {
		_, err := tx.ExecContext(ctx, `DELETE FROM chapter_choices WHERE library_id = ? AND path = ?`, ref.LibraryID, ref.Path)
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chapter_choices(library_id, path, source, updated_by, updated_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(library_id, path) DO UPDATE SET
		     source = excluded.source, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		ref.LibraryID, ref.Path, source, editor, now); err != nil {
		return err
	}
	return bridge.CancelWaitingBookJob(ctx, tx, bridge.KindDetectChapters, ref.LibraryID, ref.Path, bridge.CancelChaptersChosen, c.now())
}

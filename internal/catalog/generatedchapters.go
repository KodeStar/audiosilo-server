package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"path"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/chapteralign"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Generated chapters (BRIDGE-PLAN.md Phase 1d): the Bridge finds the chapters in
// the narration of a book with no chapter markers and writes them beside it as
// .audiosilo/chapters.json (metadata.ChapterList), which the scan stores in
// book_attachments.chapters. They are a chapter source below the community's
// and above the files' (applyChapterSource): used only when no source was
// chosen, the community's don't apply, the book has no chapters of its own, the
// list was found on the files the book has now (each one's path relative to the
// book and its size, in play order) and it fits their audio
// (chapteralign.Align with no snapping, which must answer a fill). Anything else
// leaves the scan's own, so a list for other audio, or one beside a book that
// was tagged with chapters since, never stands in.

// applyGenerated puts the book's generated chapters, fitted onto its files
// (library relative, titles as found), in its rows (useChapters, the fit named
// by generatedKey: what it was fitted from) and reports whether they apply (see
// above), false leaving the rows to the caller. When the rows already hold the
// fit of that key, the list is neither decoded nor fitted again.
func applyGenerated(ctx context.Context, tx *sql.Tx, r *chapterRow) (bool, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT chapters FROM book_attachments WHERE book_id = ?`, r.book.ID).Scan(&raw); err != nil {
		return false, err
	}
	files, err := playFiles(ctx, tx, &r.book)
	if err != nil {
		return false, err
	}
	key := generatedKey(raw, r.snapshot, files, r.book.RelPath)
	if r.holds(ChaptersFromGenerated, key) {
		return true, nil
	}
	chs, err := fitGenerated(ctx, tx, &r.book, raw, r.snapshot, files)
	if err != nil || chs == nil {
		return false, err
	}
	return true, useChapters(ctx, tx, r, ChaptersFromGenerated, key, chs)
}

// fitGenerated is the stored chapters.json raw fitted onto book b's files
// (playFiles) when it applies to them (see above), else nil: the list's files
// are the book's and chapteralign answers a fill. snapshot is the book's
// scanned_chapters.
func fitGenerated(ctx context.Context, q rowQuerier, b *Book, raw, snapshot string, files []BookFile) ([]metadata.Chapter, error) {
	list := decodeAttachment[metadata.ChapterList](raw)
	if list == nil || !sameChapterFiles(list.Files, files, b.RelPath) {
		return nil, nil
	}
	own, err := scannedChapters(ctx, q, b.ID, snapshot)
	if err != nil {
		return nil, err
	}
	remote := make([]chapteralign.Remote, len(list.Chapters))
	for i, ch := range list.Chapters {
		remote[i] = chapteralign.Remote(ch)
	}
	res, err := chapteralign.Align(ctx, AlignFiles(files), own, remote, nil)
	if err != nil {
		return nil, err
	}
	// A fill is chapteralign's word for "the book has no chapters of its own":
	// any other fitted status means a file holds two chapters or more.
	if res.Status != chapteralign.Fill {
		return nil, nil
	}
	return res.Chapters, nil
}

// generatedFits reports whether the book at bookID has a stored chapters.json
// that applies to its files as they are now (fitGenerated): the chapters that
// would stand in were no chapter source chosen and the community's not to
// apply. The book page reads it to offer "Use automatic" over a choice.
func generatedFits(ctx context.Context, q querier, bookID int64) (bool, error) {
	b := Book{ID: bookID}
	var raw, snapshot string
	if err := q.QueryRowContext(ctx,
		`SELECT b.rel_path, b.is_folder, b.size, b.duration, b.scanned_chapters, COALESCE(a.chapters, '')
		   FROM books b LEFT JOIN book_attachments a ON a.book_id = b.id WHERE b.id = ?`, bookID).
		Scan(&b.RelPath, &b.IsFolder, &b.Size, &b.Duration, &snapshot, &raw); err != nil || raw == "" {
		return false, err
	}
	files, err := playFiles(ctx, q, &b)
	if err != nil {
		return false, err
	}
	chs, err := fitGenerated(ctx, q, &b, raw, snapshot, files)
	return chs != nil, err
}

// generatedKey names a generated fit (books.chapters_fit) by what it is
// fitted from: the stored chapters.json (raw), the scan's own chapters
// (snapshot) and the book's files in play order (each one's path relative to
// the book, size and length). JSON holds no NUL, nor does a path.
func generatedKey(raw, snapshot string, files []BookFile, bookPath string) string {
	var b strings.Builder
	b.WriteString(raw + "\x00" + snapshot)
	for _, f := range files {
		fmt.Fprintf(&b, "\x00%s\x00%d\x00%g", bookFileName(f.RelPath, bookPath), f.Size, f.Duration)
	}
	return chaptersHash(b.String())
}

// sameChapterFiles reports whether a chapters.json's files are the book's
// (files, library relative): as many, in the same order, each the same path
// relative to the book's folder (bookFileName) and the same size.
func sameChapterFiles(listed []metadata.ChapterListFile, files []BookFile, bookPath string) bool {
	if len(listed) == 0 || len(listed) != len(files) {
		return false
	}
	for i, f := range files {
		if listed[i].Path != bookFileName(f.RelPath, bookPath) || listed[i].Size != f.Size {
			return false
		}
	}
	return true
}

// bookFileName is a book's file (library relative) as a chapters.json names
// it: relative to the folder the book's .audiosilo folder is in, so a folder
// book's files as inBook has them, and a single-file book's one file by its
// name.
func bookFileName(file, bookPath string) string {
	return cmp.Or(inBook(file, bookPath), path.Base(file))
}

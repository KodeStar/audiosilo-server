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

// generatedFit is the book's generated chapters fitted onto its files (library
// relative, titles as found) and the fit's key (generatedKey: what it was
// fitted from), or no key when they don't apply (see above). When the rows
// already hold the fit of that key, the list is neither decoded nor fitted
// again: the key comes back with no chapters, and useChapters leaves the rows.
func generatedFit(ctx context.Context, tx *sql.Tx, r *chapterRow) ([]metadata.Chapter, string, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT chapters FROM book_attachments WHERE book_id = ?`, r.book.ID).Scan(&raw); err != nil {
		return nil, "", err
	}
	files, err := playFiles(ctx, tx, &r.book)
	if err != nil {
		return nil, "", err
	}
	key := generatedKey(raw, r.snapshot, files, r.book.RelPath)
	if r.holds(ChaptersFromGenerated, key) {
		return nil, key, nil
	}
	list := decodeAttachment[metadata.ChapterList](raw)
	if list == nil || !sameChapterFiles(list.Files, files, r.book.RelPath) {
		return nil, "", nil
	}
	own, err := scannedChapters(ctx, tx, r.book.ID, r.snapshot)
	if err != nil {
		return nil, "", err
	}
	remote := make([]chapteralign.Remote, len(list.Chapters))
	for i, ch := range list.Chapters {
		remote[i] = chapteralign.Remote(ch)
	}
	res, err := chapteralign.Align(ctx, AlignFiles(files), own, remote, nil)
	if err != nil {
		return nil, "", err
	}
	// A fill is chapteralign's word for "the book has no chapters of its own":
	// any other fitted status means a file holds two chapters or more.
	if res.Status != chapteralign.Fill {
		return nil, "", nil
	}
	return res.Chapters, key, nil
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

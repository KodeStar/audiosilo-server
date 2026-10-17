package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
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

// generatedFit is book id's generated chapters fitted onto its files (library
// relative, titles as found) and the fit's key (a hash of it relative to the
// book, as a community fit's), or nil when they don't apply (see above). raw is
// the stored chapters.json; snapshot the book's scanned_chapters.
func generatedFit(ctx context.Context, tx *sql.Tx, bookID int64, bookPath, snapshot, raw string) ([]metadata.Chapter, string, error) {
	list := decodeAttachment[metadata.ChapterList](raw)
	if list == nil {
		return nil, "", nil
	}
	files, err := chapterFiles(ctx, tx, bookID)
	if err != nil {
		return nil, "", err
	}
	if !sameChapterFiles(list.Files, files, bookPath) {
		return nil, "", nil
	}
	own, err := scannedChapters(ctx, tx, bookID, snapshot)
	if err != nil {
		return nil, "", err
	}
	alignFiles := make([]chapteralign.File, len(files))
	for i, f := range files {
		alignFiles[i] = chapteralign.File{Path: f.RelPath, Duration: f.Duration}
	}
	remote := make([]chapteralign.Remote, len(list.Chapters))
	for i, ch := range list.Chapters {
		remote[i] = chapteralign.Remote{Title: ch.Title, StartMS: ch.StartMS, LengthMS: ch.LengthMS}
	}
	res, err := chapteralign.Align(ctx, alignFiles, own, remote, nil)
	if err != nil {
		return nil, "", err
	}
	// A fill is chapteralign's word for "the book has no chapters of its own":
	// any other fitted status means a file holds two chapters or more.
	if res.Status != chapteralign.Fill {
		return nil, "", nil
	}
	key, err := json.Marshal(bookRelative(res.Chapters, bookPath))
	if err != nil {
		return nil, "", err
	}
	return res.Chapters, chaptersHash(string(key)), nil
}

// chapterFiles is book id's files in play order (library relative, with size
// and length): its book_files, or the book itself for a single-file book.
func chapterFiles(ctx context.Context, tx *sql.Tx, bookID int64) ([]BookFile, error) {
	var (
		relPath  string
		isFolder bool
		size     int64
		duration float64
	)
	if err := tx.QueryRowContext(ctx, `SELECT rel_path, is_folder, size, duration FROM books WHERE id = ?`, bookID).
		Scan(&relPath, &isFolder, &size, &duration); err != nil {
		return nil, err
	}
	files, err := queryRows(ctx, tx, func(r *sql.Rows, f *BookFile) error {
		return r.Scan(&f.RelPath, &f.Size, &f.Duration)
	}, `SELECT rel_path, size, duration FROM book_files WHERE book_id = ? ORDER BY seq`, bookID)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 && !isFolder {
		files = []BookFile{{RelPath: relPath, Size: size, Duration: duration}}
	}
	return files, nil
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
// it: relative to the folder the book's .audiosilo folder is in. A folder
// book's files are under its own folder (the library root for a book on it); a
// single-file book's one file is its name.
func bookFileName(file, bookPath string) string {
	switch {
	case file == bookPath:
		return path.Base(file)
	case bookPath == "." || bookPath == "":
		return file
	}
	return strings.TrimPrefix(file, bookPath+"/")
}

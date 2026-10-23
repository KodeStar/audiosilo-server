package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"path"
	"slices"
	"sort"
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
// book and its size; in play order, or in the byte order a book had before
// natural file order, #160, its chapters then moved with their files:
// chaptersOnFiles) and it fits their audio
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
	if list == nil {
		return nil, nil
	}
	chapters := chaptersOnFiles(list, files, b.RelPath)
	if chapters == nil {
		return nil, nil
	}
	own, err := scannedChapters(ctx, q, b.ID, snapshot)
	if err != nil {
		return nil, err
	}
	remote := make([]chapteralign.Remote, len(chapters))
	for i, ch := range chapters {
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

// chaptersOnFiles is a chapters.json's chapters on the book's files (files,
// library relative, in play order), or nil when the list was found on other
// files. Its files must be the book's: as many, each the same path relative to
// the book's folder (bookFileName) and the same size. In the same order its
// chapters are as listed; in another (a list the Bridge found while the book
// played its files in byte order, before #160) they move with their files
// (movedChapters).
func chaptersOnFiles(list *metadata.ChapterList, files []BookFile, bookPath string) []metadata.ChapterListEntry {
	order := listedOrder(list.Files, files, bookPath)
	if order == nil {
		return nil
	}
	if slices.IsSorted(order) {
		return list.Chapters
	}
	return movedChapters(list, order)
}

// listedOrder maps the book's files (files, in play order) onto a
// chapters.json's (listed): order[k] is the listed index of the book's k-th
// file. nil when the two are not the same files (see chaptersOnFiles).
func listedOrder(listed []metadata.ChapterListFile, files []BookFile, bookPath string) []int {
	if len(listed) == 0 || len(listed) != len(files) {
		return nil
	}
	at := make(map[string]int, len(listed))
	for i, f := range listed {
		if _, dup := at[f.Path]; dup {
			return nil
		}
		at[f.Path] = i
	}
	order := make([]int, len(files))
	for k, f := range files {
		i, ok := at[bookFileName(f.RelPath, bookPath)]
		if !ok || listed[i].Size != f.Size {
			return nil
		}
		order[k] = i
	}
	return order
}

// movedChapters lays a chapters.json's chapters out on its files played in
// another order (order, as listedOrder has it): each chapter starts at the
// same place in the same file (by the listed files' lengths), and runs to the
// next start in the new order. A file's audio before its first chapter start
// stays with the chapter before it in the new order, as nothing was heard to
// begin there; the first file's, when no chapter starts at its top, takes the
// title of the chapter it was in. nil when a listed file has no length (the
// chapters can't be placed in it) or a chapter would be empty.
func movedChapters(list *metadata.ChapterList, order []int) []metadata.ChapterListEntry {
	n := len(list.Files)
	listedAt := make([]int64, n+1) // where each listed file starts on the listed timeline
	for i, f := range list.Files {
		if f.DurationMS <= 0 {
			return nil
		}
		listedAt[i+1] = listedAt[i] + f.DurationMS
	}
	playedAt := make([]int64, n) // where each listed file starts in the new order
	var total int64
	for _, i := range order {
		playedAt[i] = total
		total += list.Files[i].DurationMS
	}
	// in is the listed file holding ms on the listed timeline (the last for a
	// start past its end).
	in := func(ms int64) int {
		return min(sort.Search(n, func(i int) bool { return listedAt[i+1] > ms }), n-1)
	}
	starts := make([]metadata.ChapterListEntry, 0, len(list.Chapters)+1)
	opens := false
	for _, ch := range list.Chapters {
		i := in(ch.StartMS)
		at := playedAt[i] + ch.StartMS - listedAt[i]
		opens = opens || at == 0
		starts = append(starts, metadata.ChapterListEntry{Title: ch.Title, StartMS: at})
	}
	if !opens {
		first, title := listedAt[order[0]], list.Chapters[0].Title
		for _, ch := range list.Chapters {
			if ch.StartMS <= first {
				title = ch.Title
			}
		}
		starts = append(starts, metadata.ChapterListEntry{Title: title})
	}
	slices.SortStableFunc(starts, func(a, b metadata.ChapterListEntry) int { return cmp.Compare(a.StartMS, b.StartMS) })
	for i := range starts {
		end := total
		if i+1 < len(starts) {
			end = starts[i+1].StartMS
		}
		if starts[i].LengthMS = end - starts[i].StartMS; starts[i].LengthMS <= 0 {
			return nil
		}
	}
	return starts
}

// bookFileName is a book's file (library relative) as a chapters.json names
// it: relative to the folder the book's .audiosilo folder is in, so a folder
// book's files as inBook has them, and a single-file book's one file by its
// name.
func bookFileName(file, bookPath string) string {
	return cmp.Or(inBook(file, bookPath), path.Base(file))
}

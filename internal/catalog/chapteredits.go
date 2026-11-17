package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"slices"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Edited chapters (server#240): an admin's own chapter list for a book, made in
// the console's chapter editor, stored in chapter_edits as a
// metadata.ChapterList (generator ChapterEditGenerator) with the files it was
// made on. It is the first chapter source (applyChapterSource): above a choice,
// the community's, the generated ones and the files'. It applies only while the
// book's files are those (chaptersOnFiles: count, book-relative path, size; in
// another order its chapters move with their files; a single-file book's one
// file by size alone, so a renamed file keeps it), placed on them by
// placeOnFiles at the same place in the same file, by the lengths it was made
// on; on other files it is dormant, never applied to other audio,
// and the book's chapters come from the next source. No chapter crosses a file:
// every file starts a chapter (its start is fixed, its title editable). While
// an edit applies the book's chapter renames (chapter_overrides) are dormant:
// its titles are the edit's.

// ChaptersFromEdited is an admin's own chapter list (chapter_edits) standing in
// for the book's chapters.
const ChaptersFromEdited = "edited"

// ChapterEditGenerator is the generator an edit's list is stored with.
const ChapterEditGenerator = "console"

// What a chapter edit's refusal says (ChapterEditError.Detail).
const (
	ChapterEditEmpty      = "empty"        // no chapters
	ChapterEditTooMany    = "too_many"     // over metadata.MaxChapterListEntries
	ChapterEditOrder      = "order"        // not in time order, at least minChapterGapMS apart
	ChapterEditOutOfRange = "out_of_range" // a start before 0 or at or past the book's end
	ChapterEditTitle      = "title"        // a title metadata.ListChapterTitle refuses
	ChapterEditFileStart  = "file_start"   // a file's start begins no chapter
)

// minChapterGapMS is how close two edited chapters may start. The console's
// editor mirrors these rules and limits (admin-ui chapters-edit-model.ts
// validateDraft): change both together.
const minChapterGapMS = 1000

// fileStartSnapMS is how far a chapter may start from a file's start and still
// be the chapter that file starts with (moved onto it): a client's rounding of
// the file's book offset (file starts are the sums of the files' lengths in
// whole milliseconds, as the basis gives them: fileStartsMS). Well under
// minChapterGapMS, so at most one chapter is that near.
const fileStartSnapMS = 50

// ErrChaptersBasis is a chapter edit made on other files than the book has now
// (its basis no longer the book's ChapterBasis).
var ErrChaptersBasis = errors.New("the book's files have changed since the chapters were read")

// ChapterEditError is a chapter edit refused for what it holds; Detail is one
// of the ChapterEdit* values.
type ChapterEditError struct{ Detail string }

func (e *ChapterEditError) Error() string { return "invalid chapters: " + e.Detail }

// EditedChapter is one chapter of an edit: its start on the whole-book
// timeline, in seconds (kept to the millisecond), and its title.
type EditedChapter struct {
	Start float64 `json:"start"`
	Title string  `json:"title"`
}

// ChapterBasis is the files a book's chapters are edited on, in play order:
// each one's path relative to the book (a single-file book's is its file
// name), size and length in milliseconds. The book page sends it and an edit
// sends it back, so an edit made on other files is refused (ErrChaptersBasis).
type ChapterBasis struct {
	Files []metadata.ChapterListFile `json:"files"`
}

// ChapterEdit is a book's stored edit on the console's book page: whether it
// applies (ChapterEditApplied) or is dormant (ChapterEditDormant: the book's
// files are not the ones it was made on), who saved it (null when the account
// is gone) and when.
type ChapterEdit struct {
	State    string  `json:"state"`
	EditedBy *string `json:"edited_by"`
	EditedAt string  `json:"edited_at"`
}

// ChapterEdit states.
const (
	ChapterEditApplied = "applied"
	ChapterEditDormant = "dormant"
)

// basisOf is the ChapterBasis of a book's files (playFiles).
func basisOf(files []BookFile, bookPath string) ChapterBasis {
	out := ChapterBasis{Files: make([]metadata.ChapterListFile, len(files))}
	for i, f := range files {
		out.Files[i] = metadata.ChapterListFile{Path: bookFileName(f.RelPath, bookPath), Size: f.Size, DurationMS: msOf(f.Duration)}
	}
	return out
}

// msOf is seconds in whole milliseconds.
func msOf(s float64) int64 { return int64(math.Round(s * 1000)) }

// fileStartsMS is where each of a book's files (in play order) starts on the
// whole-book timeline, in milliseconds, then where the last one ends: each the
// sum of the files' lengths in whole milliseconds before it (msOf, the basis'
// duration_ms), so a file start the console sums from the basis is exact
// however many files the book has.
func fileStartsMS(files []BookFile) []int64 {
	return msSums(len(files), func(i int) int64 { return msOf(files[i].Duration) })
}

// msSums is the running sums of n lengths (milliseconds): out[i] the sum of the
// first i, out[n] of all of them.
func msSums(n int, length func(int) int64) []int64 {
	out := make([]int64, n+1)
	for i := range n {
		out[i+1] = out[i] + length(i)
	}
	return out
}

// snapToFiles moves the chapter start (starts, milliseconds) within
// fileStartSnapMS of each file's start onto it, and reports whether every file
// starts a chapter. fileStarts are the files' starts (fileStartsMS without the
// end). Each file takes the nearest start in reach that no file before it
// took, so a file shorter than twice fileStartSnapMS never takes the chapter
// of the file before or after it.
func snapToFiles(starts, fileStarts []int64) bool {
	taken := make([]bool, len(starts))
	for _, fs := range fileStarts {
		best := -1
		for i, s := range starts {
			d := s - fs
			if taken[i] || d < -fileStartSnapMS || d > fileStartSnapMS {
				continue
			}
			if best < 0 || abs64(d) < abs64(starts[best]-fs) {
				best = i
			}
		}
		if best < 0 {
			return false
		}
		starts[best], taken[best] = fs, true
	}
	return true
}

// abs64 is n's distance from 0.
func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// checkChapterEdit is an edit's chapters as its list stores them (titles
// trimmed, each start in milliseconds, each file's start exact, each length
// to the next start or the book's end) when they hold on the book's files
// (in play order), else a *ChapterEditError naming the first rule broken: some
// chapters, at most metadata.MaxChapterListEntries; each start in the book
// and each title one a chapter list may hold; every file starting a chapter;
// the starts in order at least minChapterGapMS apart.
func checkChapterEdit(chapters []EditedChapter, files []BookFile) ([]metadata.ChapterListEntry, error) {
	refuse := func(detail string) ([]metadata.ChapterListEntry, error) {
		return nil, &ChapterEditError{Detail: detail}
	}
	switch {
	case len(chapters) == 0:
		return refuse(ChapterEditEmpty)
	case len(chapters) > metadata.MaxChapterListEntries:
		return refuse(ChapterEditTooMany)
	}
	fileStarts := fileStartsMS(files)
	end := fileStarts[len(files)]
	entries := make([]metadata.ChapterListEntry, len(chapters))
	starts := make([]int64, len(chapters))
	for i, ch := range chapters {
		// Compared in seconds first: a start too far out has no int64 form.
		if math.IsNaN(ch.Start) || ch.Start < 0 || ch.Start*1000 >= float64(end) {
			return refuse(ChapterEditOutOfRange)
		}
		if starts[i] = msOf(ch.Start); starts[i] >= end {
			return refuse(ChapterEditOutOfRange)
		}
		// One that shows nothing is no title, as for a rename (server#349).
		title, ok := metadata.ListChapterTitle(ch.Title)
		if !ok || blankIfInvisible(title) == "" {
			return refuse(ChapterEditTitle)
		}
		entries[i].Title = title
	}
	if !snapToFiles(starts, fileStarts[:len(files)]) {
		return refuse(ChapterEditFileStart)
	}
	for i := range starts {
		if i > 0 && starts[i]-starts[i-1] < minChapterGapMS {
			return refuse(ChapterEditOrder)
		}
		next := end
		if i+1 < len(starts) {
			next = starts[i+1]
		}
		entries[i].StartMS, entries[i].LengthMS = starts[i], next-starts[i]
	}
	return entries, nil
}

// placeOnFiles lays an edit's chapters (starts in milliseconds, in order, on
// the timeline of listed: the length the edit's list gives each of the book's
// files, in play order, as chaptersInOrder gives them) out on the book's files
// (library relative, in play order): each chapter in the file its start falls
// in, at the same place in it, from there to the next start or the file's end,
// its book offset on the files' lengths now (fileStartsMS). So a file that
// reads a little longer or shorter now (another probe of the same file) moves
// no chapter within its audio. nil when they don't fit the files: a start out
// of order or past the end, one at or past its file's end now, or a file
// starting no chapter (a start within fileStartSnapMS of a file's start is its
// first chapter). Pure: no I/O.
func placeOnFiles(entries []metadata.ChapterListEntry, files []BookFile, listed []int64) []metadata.Chapter {
	n := len(files)
	if len(entries) == 0 || n == 0 || len(listed) != n {
		return nil
	}
	at := msSums(n, func(i int) int64 { return listed[i] }) // where each file starts on the list's timeline
	now := fileStartsMS(files)                              // and on the book's as it is now
	starts := make([]int64, len(entries))
	for i, e := range entries {
		starts[i] = e.StartMS
	}
	if !snapToFiles(starts, at[:n]) {
		return nil
	}
	out := make([]metadata.Chapter, len(entries))
	fi := 0
	for i, s := range starts {
		if s < 0 || s >= at[n] || (i > 0 && s <= starts[i-1]) {
			return nil
		}
		for fi+1 < n && s >= at[fi+1] {
			fi++
		}
		f, off := files[fi], s-at[fi]
		if off >= msOf(f.Duration) {
			return nil // the file is shorter now than where the chapter starts in it
		}
		chEnd := f.Duration // to the file's end, unless a chapter starts before it
		if i+1 < len(starts) && starts[i+1] < at[fi+1] {
			chEnd = float64(starts[i+1]-at[fi]) / 1000
		}
		out[i] = metadata.Chapter{Index: i, Title: entries[i].Title, FileIndex: fi, FilePath: f.RelPath,
			Start: float64(off) / 1000, End: chEnd, BookOffset: float64(now[fi]+off) / 1000}
	}
	return out
}

// editedChapters is the stored edit raw placed on book b's files (playFiles,
// library relative), nil while it is dormant: other files, or chapters that
// no longer fit them.
func editedChapters(raw string, files []BookFile, b *Book) []metadata.Chapter {
	list := decodeAttachment[metadata.ChapterList](raw)
	if list == nil {
		return nil
	}
	// A single-file book's one file is the book: renamed, it is the book moved
	// (the edit moved with it: MoveDurableState), the same audio, so its size
	// alone is compared.
	if !b.IsFolder && len(list.Files) == 1 && len(files) == 1 {
		list.Files[0].Path = bookFileName(files[0].RelPath, b.RelPath)
	}
	order := listedOrder(list.Files, files, b.RelPath)
	if order == nil {
		return nil
	}
	listed := make([]int64, len(order))
	for k, i := range order {
		listed[k] = list.Files[i].DurationMS
	}
	return placeOnFiles(chaptersInOrder(list, order), files, listed)
}

// applyEdited puts the book's edited chapters (raw, its chapter_edits list) in
// its rows (useChapters, the fit named by listFitKey) and reports whether they
// apply, false (dormant) leaving the rows to the caller. When the rows already
// hold that fit, the list is not decoded again.
func applyEdited(ctx context.Context, tx *sql.Tx, r *chapterRow, raw string) (bool, error) {
	files, err := r.filesOf(ctx, tx)
	if err != nil {
		return false, err
	}
	key := listFitKey(raw, "", files, r.book.RelPath)
	if r.holds(ChaptersFromEdited, key) {
		return true, nil
	}
	chs := editedChapters(raw, files, &r.book)
	if chs == nil {
		return false, nil
	}
	return true, useChapters(ctx, tx, r, ChaptersFromEdited, key, chs)
}

// editableBook is the book at (libraryID, path) with its files in play order,
// for a chapter edit: ErrNotFound when none is indexed there,
// ErrNotBookLibrary in a podcast library (an episode's chapters are its
// feed's).
func editableBook(ctx context.Context, q querier, libraryID int64, path string) (*Book, []BookFile, error) {
	b := &Book{LibraryID: libraryID, RelPath: path}
	var kind string
	err := q.QueryRowContext(ctx,
		`SELECT b.id, b.is_folder, b.size, b.duration, l.kind FROM books b JOIN libraries l ON l.id = b.library_id
		  WHERE b.library_id = ? AND b.rel_path = ?`, libraryID, path).
		Scan(&b.ID, &b.IsFolder, &b.Size, &b.Duration, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if kind == KindPodcasts {
		return nil, nil, ErrNotBookLibrary
	}
	files, err := playFiles(ctx, q, b)
	return b, files, err
}

// SaveChapterEdit stores chapters as the book's own list (chapter_edits,
// replacing any) by userID, made on basis, and rebuilds the book's chapters
// from it, in one transaction. ErrNotFound when no book is indexed at the
// path, ErrNotBookLibrary in a podcast library, ErrChaptersBasis when basis is
// not the book's files now, a *ChapterEditError for chapters that don't hold
// on them (checkChapterEdit).
func (c *Catalog) SaveChapterEdit(ctx context.Context, libraryID int64, path string, basis ChapterBasis, chapters []EditedChapter, userID int64) error {
	path = CleanRelPath(path)
	err := c.db.WithTx(ctx, "SaveChapterEdit", func(tx *sql.Tx) error {
		b, files, err := editableBook(ctx, tx, libraryID, path)
		if err != nil {
			return err
		}
		current := basisOf(files, path)
		if !slices.Equal(current.Files, basis.Files) {
			return ErrChaptersBasis
		}
		entries, err := checkChapterEdit(chapters, files)
		if err != nil {
			return err
		}
		now := c.ts()
		raw, err := json.Marshal(metadata.ChapterList{Version: metadata.ChapterListVersion, Generator: ChapterEditGenerator,
			GeneratedAt: now, Files: current.Files, Chapters: entries})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chapter_edits(library_id, path, list, updated_by, updated_at) VALUES(?,?,?,?,?)
			 ON CONFLICT(library_id, path) DO UPDATE SET
			     list = excluded.list, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			libraryID, path, string(raw), nullableID(userID), now); err != nil {
			return err
		}
		return refreshEffective(ctx, tx, b.ID)
	})
	if err == nil {
		c.changed()
	}
	return err
}

// RevertChapterEdit removes the book's edit, so its chapters come from the
// next source again (and its renames apply again), and reports whether it had
// one. ErrNotFound when no book is indexed at the path.
func (c *Catalog) RevertChapterEdit(ctx context.Context, libraryID int64, path string) (bool, error) {
	path = CleanRelPath(path)
	var removed bool
	err := c.db.WithTx(ctx, "RevertChapterEdit", func(tx *sql.Tx) error {
		id, err := bookIDByPath(ctx, tx, libraryID, path)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM chapter_edits WHERE library_id = ? AND path = ?`, libraryID, path)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		removed = true
		return refreshEffective(ctx, tx, id)
	})
	if err == nil && removed {
		c.changed()
	}
	return removed, err
}

// chapterEditOf is the book's stored edit for its page, nil when it has none;
// applied says whether the book's chapters are it now.
func chapterEditOf(ctx context.Context, q querier, libraryID int64, path string, applied bool) (*ChapterEdit, error) {
	e := ChapterEdit{State: ChapterEditDormant}
	if applied {
		e.State = ChapterEditApplied
	}
	err := q.QueryRowContext(ctx,
		`SELECT ce.updated_at, u.username FROM chapter_edits ce LEFT JOIN users u ON u.id = ce.updated_by
		  WHERE ce.library_id = ? AND ce.path = ?`, libraryID, path).Scan(&e.EditedAt, &e.EditedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

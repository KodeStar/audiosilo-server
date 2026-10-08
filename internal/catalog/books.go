package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// UpsertBook inserts or updates a book keyed by (library_id, rel_path) with the
// values the scan found, replaces its files and chapters, then layers enrichment and
// metadata overrides on top and refreshes the FTS row (refreshEffective) - all in one
// transaction, so an edited field is never visible with its scanned value. It
// returns the book ID.
func (c *Catalog) UpsertBook(ctx context.Context, b *Book) (int64, error) {
	// The snapshot carries this upsert's indexed_at, which is how loadLayers tells
	// it from one an older server left stale (see scannedStampKey).
	indexedAt := c.ts()
	scanned, err := scannedJSON(b, indexedAt)
	if err != nil {
		return 0, err
	}
	// The scan's own chapters, kept as found (books.scanned_chapters): what a
	// community chapter check fits against, and what the chapters go back to.
	ownChapters := b.Chapters
	if ownChapters == nil {
		ownChapters = []metadata.Chapter{}
	}
	scannedChapters, err := json.Marshal(ownChapters)
	if err != nil {
		return 0, err
	}
	// has_cover holds whenever there is a sibling cover; the scanner reports
	// embedded art. Unknown (nil) stays NULL until a scan checks.
	hasCover := b.HasCover
	if b.CoverPath != "" {
		yes := true
		hasCover = &yes
	}
	var id int64
	err = c.db.WithTx(ctx, "UpsertBook", func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO books(library_id, rel_path, is_folder, title, author, series,
			     series_index, narrator, duration, asin, isbn, cover_path, format, codec, size,
			     mtime, content_hash, indexed_at, added_at, published, description, has_cover, scanned,
			     scan_error, scan_error_file, scan_error_detail, suspect_parts, split_parent,
			     released, released_checked,
			     scanned_chapters, chapters_hash, chapters_source, chapters_fit)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,'','')
			 ON CONFLICT(library_id, rel_path) DO UPDATE SET
			     is_folder=excluded.is_folder, title=excluded.title, author=excluded.author,
			     series=excluded.series, series_index=excluded.series_index,
			     narrator=excluded.narrator, duration=excluded.duration, asin=excluded.asin,
			     isbn=excluded.isbn, cover_path=excluded.cover_path, format=excluded.format,
			     codec=excluded.codec, size=excluded.size, mtime=excluded.mtime,
			     content_hash=excluded.content_hash, indexed_at=excluded.indexed_at,
			     published=excluded.published, description=excluded.description,
			     has_cover=excluded.has_cover, scanned=excluded.scanned,
			     scan_error=excluded.scan_error, scan_error_file=excluded.scan_error_file,
			     scan_error_detail=excluded.scan_error_detail,
			     suspect_parts=excluded.suspect_parts, split_parent=excluded.split_parent,
			     released=excluded.released, released_checked=1,
			     -- The rows below are the scan's: refreshEffective puts a community
			     -- list back over them when it should.
			     scanned_chapters=excluded.scanned_chapters, chapters_hash=excluded.chapters_hash,
			     chapters_source='', chapters_fit=''
			     -- added_at intentionally not updated: it records first-seen, so a
			     -- re-index of an existing book keeps its original added date.
			 RETURNING id`,
			b.LibraryID, b.RelPath, b.IsFolder, b.Title, b.Author, b.Series,
			b.SeriesIndex, b.Narrator, b.Duration, b.ASIN, b.ISBN, b.CoverPath,
			b.Format, b.Codec, b.Size, b.MTime, b.ContentHash, indexedAt, b.AddedAt,
			b.Published, b.Description, hasCover, scanned,
			b.ScanError, b.ScanErrorFile, b.ScanErrorDetail, b.SuspectParts, b.SplitParent,
			b.Released, string(scannedChapters), chaptersHash(string(scannedChapters))).Scan(&id); err != nil {
			return err
		}
		b.ID = id
		if err := refreshCoverArt(ctx, tx, b.LibraryID, b.RelPath); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM book_files WHERE book_id = ?`, id); err != nil {
			return err
		}
		for _, f := range b.Files {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO book_files(book_id, rel_path, seq, duration, format, codec, size)
				 VALUES(?,?,?,?,?,?,?)`, id, f.RelPath, f.Seq, f.Duration, f.Format, f.Codec, f.Size); err != nil {
				return err
			}
		}

		if err := replaceChapters(ctx, tx, id, b.Chapters); err != nil {
			return err
		}
		return refreshEffective(ctx, tx, id)
	})
	if err != nil {
		return 0, err
	}
	c.changed()
	return id, nil
}

// BooksByPaths returns the books in a library whose rel_path is in paths, keyed
// by rel_path. It is used to annotate filesystem-view entries with their
// book id and metadata (the hybrid view), so a user who browses to a file or
// book folder can act on it directly. Files and book folders both match here.
// Any number of paths may be asked for (a whole folder, for GET /next): they are
// read in chunks that stay well under SQLite's bound-parameter limit.
func (c *Catalog) BooksByPaths(ctx context.Context, libraryID int64, paths []string) (map[string]Book, error) {
	out := map[string]Book{}
	const chunk = 500
	for start := 0; start < len(paths); start += chunk {
		part := paths[start:min(start+chunk, len(paths))]
		args := make([]any, 0, len(part)+1)
		args = append(args, libraryID)
		for _, p := range part {
			args = append(args, p)
		}
		if err := c.booksByPaths(ctx, out, `SELECT `+bookCols+` FROM books WHERE library_id = ? AND rel_path IN (`+
			placeholders(len(part))+`)`, args); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// booksByPaths runs one BooksByPaths query into out.
func (c *Catalog) booksByPaths(ctx context.Context, out map[string]Book, q string, args []any) error {
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return err
		}
		out[b.RelPath] = *b
	}
	return rows.Err()
}

// BookIdentifiers is what BooksByRefs reads of the book at one Ref: its path
// (cleaned, CleanRelPath) and its identifiers. The zero value, RelPath "", means
// no book is indexed there.
type BookIdentifiers struct {
	RelPath string
	ASIN    string
	ISBN    string
}

// BooksByRefs returns the identifiers of the book at each ref, aligned to refs:
// a ref naming no indexed book, or no library, is the zero value. One query per
// library, selecting only the columns the answer holds.
func (c *Catalog) BooksByRefs(ctx context.Context, refs []Ref) ([]BookIdentifiers, error) {
	paths, byLib := groupRefs(refs)
	found := make(map[Ref]BookIdentifiers, len(refs))
	for libID, ps := range byLib {
		if err := c.identifiersIn(ctx, libID, ps, found); err != nil {
			return nil, err
		}
	}
	out := make([]BookIdentifiers, len(refs))
	for i, r := range refs {
		out[i] = found[Ref{LibraryID: r.LibraryID, Path: paths[i]}]
	}
	return out, nil
}

// identifiersIn adds the identifiers of the books at paths in one library to
// found, keyed by ref.
func (c *Catalog) identifiersIn(ctx context.Context, libraryID int64, paths []string, found map[Ref]BookIdentifiers) error {
	args := make([]any, 0, len(paths)+1)
	args = append(args, libraryID)
	for _, p := range paths {
		args = append(args, p)
	}
	rows, err := c.db.QueryContext(ctx, `SELECT rel_path, asin, isbn FROM books
		 WHERE library_id = ? AND rel_path IN (`+placeholders(len(paths))+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b BookIdentifiers
		if err := rows.Scan(&b.RelPath, &b.ASIN, &b.ISBN); err != nil {
			return err
		}
		found[Ref{LibraryID: libraryID, Path: b.RelPath}] = b
	}
	return rows.Err()
}

// groupRefs cleans each ref's path (CleanRelPath), returning the cleaned paths in
// ref order and the same paths grouped by library, for one query per library.
func groupRefs(refs []Ref) (paths []string, byLib map[int64][]string) {
	paths = make([]string, len(refs))
	byLib = map[int64][]string{}
	for i, r := range refs {
		paths[i] = CleanRelPath(r.Path)
		byLib[r.LibraryID] = append(byLib[r.LibraryID], paths[i])
	}
	return paths, byLib
}

// Signature captures the on-disk fingerprint used to skip unchanged books, plus
// the stored Duration/Codec so the scanner can re-probe entries that predate a
// metadata column (e.g. codec) and never had it backfilled.
type Signature struct {
	MTime       int64
	Size        int64
	Duration    float64
	Codec       string
	ContentHash string
	CoverPath   string
	HasCover    *bool // nil = never checked (indexed before migration 0016)
	IsFolder    bool
	// SuspectUnchecked: a folder book whose parts haven't been checked for holding
	// several books (indexed before migration 0017; see books.suspect_parts).
	SuspectUnchecked bool
	// ReleasedUnchecked: a book whose tags haven't been read for a release date
	// (indexed before migration 0037; see books.released_checked), which the next
	// scan reads (SetReleased).
	ReleasedUnchecked bool
	// ScanError and ScanErrorFile are the read problem the last indexing recorded
	// (books.scan_error), so a scan can look at that file again.
	ScanError, ScanErrorFile string
	// SplitParent is books.split_parent as stored, so a scan records a change to it
	// (a disc folder's sibling added or gone) without re-indexing the book.
	SplitParent string
}

// Signatures returns the stored mtime/size for every book in a library, keyed
// by rel_path. The scanner uses it to skip re-extracting unchanged books.
func (c *Catalog) Signatures(ctx context.Context, libraryID int64) (map[string]Signature, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT rel_path, mtime, size, duration, codec, content_hash, cover_path, has_cover,
		        is_folder, suspect_parts IS NULL, scan_error, scan_error_file, split_parent,
		        released_checked = 0
		   FROM books WHERE library_id = ?`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Signature{}
	for rows.Next() {
		var rel string
		var sig Signature
		if err := rows.Scan(&rel, &sig.MTime, &sig.Size, &sig.Duration, &sig.Codec, &sig.ContentHash,
			&sig.CoverPath, &sig.HasCover, &sig.IsFolder, &sig.SuspectUnchecked,
			&sig.ScanError, &sig.ScanErrorFile, &sig.SplitParent, &sig.ReleasedUnchecked); err != nil {
			return nil, err
		}
		out[rel] = sig
	}
	return out, rows.Err()
}

// SetHasCover records whether books have cover art, by path, in one transaction
// (the scanner's backfill for rows indexed before the column existed). A sibling
// cover image still counts, whatever the flag says.
func (c *Catalog) SetHasCover(ctx context.Context, libraryID int64, flags map[string]bool) error {
	if len(flags) == 0 {
		return nil
	}
	return c.db.WithTx(ctx, "SetHasCover", func(tx *sql.Tx) error {
		for relPath, has := range flags {
			if _, err := tx.ExecContext(ctx,
				`UPDATE books SET has_cover = (? OR cover_path <> '') WHERE library_id = ? AND rel_path = ?`,
				has, libraryID, relPath); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetSuspectParts records, by path, how many separate books a folder book's parts
// look like (the scanner's check of rows indexed before the column existed), in one
// transaction.
func (c *Catalog) SetSuspectParts(ctx context.Context, libraryID int64, parts map[string]int) error {
	if len(parts) == 0 {
		return nil
	}
	return c.db.WithTx(ctx, "SetSuspectParts", func(tx *sql.Tx) error {
		for relPath, n := range parts {
			if _, err := tx.ExecContext(ctx,
				`UPDATE books SET suspect_parts = ? WHERE library_id = ? AND rel_path = ?`,
				n, libraryID, relPath); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetSplitParent records, by path, which books are discs of a book split across
// disc folders (the folder holding them; "" = not one), for books a scan left
// unchanged, in one transaction.
func (c *Catalog) SetSplitParent(ctx context.Context, libraryID int64, parents map[string]string) error {
	if len(parents) == 0 {
		return nil
	}
	return c.db.WithTx(ctx, "SetSplitParent", func(tx *sql.Tx) error {
		for relPath, parent := range parents {
			if _, err := tx.ExecContext(ctx,
				`UPDATE books SET split_parent = ? WHERE library_id = ? AND rel_path = ?`,
				parent, libraryID, relPath); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetReleased records, by path, the release date a read of each book's primary
// file found (the scanner's backfill for rows indexed before migration 0037),
// marking it checked, in one transaction.
func (c *Catalog) SetReleased(ctx context.Context, libraryID int64, dates map[string]string) error {
	if len(dates) == 0 {
		return nil
	}
	return c.db.WithTx(ctx, "SetReleased", func(tx *sql.Tx) error {
		for relPath, date := range dates {
			if _, err := tx.ExecContext(ctx,
				`UPDATE books SET released = ?, released_checked = 1 WHERE library_id = ? AND rel_path = ?`,
				date, libraryID, relPath); err != nil {
				return err
			}
		}
		return nil
	})
}

// SplitFolders reports which of folders (library-relative) hold a book split across
// disc folders whose discs are indexed as books of their own: a split_parent of
// some book. The admin console offers "Always one book", which joins them, on
// those (and on a folder already joined).
func (c *Catalog) SplitFolders(ctx context.Context, libraryID int64, folders []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(folders) == 0 {
		return out, nil
	}
	args := []any{libraryID}
	for _, f := range folders {
		args = append(args, f)
	}
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT split_parent FROM books
		WHERE library_id = ? AND split_parent <> '' AND split_parent IN (`+
		strings.Repeat("?,", len(folders)-1)+`?)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// DeleteBooksNotIn removes books in a library whose rel_path is not in keep and
// returns their paths. Used by the scanner to prune vanished files (the paths go
// in the scan's log). The removal is one transaction: all of the books go, with
// their search rows, or none do.
func (c *Catalog) DeleteBooksNotIn(ctx context.Context, libraryID int64, keep map[string]bool) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, rel_path FROM books WHERE library_id = ?`, libraryID)
	if err != nil {
		return nil, err
	}
	var stale []int64
	var paths []string
	for rows.Next() {
		var id int64
		var rel string
		if err := rows.Scan(&id, &rel); err != nil {
			rows.Close()
			return nil, err
		}
		if !keep[rel] {
			stale = append(stale, id)
			paths = append(paths, rel)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(stale) == 0 {
		return nil, nil
	}
	if err := c.db.WithTx(ctx, "DeleteBooksNotIn", func(tx *sql.Tx) error {
		for _, id := range stale {
			if _, err := tx.ExecContext(ctx, `DELETE FROM books WHERE id = ?`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM books_fts WHERE rowid = ?`, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return paths, nil
}

const bookCols = `id, library_id, rel_path, is_folder, title, author, series,
	series_index, narrator, duration, asin, isbn, cover_path, format, codec, size, mtime,
	added_at, content_hash, published, has_cover, cover_art, cover_color`

// bookDest returns the scan destinations for bookCols, in order, so every query
// selecting bookCols (plain or prefixed) scans it the same way; finish, called
// after the scan, derives the cover fields from the columns behind them.
func bookDest(b *Book) (dest []any, finish func()) {
	var art, color string
	return []any{&b.ID, &b.LibraryID, &b.RelPath, &b.IsFolder, &b.Title, &b.Author,
			&b.Series, &b.SeriesIndex, &b.Narrator, &b.Duration, &b.ASIN, &b.ISBN,
			&b.CoverPath, &b.Format, &b.Codec, &b.Size, &b.MTime, &b.AddedAt, &b.ContentHash,
			&b.Published, &b.HasCover, &art, &color},
		func() {
			b.CoverVersion = CoverVersion(art)
			b.CoverColor, _ = storedCoverColor(art, color)
		}
}

func scanBook(row interface{ Scan(...any) error }) (*Book, error) {
	var b Book
	dest, finish := bookDest(&b)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	finish()
	return &b, nil
}

// GetBookByPath returns a book by its library + relative path, including files
// and chapters. Used by the resolve endpoint to map a browsed path to a book.
func (c *Catalog) GetBookByPath(ctx context.Context, libraryID int64, relPath string) (*Book, error) {
	id, err := bookIDByPath(ctx, c.db, libraryID, relPath)
	if err != nil {
		return nil, err
	}
	return c.GetBook(ctx, id)
}

// GetBookHolding returns the indexed folder book holding relPath below its own
// path: a folder book above relPath one of whose files is relPath or lies under it
// (a part of a folder book; a disc folder of a joined book, or a file in one). The
// innermost such book wins; ErrNotFound when there is none. It answers a path a
// client still holds after a join (a disc folder's) from the index, where
// GetBookByPath, by the book's own path, finds nothing.
func (c *Catalog) GetBookHolding(ctx context.Context, libraryID int64, relPath string) (*Book, error) {
	args := []any{libraryID}
	for d := path.Dir(relPath); d != "." && d != "/"; d = path.Dir(d) {
		args = append(args, d)
	}
	if len(args) == 1 {
		return nil, ErrNotFound
	}
	n := len(args) - 1
	args = append(args, relPath, escapeLike(relPath)+"/%")
	var id int64
	err := c.db.QueryRowContext(ctx,
		`SELECT b.id FROM books b
		  WHERE b.library_id = ? AND b.is_folder = 1 AND b.rel_path IN (`+placeholders(n)+`)
		    AND EXISTS (SELECT 1 FROM book_files f WHERE f.book_id = b.id
		                  AND (f.rel_path = ? OR f.rel_path LIKE ? ESCAPE '\'))
		  ORDER BY length(b.rel_path) DESC LIMIT 1`, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c.GetBook(ctx, id)
}

// GetBook returns a book by ID including its files, chapters and description
// (which only this single-book read loads).
func (c *Catalog) GetBook(ctx context.Context, id int64) (*Book, error) {
	var b Book
	dest, finish := bookDest(&b)
	err := c.db.QueryRowContext(ctx, `SELECT `+bookCols+`, description, chapters_source FROM books WHERE id = ?`, id).
		Scan(append(dest, &b.Description, &b.ChaptersSource)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	finish()
	if err := c.loadFiles(ctx, &b); err != nil {
		return nil, err
	}
	if err := c.loadChapters(ctx, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (c *Catalog) loadFiles(ctx context.Context, b *Book) error {
	rows, err := c.db.QueryContext(ctx,
		`SELECT rel_path, seq, duration, format, codec, size FROM book_files WHERE book_id = ? ORDER BY seq`, b.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f BookFile
		if err := rows.Scan(&f.RelPath, &f.Seq, &f.Duration, &f.Format, &f.Codec, &f.Size); err != nil {
			return err
		}
		b.Files = append(b.Files, f)
	}
	return rows.Err()
}

func (c *Catalog) loadChapters(ctx context.Context, b *Book) error {
	rows, err := c.db.QueryContext(ctx,
		`SELECT idx, title, file_index, file_path, start, "end", book_offset
		   FROM chapters WHERE book_id = ? ORDER BY idx`, b.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ch metadata.Chapter
		if err := rows.Scan(&ch.Index, &ch.Title, &ch.FileIndex, &ch.FilePath, &ch.Start, &ch.End, &ch.BookOffset); err != nil {
			return err
		}
		b.Chapters = append(b.Chapters, ch)
	}
	return rows.Err()
}

// ListOptions controls book listing.
type ListOptions struct {
	LibraryID int64
	Author    string // optional: the whole credit, or exactly one person it names (creditFilter)
	Series    string // optional exact-match filter
	Narrator  string // optional, as Author
	Sort      string // "author" (default) | "title" | "recent"
	Limit     int
	Cursor    string // opaque keyset cursor from a previous page
	Scope     *Scope // optional access scope; nil = unrestricted (admin/internal)
}

// Page is a page of books plus the cursor for the next page ("" when exhausted).
type Page struct {
	Books      []Book `json:"books"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// sortColumn is the textual column used by a sort's keyset. "recent" orders by
// added_at (filesystem-derived, set by the scanner) so the order is stable across
// re-indexes; id breaks ties.
func sortColumn(sort string) (col string, asc bool) {
	switch sort {
	case "title":
		return "title", true
	case "recent":
		return "added_at", false
	default:
		return "author", true
	}
}

// CountBooksByLibrary returns the number of indexed books per library id. Used
// by the admin stats view; cheap (a single grouped count over the index).
func (c *Catalog) CountBooksByLibrary(ctx context.Context) (map[int64]int, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT library_id, COUNT(*) FROM books GROUP BY library_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var libID int64
		var n int
		if err := rows.Scan(&libID, &n); err != nil {
			return nil, err
		}
		out[libID] = n
	}
	return out, rows.Err()
}

// ListBooks returns a page of books using keyset pagination so paging stays
// O(1) regardless of how deep into a large library the caller is.
func (c *Catalog) ListBooks(ctx context.Context, opt ListOptions) (*Page, error) {
	col, asc := sortColumn(opt.Sort)
	if opt.Limit <= 0 || opt.Limit > 200 {
		opt.Limit = 50
	}

	where := []string{"library_id = ?"}
	args := []any{opt.LibraryID}
	if opt.Author != "" {
		cond, a := creditFilter("", "author", opt.Author)
		where, args = append(where, cond), append(args, a...)
	}
	if opt.Series != "" {
		where = append(where, "series = ?")
		args = append(args, opt.Series)
	}
	if opt.Narrator != "" {
		cond, a := creditFilter("", "narrator", opt.Narrator)
		where, args = append(where, cond), append(args, a...)
	}
	// Restrict to the caller's access scope (share path rules), if provided.
	if opt.Scope != nil {
		frag, fargs := pathFilterSQL("rel_path", *opt.Scope)
		where = append(where, frag)
		args = append(args, fargs...)
	}

	cmp, order := ">", "ASC"
	if !asc {
		cmp, order = "<", "DESC"
	}
	if opt.Cursor != "" {
		cval, cid, err := decodeCursor(opt.Cursor)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
		}
		// sortColumn always yields a non-empty column, so paginate on the
		// (col, id) row-value keyset - index-friendly and stable across ties.
		where = append(where, fmt.Sprintf("(%s, id) %s (?, ?)", col, cmp))
		args = append(args, cval, cid)
	}

	orderBy := col + " " + order + ", id " + order
	query := fmt.Sprintf(`SELECT %s FROM books WHERE %s ORDER BY %s LIMIT ?`,
		bookCols, strings.Join(where, " AND "), orderBy)
	args = append(args, opt.Limit+1) // fetch one extra to detect a next page

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	page := &Page{}
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		page.Books = append(page.Books, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Books) > opt.Limit {
		page.Books = page.Books[:opt.Limit]
		last := page.Books[len(page.Books)-1]
		page.NextCursor = encodeCursor(sortValue(&last, col), last.ID)
	}
	return page, nil
}

// RecentBooks returns the most recently added books across the caller's accessible
// libraries (newest first), each restricted to that library's share path rules.
// A single cross-library query - unlike per-library ListBooks - so a client can
// render one merged "recently added" list without fanning out and concatenating.
func (c *Catalog) RecentBooks(ctx context.Context, scopes []Scope, limit int) ([]Book, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var conds []string
	var args []any
	for _, s := range scopes {
		frag, fargs := pathFilterSQL("b.rel_path", s)
		conds = append(conds, "(b.library_id = ? AND "+frag+")")
		args = append(args, s.LibraryID)
		args = append(args, fargs...)
	}
	// Over-fetch so de-dup can still return up to `limit` distinct books.
	args = append(args, dedupFetch(limit))
	q := `SELECT ` + prefixCols("b.") + dedupCols + `
		FROM books b` + dedupJoins + `
		WHERE (` + strings.Join(conds, " OR ") + `)
		ORDER BY b.added_at DESC, b.id DESC LIMIT ?`
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cands []candidate
	for rows.Next() {
		cand, err := scanCandidate(rows, len(cands))
		if err != nil {
			return nil, err
		}
		cands = append(cands, cand)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dedupBooks(cands, limit), nil
}

func sortValue(b *Book, col string) string {
	switch col {
	case "title":
		return b.Title
	case "author":
		return b.Author
	case "added_at":
		return b.AddedAt
	default:
		return ""
	}
}

func encodeCursor(val string, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(val + "\x00" + strconv.FormatInt(id, 10)))
}

func decodeCursor(s string) (string, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", 0, err
	}
	// Split at the LAST NUL, encodeCursor's exact inverse: the id never holds
	// one, so a sort value that does still round-trips.
	i := bytes.LastIndexByte(raw, 0)
	if i < 0 {
		return "", 0, errors.New("malformed cursor")
	}
	id, err := strconv.ParseInt(string(raw[i+1:]), 10, 64)
	if err != nil {
		return "", 0, err
	}
	return string(raw[:i]), id, nil
}

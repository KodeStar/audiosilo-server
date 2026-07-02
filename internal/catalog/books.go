package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// UpsertBook inserts or updates a book keyed by (library_id, rel_path), then
// replaces its files and chapters and refreshes the FTS row. It returns the
// book ID.
func (c *Catalog) UpsertBook(ctx context.Context, b *Book) (int64, error) {
	var id int64
	err := c.db.WithTx(ctx, "UpsertBook", func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO books(library_id, rel_path, is_folder, title, author, series,
			     series_index, narrator, duration, asin, isbn, cover_path, format, codec, size,
			     mtime, content_hash, indexed_at, added_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(library_id, rel_path) DO UPDATE SET
			     is_folder=excluded.is_folder, title=excluded.title, author=excluded.author,
			     series=excluded.series, series_index=excluded.series_index,
			     narrator=excluded.narrator, duration=excluded.duration, asin=excluded.asin,
			     isbn=excluded.isbn, cover_path=excluded.cover_path, format=excluded.format,
			     codec=excluded.codec, size=excluded.size, mtime=excluded.mtime,
			     content_hash=excluded.content_hash, indexed_at=excluded.indexed_at
			     -- added_at intentionally not updated: it records first-seen, so a
			     -- re-index of an existing book keeps its original added date.
			 RETURNING id`,
			b.LibraryID, b.RelPath, b.IsFolder, b.Title, b.Author, b.Series,
			b.SeriesIndex, b.Narrator, b.Duration, b.ASIN, b.ISBN, b.CoverPath,
			b.Format, b.Codec, b.Size, b.MTime, b.ContentHash, c.ts(), b.AddedAt).Scan(&id); err != nil {
			return err
		}
		b.ID = id

		if _, err := tx.ExecContext(ctx, `DELETE FROM book_files WHERE book_id = ?`, id); err != nil {
			return err
		}
		for _, f := range b.Files {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO book_files(book_id, rel_path, seq, duration, format, size)
				 VALUES(?,?,?,?,?,?)`, id, f.RelPath, f.Seq, f.Duration, f.Format, f.Size); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM chapters WHERE book_id = ?`, id); err != nil {
			return err
		}
		for _, ch := range b.Chapters {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO chapters(book_id, idx, title, file_index, file_path, start, "end", book_offset)
				 VALUES(?,?,?,?,?,?,?,?)`,
				id, ch.Index, ch.Title, ch.FileIndex, ch.FilePath, ch.Start, ch.End, ch.BookOffset); err != nil {
				return err
			}
		}

		// Refresh FTS: delete-then-insert keyed by rowid = book id.
		if _, err := tx.ExecContext(ctx, `DELETE FROM books_fts WHERE rowid = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO books_fts(rowid, title, author, series, narrator) VALUES(?,?,?,?,?)`,
			id, b.Title, b.Author, b.Series, b.Narrator); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// BooksByPaths returns the books in a library whose rel_path is in paths, keyed
// by rel_path. It is used to annotate filesystem-view entries with their
// book id and metadata (the hybrid view), so a user who browses to a file or
// book folder can act on it directly. Files and book folders both match here.
func (c *Catalog) BooksByPaths(ctx context.Context, libraryID int64, paths []string) (map[string]Book, error) {
	out := map[string]Book{}
	if len(paths) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(paths))
	args := []any{libraryID}
	for i, p := range paths {
		placeholders[i] = "?"
		args = append(args, p)
	}
	q := `SELECT ` + bookCols + ` FROM books WHERE library_id = ? AND rel_path IN (` +
		strings.Join(placeholders, ",") + `)`
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		out[b.RelPath] = *b
	}
	return out, rows.Err()
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
}

// FingerprintsForPaths returns the stored content fingerprint (content_hash)
// for the given rel_paths in a library, keyed by rel_path. Used by the scanner
// to match a vanished file to a newly-appeared one (move detection).
func (c *Catalog) FingerprintsForPaths(ctx context.Context, libraryID int64, paths []string) (map[string]string, error) {
	out := map[string]string{}
	if len(paths) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(paths))
	args := []any{libraryID}
	for i, p := range paths {
		placeholders[i] = "?"
		args = append(args, p)
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT rel_path, content_hash FROM books WHERE library_id = ? AND rel_path IN (`+
			strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rel, fp string
		if err := rows.Scan(&rel, &fp); err != nil {
			return nil, err
		}
		out[rel] = fp
	}
	return out, rows.Err()
}

// Signatures returns the stored mtime/size for every book in a library, keyed
// by rel_path. The scanner uses it to skip re-extracting unchanged books.
func (c *Catalog) Signatures(ctx context.Context, libraryID int64) (map[string]Signature, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT rel_path, mtime, size, duration, codec, content_hash FROM books WHERE library_id = ?`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Signature{}
	for rows.Next() {
		var rel string
		var sig Signature
		if err := rows.Scan(&rel, &sig.MTime, &sig.Size, &sig.Duration, &sig.Codec, &sig.ContentHash); err != nil {
			return nil, err
		}
		out[rel] = sig
	}
	return out, rows.Err()
}

// DeleteBooksNotIn removes books in a library whose rel_path is not in keep.
// Returns the number deleted. Used by the scanner to prune vanished files.
func (c *Catalog) DeleteBooksNotIn(ctx context.Context, libraryID int64, keep map[string]bool) (int, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, rel_path FROM books WHERE library_id = ?`, libraryID)
	if err != nil {
		return 0, err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var rel string
		if err := rows.Scan(&id, &rel); err != nil {
			rows.Close()
			return 0, err
		}
		if !keep[rel] {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range stale {
		if _, err := c.db.ExecContext(ctx, `DELETE FROM books WHERE id = ?`, id); err != nil {
			return 0, err
		}
		if _, err := c.db.ExecContext(ctx, `DELETE FROM books_fts WHERE rowid = ?`, id); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

const bookCols = `id, library_id, rel_path, is_folder, title, author, series,
	series_index, narrator, duration, asin, isbn, cover_path, format, codec, size, mtime,
	added_at, content_hash`

func scanBook(row interface{ Scan(...any) error }) (*Book, error) {
	var b Book
	err := row.Scan(&b.ID, &b.LibraryID, &b.RelPath, &b.IsFolder, &b.Title, &b.Author,
		&b.Series, &b.SeriesIndex, &b.Narrator, &b.Duration, &b.ASIN, &b.ISBN,
		&b.CoverPath, &b.Format, &b.Codec, &b.Size, &b.MTime, &b.AddedAt, &b.ContentHash)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// GetBookByPath returns a book by its library + relative path, including files
// and chapters. Used by the resolve endpoint to map a browsed path to a book.
func (c *Catalog) GetBookByPath(ctx context.Context, libraryID int64, relPath string) (*Book, error) {
	var id int64
	err := c.db.QueryRowContext(ctx,
		`SELECT id FROM books WHERE library_id = ? AND rel_path = ?`, libraryID, relPath).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c.GetBook(ctx, id)
}

// GetBook returns a book by ID including its files and chapters.
func (c *Catalog) GetBook(ctx context.Context, id int64) (*Book, error) {
	row := c.db.QueryRowContext(ctx, `SELECT `+bookCols+` FROM books WHERE id = ?`, id)
	b, err := scanBook(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := c.loadFiles(ctx, b); err != nil {
		return nil, err
	}
	if err := c.loadChapters(ctx, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (c *Catalog) loadFiles(ctx context.Context, b *Book) error {
	rows, err := c.db.QueryContext(ctx,
		`SELECT rel_path, seq, duration, format, size FROM book_files WHERE book_id = ? ORDER BY seq`, b.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f BookFile
		if err := rows.Scan(&f.RelPath, &f.Seq, &f.Duration, &f.Format, &f.Size); err != nil {
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
	Author    string // optional exact-match filter
	Series    string // optional exact-match filter
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
		where = append(where, "author = ?")
		args = append(args, opt.Author)
	}
	if opt.Series != "" {
		where = append(where, "series = ?")
		args = append(args, opt.Series)
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
	parts := strings.SplitN(string(raw), "\x00", 2)
	if len(parts) != 2 {
		return "", 0, errors.New("malformed cursor")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, err
	}
	return parts[0], id, nil
}

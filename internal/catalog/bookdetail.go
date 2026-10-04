package catalog

import (
	"context"
	"database/sql"
	"errors"
	"path"
)

// AdminBookDetail is everything the console's book page shows about one book.
type AdminBookDetail struct {
	Book AdminBook `json:"book"`
	// Description is long, so it rides here rather than on every list row.
	Description string                `json:"description"`
	Fields      map[string]FieldValue `json:"fields"`
	Chapters    []AdminChapter        `json:"chapters"`
	Files       []AdminFile           `json:"files"`
	Listeners   []Listener            `json:"listeners"`
	Shares      []BookShare           `json:"shares"`
	Folder      BookFolder            `json:"folder"`
	IndexedAt   string                `json:"indexed_at"`
}

// AdminChapter is a chapter with its edit state.
type AdminChapter struct {
	Index        int     `json:"index"`
	Title        string  `json:"title"`
	ScannedTitle string  `json:"scanned_title"`
	Edited       bool    `json:"edited"`
	FilePath     string  `json:"file_path"`
	Start        float64 `json:"start"`
	End          float64 `json:"end"`
	BookOffset   float64 `json:"book_offset"`
}

// AdminFile is one audio file of a book. Bitrate is derived (bits per second over
// the file's duration); 0 when the duration is unknown.
type AdminFile struct {
	Path     string  `json:"path"`
	Seq      int     `json:"seq"`
	Duration float64 `json:"duration"`
	Format   string  `json:"format"`
	Codec    string  `json:"codec"`
	Size     int64   `json:"size"`
	Bitrate  int64   `json:"bitrate"`
}

// Listener is one user's progress on the book.
type Listener struct {
	UserID    int64   `json:"user_id"`
	Username  string  `json:"username"`
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	Finished  bool    `json:"finished"`
	UpdatedAt string  `json:"updated_at"`
	// StartedAt and FinishedAt are when the user started and finished the book
	// (null when not known), as on GET /admin/users/{id}/progress.
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
}

// BookShare is a share that includes the book, and the rule that includes it.
type BookShare struct {
	ShareID        int64  `json:"share_id"`
	Name           string `json:"name"`
	Path           string `json:"path"` // the granting rule ("" = whole library)
	WholeLibraryID *int64 `json:"whole_library_id,omitempty"`
}

// BookFolder is the folder whose detection decides the book's shape (the book's
// own folder, or the folder a single-file book sits in) and its override, if any.
type BookFolder struct {
	Path     string `json:"path"`
	Override string `json:"override"` // OverrideBook | OverrideCollection | ""
}

// AdminBookDetail loads the book page for (library, path). ErrNotFound when no
// book is indexed there.
func (c *Catalog) AdminBookDetail(ctx context.Context, libraryID int64, relPath string) (*AdminBookDetail, error) {
	relPath = CleanRelPath(relPath)
	d := &AdminBookDetail{}
	err := c.db.QueryRowContext(ctx, `SELECT `+adminBookCols+`, b.description, b.indexed_at
		FROM books b JOIN libraries l ON l.id = b.library_id
		WHERE b.library_id = ? AND b.rel_path = ?`, libraryID, relPath).
		Scan(append(adminBookDest(&d.Book), &d.Description, &d.IndexedAt)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	bookID := d.Book.id

	layers, err := loadLayers(ctx, c.db, bookID)
	if err != nil {
		return nil, err
	}
	d.Fields = layers.resolve()
	if d.Chapters, err = queryRows(ctx, c.db, func(rows *sql.Rows, ch *AdminChapter) error {
		return rows.Scan(&ch.Index, &ch.Title, &ch.ScannedTitle, &ch.Edited, &ch.FilePath,
			&ch.Start, &ch.End, &ch.BookOffset)
	}, `SELECT chapters.idx, chapters.title, chapters.scanned_title,
	           EXISTS(SELECT 1 FROM chapter_overrides co WHERE co.library_id = ? AND co.path = ? AND `+chapterOverrideMatch+`),
	           chapters.file_path, chapters.start, chapters."end", chapters.book_offset
	      FROM chapters WHERE chapters.book_id = ? ORDER BY chapters.idx`, libraryID, relPath, bookID); err != nil {
		return nil, err
	}
	files := &Book{ID: bookID}
	if err := c.loadFiles(ctx, files); err != nil {
		return nil, err
	}
	d.Files = adminFiles(d.Book, files.Files)
	if d.Listeners, err = queryRows(ctx, c.db, func(rows *sql.Rows, l *Listener) error {
		return rows.Scan(&l.UserID, &l.Username, &l.Position, &l.Duration, &l.Finished, &l.UpdatedAt,
			&l.StartedAt, &l.FinishedAt)
	}, `SELECT p.user_id, u.username, p.position, p.duration, p.finished, p.updated_at,
	           p.started_at, p.finished_at
	      FROM progress p JOIN users u ON u.id = p.user_id
	     WHERE p.library_id = ? AND p.rel_path = ?
	     ORDER BY p.updated_at DESC`, libraryID, relPath); err != nil {
		return nil, err
	}
	if d.Shares, err = c.sharesContaining(ctx, libraryID, relPath); err != nil {
		return nil, err
	}
	d.Folder.Path = relPath
	if !d.Book.IsFolder {
		if d.Folder.Path = path.Dir(relPath); d.Folder.Path == "." {
			d.Folder.Path = ""
		}
	}
	err = c.db.QueryRowContext(ctx,
		`SELECT mode FROM folder_overrides WHERE library_id = ? AND path = ?`, libraryID, d.Folder.Path).
		Scan(&d.Folder.Override)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return d, nil
}

// adminFiles lists a book's audio files; a single-file book is its own one file.
func adminFiles(b AdminBook, files []BookFile) []AdminFile {
	if len(files) == 0 {
		files = []BookFile{{RelPath: b.Path, Duration: b.Duration, Format: b.Format, Codec: b.Codec, Size: b.Size}}
	}
	out := make([]AdminFile, 0, len(files))
	for _, f := range files {
		codec := f.Codec
		if codec == "" {
			codec = b.Codec // parts indexed before per-file codecs were recorded
		}
		af := AdminFile{Path: f.RelPath, Seq: f.Seq, Duration: f.Duration, Format: f.Format, Codec: codec, Size: f.Size}
		if f.Duration > 0 {
			af.Bitrate = int64(float64(f.Size*8) / f.Duration)
		}
		out = append(out, af)
	}
	return out
}

// sharesContaining returns the shares whose rules include relPath, with the rule
// that does (pathAllowedBy, the same prefix rule Scope.Allows applies).
func (c *Catalog) sharesContaining(ctx context.Context, libraryID int64, relPath string) ([]BookShare, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT s.id, s.name, sp.path, s.whole_library_id
		   FROM share_paths sp JOIN shares s ON s.id = sp.share_id
		  WHERE sp.library_id = ? ORDER BY s.name COLLATE NOCASE, sp.path`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BookShare{}
	for rows.Next() {
		var s BookShare
		if err := rows.Scan(&s.ShareID, &s.Name, &s.Path, &s.WholeLibraryID); err != nil {
			return nil, err
		}
		if pathAllowedBy(s.Path, relPath) && (len(out) == 0 || out[len(out)-1].ShareID != s.ShareID) {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

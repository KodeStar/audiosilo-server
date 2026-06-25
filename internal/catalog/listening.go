package catalog

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Durable user state (progress, bookmarks, notes, history) is keyed by the
// filesystem path — (user_id, library_id, rel_path) — not the rebuildable
// book id, so it survives DB rebuilds, re-tagging, and being recorded before a
// scan reaches the file. rel_path is the *book* path (the folder for a
// chapters_in_folder book, the file otherwise); positions are on the whole-book
// timeline.

// Ref identifies content by library + path.
type Ref struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
}

// Progress is a user's playback position for a book.
type Progress struct {
	Ref
	Position      float64 `json:"position"`
	Duration      float64 `json:"duration"`
	Finished      bool    `json:"finished"`
	PlaybackSpeed float64 `json:"playback_speed"`
	Version       int64   `json:"version"`
	DeviceID      string  `json:"device_id"`
	UpdatedAt     string  `json:"updated_at"`
}

// Bookmark is a saved position with an optional note.
type Bookmark struct {
	ID int64 `json:"id"`
	Ref
	Position  float64 `json:"position"`
	Note      string  `json:"note"`
	CreatedAt string  `json:"created_at"`
}

// Note is free-form text attached to a book (optionally a position).
type Note struct {
	ID int64 `json:"id"`
	Ref
	Position  float64 `json:"position"`
	Body      string  `json:"body"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// GetProgress returns a user's progress for a book path, or nil if none.
func (c *Catalog) GetProgress(ctx context.Context, userID int64, ref Ref) (*Progress, error) {
	var p Progress
	err := c.db.QueryRowContext(ctx,
		`SELECT library_id, rel_path, position, duration, finished, playback_speed, version, device_id, updated_at
		   FROM progress WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
		userID, ref.LibraryID, ref.Path).
		Scan(&p.LibraryID, &p.Path, &p.Position, &p.Duration, &p.Finished, &p.PlaybackSpeed,
			&p.Version, &p.DeviceID, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SaveProgress writes progress using last-write-wins reconciliation: an update
// is applied only if its (updated_at, version) is newer than what is stored.
// It returns the effective stored progress. This is the same merge the realtime
// sync layer will reuse, so REST and WebSocket writes converge.
func (c *Catalog) SaveProgress(ctx context.Context, userID int64, in Progress) (*Progress, error) {
	if in.UpdatedAt == "" {
		in.UpdatedAt = c.ts()
	}
	if in.PlaybackSpeed <= 0 {
		in.PlaybackSpeed = 1.0
	}
	existing, err := c.GetProgress(ctx, userID, in.Ref)
	if err != nil {
		return nil, err
	}
	if existing != nil && !isNewer(in, *existing) {
		return existing, nil // incoming update is stale; keep stored value
	}
	if in.Version == 0 {
		in.Version = 1
		if existing != nil {
			in.Version = existing.Version + 1
		}
	}
	_, err = c.db.ExecContext(ctx,
		`INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished,
		     playback_speed, version, device_id, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(user_id, library_id, rel_path) DO UPDATE SET
		     position=excluded.position, duration=excluded.duration,
		     finished=excluded.finished, playback_speed=excluded.playback_speed,
		     version=excluded.version, device_id=excluded.device_id,
		     updated_at=excluded.updated_at`,
		userID, in.LibraryID, in.Path, in.Position, in.Duration, in.Finished,
		in.PlaybackSpeed, in.Version, in.DeviceID, in.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &in, nil
}

// isNewer reports whether candidate should replace current under last-write-wins
// (newer updated_at wins; version breaks ties for same-timestamp updates).
func isNewer(candidate, current Progress) bool {
	ct, err1 := time.Parse(time.RFC3339, candidate.UpdatedAt)
	pt, err2 := time.Parse(time.RFC3339, current.UpdatedAt)
	if err1 == nil && err2 == nil && !ct.Equal(pt) {
		return ct.After(pt)
	}
	return candidate.Version > current.Version
}

// ListProgress returns a user's progress rows scoped to paths they can still
// access (for offline sync seeding). scopes is the caller's effective access (see
// UserScopes); an empty slice yields no rows.
func (c *Catalog) ListProgress(ctx context.Context, userID int64, scopes []Scope) ([]Progress, error) {
	filter, fargs := scopesFilterSQL("library_id", "rel_path", scopes)
	args := append([]any{userID}, fargs...)
	rows, err := c.db.QueryContext(ctx,
		`SELECT library_id, rel_path, position, duration, finished, playback_speed, version, device_id, updated_at
		   FROM progress WHERE user_id = ? AND `+filter, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Progress
	for rows.Next() {
		var p Progress
		if err := rows.Scan(&p.LibraryID, &p.Path, &p.Position, &p.Duration, &p.Finished,
			&p.PlaybackSpeed, &p.Version, &p.DeviceID, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListeningRow is one user's progress on one book, enriched with book metadata
// (title/author) for the admin stats view. Title/author may be empty if the
// scan has not reached the path yet (durable state is path-keyed, not FK'd).
type ListeningRow struct {
	UserID    int64   `json:"user_id"`
	Username  string  `json:"username"`
	LibraryID int64   `json:"library_id"`
	Path      string  `json:"path"`
	Title     string  `json:"title"`
	Author    string  `json:"author"`
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	Finished  bool    `json:"finished"`
	UpdatedAt string  `json:"updated_at"`
}

// ListeningOverview returns every user's playback progress joined to book
// metadata, newest first. Admin-scoped (no path filtering); intended for the
// stats dashboard. Books are LEFT-joined on the path identity so progress shows
// even before/without an index entry.
func (c *Catalog) ListeningOverview(ctx context.Context, limit int) ([]ListeningRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT p.user_id, u.username, p.library_id, p.rel_path,
		        COALESCE(b.title, ''), COALESCE(b.author, ''),
		        p.position, p.duration, p.finished, p.updated_at
		   FROM progress p
		   JOIN users u ON u.id = p.user_id
		   LEFT JOIN books b ON b.library_id = p.library_id AND b.rel_path = p.rel_path
		  ORDER BY p.updated_at DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ListeningRow
	for rows.Next() {
		var r ListeningRow
		if err := rows.Scan(&r.UserID, &r.Username, &r.LibraryID, &r.Path,
			&r.Title, &r.Author, &r.Position, &r.Duration, &r.Finished, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MoveDurableState migrates a user-state from an old path to a new one within a
// library, used by the scanner when it detects a file move. It is a no-op if
// nothing references the old path.
func (c *Catalog) MoveDurableState(ctx context.Context, libraryID int64, oldPath, newPath string) error {
	// Wrap the five updates in one transaction so a "move" either fully applies
	// or fully rolls back, rather than leaving durable user state half-migrated
	// across tables if a statement fails midway.
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// One fully-constant UPDATE per durable-state table, iterated. The statements
	// are spelled out rather than built as `"UPDATE "+table+...` on purpose: that
	// concatenation trips gosec G202 (the project lints at a green baseline), and
	// only the values are bound parameters here anyway. Add a table -> add a line.
	stmts := []string{
		`UPDATE progress SET rel_path = ? WHERE library_id = ? AND rel_path = ?`,
		`UPDATE bookmarks SET rel_path = ? WHERE library_id = ? AND rel_path = ?`,
		`UPDATE notes SET rel_path = ? WHERE library_id = ? AND rel_path = ?`,
		`UPDATE listening_history SET rel_path = ? WHERE library_id = ? AND rel_path = ?`,
		`UPDATE favourites SET rel_path = ? WHERE library_id = ? AND rel_path = ?`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt, newPath, libraryID, oldPath); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddBookmark stores a bookmark and returns it with its ID.
func (c *Catalog) AddBookmark(ctx context.Context, userID int64, b Bookmark) (*Bookmark, error) {
	b.CreatedAt = c.ts()
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, created_at)
		 VALUES(?,?,?,?,?,?)`, userID, b.LibraryID, b.Path, b.Position, b.Note, b.CreatedAt)
	if err != nil {
		return nil, err
	}
	b.ID, _ = res.LastInsertId()
	return &b, nil
}

// ListBookmarks returns a user's bookmarks for a book path ordered by position.
func (c *Catalog) ListBookmarks(ctx context.Context, userID int64, ref Ref) ([]Bookmark, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, library_id, rel_path, position, note, created_at FROM bookmarks
		  WHERE user_id = ? AND library_id = ? AND rel_path = ? ORDER BY position`,
		userID, ref.LibraryID, ref.Path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bookmark
	for rows.Next() {
		var b Bookmark
		if err := rows.Scan(&b.ID, &b.LibraryID, &b.Path, &b.Position, &b.Note, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBookmark removes a user's bookmark by ID.
func (c *Catalog) DeleteBookmark(ctx context.Context, userID, id int64) error {
	_, err := c.db.ExecContext(ctx,
		`DELETE FROM bookmarks WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// AddNote stores a note and returns it with its ID.
func (c *Catalog) AddNote(ctx context.Context, userID int64, n Note) (*Note, error) {
	n.CreatedAt = c.ts()
	n.UpdatedAt = n.CreatedAt
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO notes(user_id, library_id, rel_path, position, body, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?)`, userID, n.LibraryID, n.Path, n.Position, n.Body, n.CreatedAt, n.UpdatedAt)
	if err != nil {
		return nil, err
	}
	n.ID, _ = res.LastInsertId()
	return &n, nil
}

// ListNotes returns a user's notes for a book path ordered by position.
func (c *Catalog) ListNotes(ctx context.Context, userID int64, ref Ref) ([]Note, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, library_id, rel_path, position, body, created_at, updated_at FROM notes
		  WHERE user_id = ? AND library_id = ? AND rel_path = ? ORDER BY position`,
		userID, ref.LibraryID, ref.Path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.LibraryID, &n.Path, &n.Position, &n.Body, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DeleteNote removes a user's note by ID.
func (c *Catalog) DeleteNote(ctx context.Context, userID, id int64) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM notes WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// AddHistory records a listening-history span.
func (c *Catalog) AddHistory(ctx context.Context, userID int64, ref Ref, from, to float64, startedAt, endedAt string) error {
	if startedAt == "" {
		startedAt = c.ts()
	}
	if endedAt == "" {
		endedAt = c.ts()
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO listening_history(user_id, library_id, rel_path, from_pos, to_pos, started_at, ended_at)
		 VALUES(?,?,?,?,?,?,?)`, userID, ref.LibraryID, ref.Path, from, to, startedAt, endedAt)
	return err
}

// History is a recorded listening span.
type History struct {
	ID int64 `json:"id"`
	Ref
	From      float64 `json:"from_pos"`
	To        float64 `json:"to_pos"`
	StartedAt string  `json:"started_at"`
	EndedAt   string  `json:"ended_at"`
}

func clampHistoryLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 100
	}
	return limit
}

func scanHistory(rows *sql.Rows) ([]History, error) {
	defer rows.Close()
	var out []History
	for rows.Next() {
		var h History
		if err := rows.Scan(&h.ID, &h.LibraryID, &h.Path, &h.From, &h.To, &h.StartedAt, &h.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListHistory returns a user's listening history for a book path, newest first.
func (c *Catalog) ListHistory(ctx context.Context, userID int64, ref Ref, limit int) ([]History, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, library_id, rel_path, from_pos, to_pos, started_at, ended_at FROM listening_history
		  WHERE user_id = ? AND library_id = ? AND rel_path = ? ORDER BY ended_at DESC LIMIT ?`,
		userID, ref.LibraryID, ref.Path, clampHistoryLimit(limit))
	if err != nil {
		return nil, err
	}
	return scanHistory(rows)
}

// ListAllHistory returns a user's recent listening history across all books,
// scoped to paths they can still access. The scope filter is applied in the query
// so LIMIT counts only accessible rows (see UserScopes); empty scopes yield none.
func (c *Catalog) ListAllHistory(ctx context.Context, userID int64, scopes []Scope, limit int) ([]History, error) {
	filter, fargs := scopesFilterSQL("library_id", "rel_path", scopes)
	args := append([]any{userID}, fargs...)
	args = append(args, clampHistoryLimit(limit))
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, library_id, rel_path, from_pos, to_pos, started_at, ended_at FROM listening_history
		  WHERE user_id = ? AND `+filter+` ORDER BY ended_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	return scanHistory(rows)
}

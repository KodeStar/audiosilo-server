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

// ListProgress returns all progress rows for a user (for offline sync seeding).
func (c *Catalog) ListProgress(ctx context.Context, userID int64) ([]Progress, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT library_id, rel_path, position, duration, finished, playback_speed, version, device_id, updated_at
		   FROM progress WHERE user_id = ?`, userID)
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

// MoveDurableState migrates a user-state from an old path to a new one within a
// library, used by the scanner when it detects a file move. It is a no-op if
// nothing references the old path.
func (c *Catalog) MoveDurableState(ctx context.Context, libraryID int64, oldPath, newPath string) error {
	for _, table := range []string{"progress", "bookmarks", "notes", "listening_history"} {
		if _, err := c.db.ExecContext(ctx,
			`UPDATE `+table+` SET rel_path = ? WHERE library_id = ? AND rel_path = ?`,
			newPath, libraryID, oldPath); err != nil {
			return err
		}
	}
	return nil
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

package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Durable user state (progress, bookmarks, notes, history) is keyed by the
// filesystem path - (user_id, library_id, rel_path) - not the rebuildable
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
//
// StartedAt and FinishedAt (RFC3339 UTC, "" = unknown/none, then absent from the
// JSON) are server-kept: SaveProgress stamps them and ignores a client's; past
// that, only an edit (EditProgress) sets them, and a move or join carries them. UserProgress shadows both with its own
// nullable fields for the admin console's wire shape.
type Progress struct {
	Ref
	Position      float64 `json:"position"`
	Duration      float64 `json:"duration"`
	Finished      bool    `json:"finished"`
	PlaybackSpeed float64 `json:"playback_speed"`
	Version       int64   `json:"version"`
	DeviceID      string  `json:"device_id"`
	UpdatedAt     string  `json:"updated_at"`
	StartedAt     string  `json:"started_at,omitempty"`
	FinishedAt    string  `json:"finished_at,omitempty"`
}

// progressColumns are the columns of a Progress, in scanProgress's order.
const progressColumns = `library_id, rel_path, position, duration, finished, playback_speed, version, device_id,
	updated_at, COALESCE(started_at, ''), COALESCE(finished_at, '')`

func scanProgress(row interface{ Scan(...any) error }, p *Progress) error {
	return row.Scan(&p.LibraryID, &p.Path, &p.Position, &p.Duration, &p.Finished, &p.PlaybackSpeed,
		&p.Version, &p.DeviceID, &p.UpdatedAt, &p.StartedAt, &p.FinishedAt)
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
	err := scanProgress(c.db.QueryRowContext(ctx,
		`SELECT `+progressColumns+` FROM progress WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
		userID, ref.LibraryID, ref.Path), &p)
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
// sync layer will reuse, so REST and WebSocket writes converge. The start and
// finish dates are the server's (see the stamps below): in's are ignored.
func (c *Catalog) SaveProgress(ctx context.Context, userID int64, in Progress) (*Progress, error) {
	in.StartedAt, in.FinishedAt = "", ""
	// Distrust an unparseable or far-future client timestamp (see
	// plausibleUpdatedAt) and substitute server time.
	if !plausibleUpdatedAt(in.UpdatedAt, c.now()) {
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
	// started_at is stamped by the first save and kept; finished_at is stamped
	// when finished turns on and cleared when it turns off (a restart). Both take
	// the save's own time (already checked by plausibleUpdatedAt), so a finish
	// replayed from an offline queue is dated when it happened, normalized to
	// RFC3339 UTC to the second (fixed width, so they compare as strings).
	stamp := c.now().UTC().Format(time.RFC3339)
	if t, err := time.Parse(time.RFC3339, in.UpdatedAt); err == nil {
		stamp = t.UTC().Format(time.RFC3339)
	}
	// RETURNING reads the dates the row ended up with (a kept start, a kept or new
	// finish), so the echo carries the stored ones.
	err = c.db.QueryRowContext(ctx,
		`INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished,
		     playback_speed, version, device_id, updated_at, started_at, finished_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,CASE WHEN ?6 THEN ?11 END)
		 ON CONFLICT(user_id, library_id, rel_path) DO UPDATE SET
		     position=excluded.position, duration=excluded.duration,
		     finished=excluded.finished, playback_speed=excluded.playback_speed,
		     version=excluded.version, device_id=excluded.device_id,
		     updated_at=excluded.updated_at,
		     finished_at=CASE WHEN NOT excluded.finished THEN NULL
		                      WHEN progress.finished THEN progress.finished_at
		                      ELSE ?11 END
		 RETURNING COALESCE(started_at, ''), COALESCE(finished_at, '')`,
		userID, in.LibraryID, in.Path, in.Position, in.Duration, in.Finished,
		in.PlaybackSpeed, in.Version, in.DeviceID, in.UpdatedAt, stamp).Scan(&in.StartedAt, &in.FinishedAt)
	if err != nil {
		return nil, err
	}
	return &in, nil
}

// plausibleUpdatedAt reports whether a client-supplied updated_at can be
// trusted for last-write-wins: it must parse and not be implausibly ahead of
// the server clock (a small skew is allowed so genuine cross-device ordering
// still works). Without this check a client with a broken clock (or a garbage
// value) sending a far-future timestamp would win every future comparison and
// permanently wedge the book's progress at that write; the caller substitutes
// server time (c.ts()) instead.
func plausibleUpdatedAt(v string, now time.Time) bool {
	const maxSkew = 5 * time.Minute
	t, err := time.Parse(time.RFC3339, v)
	return err == nil && !t.After(now.Add(maxSkew))
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
		`SELECT `+progressColumns+` FROM progress WHERE user_id = ? AND `+filter, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Progress
	for rows.Next() {
		var p Progress
		if err := scanProgress(rows, &p); err != nil {
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
	out := []ListeningRow{} // an empty list, never null: a fresh server has no listening yet
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
// nothing references the old path (or the two paths are the same). A listener who
// already has progress at the new path (a stale row of an earlier book there) keeps
// the newer save of the two (mergeNewest, not the furthest: a removed book's stale
// finish must not mark the moved book finished), and a favourite lands once, so
// such a collision no longer fails the move.
func (c *Catalog) MoveDurableState(ctx context.Context, libraryID int64, oldPath, newPath string) error {
	if oldPath == newPath {
		return nil
	}
	// Two transactions, each all or nothing: the book's own state first, then the
	// per-user state. They are separate so that a failure carrying the per-user
	// state can't also strand the admin's edits and cover at a path the scan is
	// about to prune.
	if err := c.db.WithTx(ctx, "MoveDurableState", func(tx *sql.Tx) error {
		return moveBookState(ctx, tx, libraryID, oldPath, newPath)
	}); err != nil {
		return err
	}
	return c.db.WithTx(ctx, "MoveDurableState", func(tx *sql.Tx) error {
		// A move is a join of one part at offset 0 that ends the book, except that
		// a collision takes the newer save rather than the furthest.
		if err := carryListeningState(ctx, tx, libraryID, JoinPart{Path: oldPath, Last: true}, newPath, 0, mergeNewest); err != nil {
			return fmt.Errorf("move listening state: %w", err)
		}
		return nil
	})
}

// carryListeningState hands every listener's state on one path (part.Path) to
// another (into), inside tx: a move (MoveDurableState) or one part of a join
// (JoinDurableState). Positions land on into's timeline (JoinPart.at; unchanged
// for a move); total is into's length (0 = unknown: each row takes where its
// part ends on into's timeline, JoinPart.end).
// Progress where the listener already has some on into merges by merge: a move
// takes the newer save (mergeNewest), a join the furthest (mergeFurthest); a
// favourite lands once.
//
// This is the one list of per-user path-keyed tables: add a table -> add a line
// (and, if it can be keyed on a folder, see carryFavourites).
// The statements are spelled out rather than built as `"UPDATE "+table+...` on
// purpose: that concatenation trips gosec G202 (the project lints at a green
// baseline), and only the values are bound parameters here anyway.
func carryListeningState(ctx context.Context, tx *sql.Tx, libraryID int64, part JoinPart, into string, total float64,
	merge progressMerge) error {
	if err := carryProgress(ctx, tx, libraryID, part, into, total, merge); err != nil {
		return err
	}
	stmts := []string{
		`UPDATE bookmarks SET rel_path = ?1, position = position + ?2 WHERE library_id = ?4 AND rel_path = ?5`,
		`UPDATE notes SET rel_path = ?1, position = position + ?2 WHERE library_id = ?4 AND rel_path = ?5`,
		`UPDATE listening_history SET rel_path = ?1, from_pos = from_pos + ?2, to_pos = to_pos + ?2
		  WHERE library_id = ?4 AND rel_path = ?5`,
		// A session that finished a part finished the joined book only when the
		// part ends it (JoinPart.Last). Its length is the joined book's, or with that
		// unknown where its part ends (JoinPart.end; SET reads the old end_pos).
		`UPDATE listening_sessions SET rel_path = ?1, start_pos = start_pos + ?2, end_pos = end_pos + ?2,
		        duration = CASE WHEN ?3 > 0 THEN MAX(duration, ?3)
		                        WHEN ?7 > 0 THEN ?2 + MAX(?7, end_pos)
		                        WHEN ?2 > 0 THEN ?2 + MAX(duration, end_pos)
		                        ELSE duration END,
		        finished = finished AND ?6
		  WHERE library_id = ?4 AND rel_path = ?5`,
		`UPDATE listening_daily SET rel_path = ?1 WHERE library_id = ?4 AND rel_path = ?5`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt, into, part.Offset, total, libraryID, part.Path, part.Last,
			part.Duration); err != nil {
			return err
		}
	}
	if err := carryListsState(ctx, tx, libraryID, part.Path, into); err != nil {
		return err
	}
	if err := carryRatings(ctx, tx, libraryID, part.Path, into); err != nil {
		return err
	}
	return carryFavourites(ctx, tx, libraryID, part.Path, into)
}

// carryFavourites hands every listener's favourite on one path to another, inside
// tx, landing once where a listener already favourited it. It is the one table of
// carryListeningState that can also be keyed on a navigation folder, so a folder
// rename carries it too (MoveFolderFavourites).
func carryFavourites(ctx context.Context, tx *sql.Tx, libraryID int64, from, into string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO favourites(user_id, library_id, rel_path, created_at)
		 SELECT user_id, library_id, ?, created_at FROM favourites WHERE library_id = ? AND rel_path = ?`,
		into, libraryID, from); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM favourites WHERE library_id = ? AND rel_path = ?`, libraryID, from)
	return err
}

// listenerProgress is one listener's progress row on a path.
type listenerProgress struct {
	user int64
	UserProgress
}

// progressOn reads every listener's progress on one path.
func progressOn(ctx context.Context, tx *sql.Tx, libraryID int64, relPath string) ([]listenerProgress, error) {
	return queryRows(ctx, tx, func(rows *sql.Rows, r *listenerProgress) error {
		var err error
		r.UserProgress, err = scanUserProgress(rows, &r.user)
		return err
	}, `SELECT `+userProgressColumns+`, p.user_id FROM progress p
	      LEFT JOIN books b ON b.library_id = p.library_id AND b.rel_path = p.rel_path
	     WHERE p.library_id = ? AND p.rel_path = ?`, libraryID, relPath)
}

// carryProgress moves every listener's progress on part.Path to into, placed on
// into's timeline (JoinPart.at), merged with any they already have there (merge).
func carryProgress(ctx context.Context, tx *sql.Tx, libraryID int64, part JoinPart, into string, total float64,
	merge progressMerge) error {
	from, err := progressOn(ctx, tx, libraryID, part.Path)
	if err != nil || len(from) == 0 {
		return err
	}
	have, err := progressOn(ctx, tx, libraryID, into)
	if err != nil {
		return err
	}
	existing := make(map[int64]UserProgress, len(have))
	for _, r := range have {
		existing[r.user] = r.UserProgress
	}
	for _, r := range from {
		p := r.UserProgress
		p.Position, p.Finished = part.at(p.Position, p.Finished)
		if total <= 0 {
			p.Duration = part.end(p.Position, p.Duration)
		}
		if !p.Finished {
			p.FinishedAt = nil
		}
		if cur, ok := existing[r.user]; ok {
			p = merge(cur, p)
		}
		if total > 0 {
			p.Duration = total
		}
		if p.Finished && p.FinishedAt == nil {
			at := p.UpdatedAt // a finish never dated: the save that holds it
			if t, err := time.Parse(time.RFC3339, at); err == nil {
				at = t.UTC().Format(time.RFC3339)
			}
			p.FinishedAt = &at
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished,
			     playback_speed, version, device_id, updated_at, started_at, finished_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(user_id, library_id, rel_path) DO UPDATE SET
			     position=excluded.position, duration=excluded.duration, finished=excluded.finished,
			     playback_speed=excluded.playback_speed, version=excluded.version,
			     device_id=excluded.device_id, updated_at=excluded.updated_at,
			     started_at=excluded.started_at, finished_at=excluded.finished_at`,
			r.user, libraryID, into, p.Position, p.Duration, p.Finished, p.PlaybackSpeed, p.Version,
			p.DeviceID, p.UpdatedAt, p.StartedAt, p.FinishedAt); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM progress WHERE library_id = ? AND rel_path = ?`, libraryID, part.Path)
	return err
}

// progressMerge is one listener's progress from the row already on a path (have)
// and the row carried onto it (carried): mergeNewest for a move, mergeFurthest for
// a join.
type progressMerge func(have, carried UserProgress) UserProgress

// mergeNewest is a move's merge: the newer save wins whole (isNewer: updated_at,
// then version), under a version above both. A row already at a book's new path is
// another book's, left behind when it was removed; its position, or its finish,
// says nothing about the moved book, so it can't win by being further on.
func mergeNewest(have, carried UserProgress) UserProgress {
	out := have
	if isNewer(carried.Progress, have.Progress) {
		out = carried
	}
	out.Version = max(have.Version, carried.Version) + 1
	return out
}

// mergeFurthest is a join's merge (the rows are parts of one book, on its
// timeline): the furthest position wins (finished over not, at the same place),
// with its speed, device and finish date; the result takes the newer save
// (isNewer), the earlier start, and a version above both, so it is not older than
// either save it replaces.
func mergeFurthest(a, b UserProgress) UserProgress {
	out, other := a, b
	if b.Position > a.Position || (b.Position == a.Position && b.Finished && !a.Finished) {
		out, other = b, a
	}
	if isNewer(other.Progress, out.Progress) {
		out.UpdatedAt = other.UpdatedAt
	}
	out.Version = max(a.Version, b.Version) + 1
	if other.StartedAt != nil && (out.StartedAt == nil || *other.StartedAt < *out.StartedAt) {
		out.StartedAt = other.StartedAt
	}
	return out
}

// moveBookState carries the book's own path-keyed state (enrichment, ignored
// issues, an admin's metadata edits, a custom cover) from oldPath to newPath.
func moveBookState(ctx context.Context, tx *sql.Tx, libraryID int64, oldPath, newPath string) error {
	// book_enrichment is keyed on the book path too, so a move must carry the
	// attached ASIN/ISBN to the new path or the moved book silently loses it.
	if _, err := tx.ExecContext(ctx,
		`UPDATE OR REPLACE book_enrichment SET path = ? WHERE library_id = ? AND path = ?`,
		newPath, libraryID, oldPath); err != nil {
		return err
	}
	// So do the Health issues an admin ignored for it.
	if _, err := tx.ExecContext(ctx,
		`UPDATE OR REPLACE issue_ignores SET path = ? WHERE library_id = ? AND path = ?`,
		newPath, libraryID, oldPath); err != nil {
		return err
	}
	// An admin's edits and custom cover belong to the book and follow it as ONE set.
	// When the moved book has any of them, whatever the new path already carries in
	// those tables (stale rows from an earlier book there) is dropped first, in every
	// table, so nothing stale can merge into the moved book's set. A moved book with
	// none keeps the path's own rows, as any book appearing at that path would.
	var hasEdits bool
	if err := tx.QueryRowContext(ctx, `SELECT
		    EXISTS(SELECT 1 FROM book_overrides WHERE library_id = ?1 AND path = ?2)
		 OR EXISTS(SELECT 1 FROM chapter_overrides WHERE library_id = ?1 AND path = ?2)
		 OR EXISTS(SELECT 1 FROM book_covers WHERE library_id = ?1 AND path = ?2)`,
		libraryID, oldPath).Scan(&hasEdits); err != nil {
		return err
	}
	if !hasEdits {
		return nil
	}
	for _, stmt := range []string{
		`DELETE FROM book_overrides WHERE library_id = ? AND path = ?`,
		`DELETE FROM chapter_overrides WHERE library_id = ? AND path = ?`,
		`DELETE FROM book_covers WHERE library_id = ? AND path = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, libraryID, newPath); err != nil {
			return err
		}
	}
	for _, stmt := range []string{
		`UPDATE book_overrides SET path = ? WHERE library_id = ? AND path = ?`,
		`UPDATE chapter_overrides SET path = ? WHERE library_id = ? AND path = ?`,
		`UPDATE book_covers SET path = ? WHERE library_id = ? AND path = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, newPath, libraryID, oldPath); err != nil {
			return err
		}
	}
	// The custom cover moved: each path's book now has other art.
	if err := refreshCoverArt(ctx, tx, libraryID, oldPath); err != nil {
		return err
	}
	return refreshCoverArt(ctx, tx, libraryID, newPath)
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

package catalog

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// UserProgress is one of a person's books as the admin console lists it: the
// progress row with the book's title and the start and finish dates. Its own
// StartedAt/FinishedAt (null when unknown, as the console reads them) shadow the
// embedded Progress's, which stay empty here; AsProgress is the player's shape.
type UserProgress struct {
	Progress
	Title      string  `json:"title"`
	Author     string  `json:"author"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
}

// AsProgress is the row as the player's Progress, carrying the dates (absent when
// unknown).
func (p UserProgress) AsProgress() Progress {
	out := p.Progress
	out.StartedAt, out.FinishedAt = "", ""
	if p.StartedAt != nil {
		out.StartedAt = *p.StartedAt
	}
	if p.FinishedAt != nil {
		out.FinishedAt = *p.FinishedAt
	}
	return out
}

const userProgressColumns = `p.library_id, p.rel_path, p.position, p.duration, p.finished, p.playback_speed,
	p.version, p.device_id, p.updated_at, COALESCE(b.title, ''), COALESCE(b.author, ''),
	p.started_at, p.finished_at`

// scanUserProgress reads a row of userProgressColumns, then any extra columns
// selected after them into extra.
func scanUserProgress(row interface{ Scan(...any) error }, extra ...any) (UserProgress, error) {
	var (
		p                 UserProgress
		started, finished sql.NullString
	)
	err := row.Scan(append([]any{&p.LibraryID, &p.Path, &p.Position, &p.Duration, &p.Finished, &p.PlaybackSpeed,
		&p.Version, &p.DeviceID, &p.UpdatedAt, &p.Title, &p.Author, &started, &finished}, extra...)...)
	if started.Valid {
		p.StartedAt = &started.String
	}
	if finished.Valid {
		p.FinishedAt = &finished.String
	}
	return p, err
}

// ListUserProgress returns every progress row of one user (admin-scoped: no
// share filtering), most recently saved first.
func (c *Catalog) ListUserProgress(ctx context.Context, userID int64) ([]UserProgress, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, p *UserProgress) error {
		var err error
		*p, err = scanUserProgress(rows)
		return err
	}, `SELECT `+userProgressColumns+` FROM progress p
	      LEFT JOIN books b ON b.library_id = p.library_id AND b.rel_path = p.rel_path
	     WHERE p.user_id = ? ORDER BY p.updated_at DESC`, userID)
}

// ErrInvalidProgressEdit is returned for a ProgressEdit that contradicts itself
// (a finish date on an unfinished book, a finish before the start, a date in the
// future, a position outside the book).
var ErrInvalidProgressEdit = errors.New("invalid progress edit")

// ErrNoAccess is returned by EditProgress for an edit that would start progress
// on a book the user can't see. Existing progress stays editable after access is
// taken away, so stale rows can still be tidied.
var ErrNoAccess = errors.New("the user has no access to this book")

// OptionalTime is a date in an edit: Set says the edit names it, and a nil Value
// clears it.
type OptionalTime struct {
	Set   bool
	Value *time.Time
}

// ProgressEdit is a change to someone's progress on a book: an admin's, or the
// listener's own (PATCH /libraries/{id}/progress). Nil or unset fields stay as
// they are.
type ProgressEdit struct {
	Finished   *bool
	Position   *float64
	StartedAt  OptionalTime
	FinishedAt OptionalTime
}

// changes reports whether the edit sets anything at all.
func (e ProgressEdit) changes() bool {
	return e.Finished != nil || e.Position != nil || e.StartedAt.Set || e.FinishedAt.Set
}

// EditProgress applies an edit (an admin's, or the user's own) to a user's
// progress on a book, creating the row when the book is indexed and the user has
// none. Marking a book finished moves the position to the end (players read that
// as done) and stamps the finish now unless the edit names a date; marking it
// unfinished clears the finish date and keeps the position unless the edit sets
// one. It is not playback, so it records no listening session. The write is stamped with the
// server's time and a higher version, so under last-write-wins it beats what a
// device saved before it, while any device with the book loaded overrides it on
// its next save, as it should. Returns ErrNotFound when the user has no progress
// on the path and no book is indexed there, and ErrNoAccess when the user has none
// and `scope` (the user's own, not the admin's) doesn't allow the path. An edit
// that sets nothing writes nothing (as UpdateCollection's): it answers the row as
// it is, or ErrNotFound with none, rather than starting the book or, stamped now,
// outranking a device's pending save.
func (c *Catalog) EditProgress(ctx context.Context, userID int64, ref Ref, e ProgressEdit, scope Scope) (*UserProgress, error) {
	now := c.now()
	var err error
	if e.changes() {
		err = c.editProgress(ctx, userID, ref, e, scope, now)
	}
	if err != nil {
		return nil, err
	}
	up, err := scanUserProgress(c.db.QueryRowContext(ctx,
		`SELECT `+userProgressColumns+` FROM progress p
		   LEFT JOIN books b ON b.library_id = p.library_id AND b.rel_path = p.rel_path
		  WHERE p.user_id = ? AND p.library_id = ? AND p.rel_path = ?`, userID, ref.LibraryID, ref.Path))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound // an edit that set nothing, on no progress
	}
	if err != nil {
		return nil, err
	}
	return &up, nil
}

// editProgress writes EditProgress's edit, in one transaction.
func (c *Catalog) editProgress(ctx context.Context, userID int64, ref Ref, e ProgressEdit, scope Scope, now time.Time) error {
	return c.db.WithTx(ctx, "EditProgress", func(tx *sql.Tx) error {
		var (
			p                 Progress
			started, finished sql.NullString
		)
		err := tx.QueryRowContext(ctx,
			`SELECT position, duration, finished, playback_speed, version, device_id, started_at, finished_at
			   FROM progress WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
			userID, ref.LibraryID, ref.Path).
			Scan(&p.Position, &p.Duration, &p.Finished, &p.PlaybackSpeed, &p.Version, &p.DeviceID, &started, &finished)
		if errors.Is(err, sql.ErrNoRows) {
			// A new row: the book is started now (unless the edit names a date).
			p.PlaybackSpeed = 1
			started = sql.NullString{String: now.UTC().Format(time.RFC3339), Valid: true}
			if err := tx.QueryRowContext(ctx,
				`SELECT duration FROM books WHERE library_id = ? AND rel_path = ?`,
				ref.LibraryID, ref.Path).Scan(&p.Duration); errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			} else if err != nil {
				return err
			}
			if !scope.Allows(ref.Path) {
				return ErrNoAccess
			}
		} else if err != nil {
			return err
		}
		wasFinished := p.Finished
		if e.Finished != nil {
			p.Finished = *e.Finished
			switch {
			case p.Finished && !wasFinished:
				finished = sql.NullString{String: now.UTC().Format(time.RFC3339), Valid: true}
				if p.Duration > 0 {
					p.Position = p.Duration
				}
			case !p.Finished:
				finished = sql.NullString{}
			}
		}
		if e.Position != nil {
			pos := *e.Position
			if pos < 0 || (p.Duration > 0 && pos > p.Duration) {
				return ErrInvalidProgressEdit
			}
			p.Position = pos
		}
		apply := func(o OptionalTime, dst *sql.NullString) error {
			if !o.Set {
				return nil
			}
			if o.Value == nil {
				*dst = sql.NullString{}
				return nil
			}
			if o.Value.After(now.Add(24 * time.Hour)) {
				return ErrInvalidProgressEdit
			}
			*dst = sql.NullString{String: o.Value.UTC().Format(time.RFC3339), Valid: true}
			return nil
		}
		if err := apply(e.StartedAt, &started); err != nil {
			return err
		}
		if err := apply(e.FinishedAt, &finished); err != nil {
			return err
		}
		if finished.Valid && !p.Finished {
			return ErrInvalidProgressEdit
		}
		if started.Valid && finished.Valid {
			s, f := parseSessionTime(started.String), parseSessionTime(finished.String)
			if !s.IsZero() && !f.IsZero() && f.Before(s) {
				return ErrInvalidProgressEdit
			}
		}
		// updated_at keeps the edit's sub-second time (as c.ts() does): devices
		// stamp their saves in milliseconds, and a whole-second stamp would let a
		// save made up to a second BEFORE the edit win last-write-wins against it.
		_, err = tx.ExecContext(ctx,
			`INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished,
			     playback_speed, version, device_id, updated_at, started_at, finished_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(user_id, library_id, rel_path) DO UPDATE SET
			     position=excluded.position, finished=excluded.finished, version=excluded.version,
			     updated_at=excluded.updated_at, started_at=excluded.started_at,
			     finished_at=excluded.finished_at`,
			userID, ref.LibraryID, ref.Path, p.Position, p.Duration, p.Finished, p.PlaybackSpeed,
			p.Version+1, p.DeviceID, now.UTC().Format(time.RFC3339Nano), started, finished)
		return err
	})
}

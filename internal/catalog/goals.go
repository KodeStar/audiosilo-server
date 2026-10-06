package catalog

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MaxBooksPerYear bounds a listening goal (books finished per calendar year).
const MaxBooksPerYear = 1000

// ErrInvalidGoal is returned by SetListeningGoal for a goal outside 1..MaxBooksPerYear.
var ErrInvalidGoal = errors.New("invalid goal")

// ListeningGoal is a person's goal: books to finish per calendar year.
type ListeningGoal struct {
	BooksPerYear int    `json:"books_per_year"`
	UpdatedAt    string `json:"updated_at"`
}

// GoalStatus is a person's goal (nil when unset) and their progress toward it:
// the books they finished in Year (server time) so far.
type GoalStatus struct {
	Goal     *ListeningGoal `json:"goal"`
	Year     string         `json:"year"`
	Finished int            `json:"finished"`
}

// GoalStatusFor reads userID's goal and the books they finished this calendar
// year, in the server's zone loc as of now (counted like UserStats' finished).
func (c *Catalog) GoalStatusFor(ctx context.Context, userID int64, now time.Time, loc *time.Location) (*GoalStatus, error) {
	if userID <= 0 {
		return nil, errNoUser
	}
	year, from, to, err := ParseActivityRange("year", now, loc)
	if err != nil {
		return nil, err
	}
	out := &GoalStatus{Year: year}
	var g ListeningGoal
	err = c.db.QueryRowContext(ctx, `SELECT books_per_year, updated_at FROM listening_goals WHERE user_id = ?`, userID).
		Scan(&g.BooksPerYear, &g.UpdatedAt)
	switch {
	case err == nil:
		out.Goal = &g
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	if out.Finished, err = c.finishedCount(ctx, userID, from, to); err != nil {
		return nil, err
	}
	return out, nil
}

// SetListeningGoal sets userID's goal to booksPerYear (1..MaxBooksPerYear, else
// ErrInvalidGoal).
func (c *Catalog) SetListeningGoal(ctx context.Context, userID int64, booksPerYear int) error {
	if booksPerYear < 1 || booksPerYear > MaxBooksPerYear {
		return ErrInvalidGoal
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO listening_goals(user_id, books_per_year, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET books_per_year = excluded.books_per_year, updated_at = excluded.updated_at`,
		userID, booksPerYear, c.stamp())
	return err
}

// DeleteListeningGoal clears userID's goal (none is not an error).
func (c *Catalog) DeleteListeningGoal(ctx context.Context, userID int64) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM listening_goals WHERE user_id = ?`, userID)
	return err
}

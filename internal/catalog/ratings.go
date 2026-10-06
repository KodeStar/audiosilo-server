package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"
)

// Ratings (player redesign Phase 1b): a listener's own 1-5 star rating and short
// note on a book. Durable user state, path-keyed on the book's own path and not
// FK'd to the index, carried by a move or join (carryRatings) like the rest of
// carryListeningState's tables.

// MaxRatingNote bounds a rating's note, in characters (runes), after trimming.
const MaxRatingNote = 500

var (
	// ErrInvalidRating is a rating outside 1-5.
	ErrInvalidRating = errors.New("rating must be a whole number from 1 to 5")
	// ErrRatingNoteTooLong is a note longer than MaxRatingNote characters.
	ErrRatingNoteTooLong = errors.New("the note is longer than 500 characters")
)

// Rating is one listener's rating of a book.
type Rating struct {
	Ref
	Stars     int    `json:"rating"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// RatedBook is a rating in the caller's list, with the book when one is indexed
// at its path (the player's list shape: no description).
type RatedBook struct {
	Rating
	Book *Book `json:"book,omitempty"`
}

const ratingColumns = `library_id, rel_path, rating, note, created_at, updated_at`

func scanRating(row interface{ Scan(...any) error }, r *Rating) error {
	return row.Scan(&r.LibraryID, &r.Path, &r.Stars, &r.Note, &r.CreatedAt, &r.UpdatedAt)
}

// CheckRating validates a rating before it is stored (SetRating checks it too):
// the note trimmed, or ErrInvalidRating / ErrRatingNoteTooLong.
func CheckRating(stars int, note string) (string, error) {
	if stars < 1 || stars > 5 {
		return "", ErrInvalidRating
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > MaxRatingNote {
		return "", ErrRatingNoteTooLong
	}
	return note, nil
}

// GetRating returns a user's rating of the book at ref, or nil when there is none.
func (c *Catalog) GetRating(ctx context.Context, userID int64, ref Ref) (*Rating, error) {
	var r Rating
	err := scanRating(c.db.QueryRowContext(ctx,
		`SELECT `+ratingColumns+` FROM ratings WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
		userID, ref.LibraryID, ref.Path), &r)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SetRating stores a user's rating of the book at ref (its own path: the caller
// resolves a part path to its book first), replacing any earlier one and keeping
// when it was first rated. The note is trimmed. ErrInvalidRating for stars outside
// 1-5, ErrRatingNoteTooLong for a note over MaxRatingNote characters.
func (c *Catalog) SetRating(ctx context.Context, userID int64, ref Ref, stars int, note string) (*Rating, error) {
	note, err := CheckRating(stars, note)
	if err != nil {
		return nil, err
	}
	now := formatSessionTime(c.now())
	var r Rating
	err = scanRating(c.db.QueryRowContext(ctx,
		`INSERT INTO ratings(user_id, library_id, rel_path, rating, note, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(user_id, library_id, rel_path) DO UPDATE SET
		     rating = excluded.rating, note = excluded.note, updated_at = excluded.updated_at
		 RETURNING `+ratingColumns,
		userID, ref.LibraryID, ref.Path, stars, note, now, now), &r)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// DeleteRating clears a user's rating of the book at ref. Idempotent.
func (c *Catalog) DeleteRating(ctx context.Context, userID int64, ref Ref) error {
	_, err := c.db.ExecContext(ctx,
		`DELETE FROM ratings WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
		userID, ref.LibraryID, ref.Path)
	return err
}

// ListRatings returns a user's ratings on paths their current access still
// reaches (scopes, see UserScopes; none yields none), newest updated_at first,
// each with its book when one is indexed at the path. A rating under a revoked
// share stays stored but is not returned.
func (c *Catalog) ListRatings(ctx context.Context, userID int64, scopes []Scope) ([]RatedBook, error) {
	filter, fargs := scopesFilterSQL("library_id", "rel_path", scopes)
	out, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *RatedBook) error {
		return scanRating(rows, &r.Rating)
	}, `SELECT `+ratingColumns+` FROM ratings WHERE user_id = ? AND `+filter+`
	     ORDER BY updated_at DESC, library_id, rel_path`, append([]any{userID}, fargs...)...)
	if err != nil {
		return nil, err
	}
	byLib := map[int64][]int{} // library -> indexes into out
	for i, r := range out {
		byLib[r.LibraryID] = append(byLib[r.LibraryID], i)
	}
	for libID, idx := range byLib {
		paths := make([]string, len(idx))
		for j, i := range idx {
			paths[j] = out[i].Path
		}
		books, err := c.BooksByPaths(ctx, libID, paths)
		if err != nil {
			return nil, err
		}
		for _, i := range idx {
			if b, ok := books[out[i].Path]; ok {
				out[i].Book = &b
			}
		}
	}
	return out, nil
}

// carryRatings hands every listener's rating on one path to another, inside tx (a
// move or one part of a join, see carryListeningState). Where the listener already
// rated the destination, the newer updated_at wins whole (a tie keeps the
// destination's), as a move's progress does (mergeNewest).
func carryRatings(ctx context.Context, tx *sql.Tx, libraryID int64, from, into string) error {
	if from == into {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ratings(user_id, library_id, rel_path, rating, note, created_at, updated_at)
		 SELECT user_id, library_id, ?1, rating, note, created_at, updated_at
		   FROM ratings WHERE library_id = ?2 AND rel_path = ?3
		 ON CONFLICT(user_id, library_id, rel_path) DO UPDATE SET
		     rating = excluded.rating, note = excluded.note,
		     created_at = excluded.created_at, updated_at = excluded.updated_at
		   WHERE excluded.updated_at > ratings.updated_at`,
		into, libraryID, from); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM ratings WHERE library_id = ? AND rel_path = ?`, libraryID, from)
	return err
}

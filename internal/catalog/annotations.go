package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"unicode/utf8"
)

// Annotations (player redesign Phase 4, capability "annotations"): a bookmark's
// label, the owner's edits of a bookmark or note, and the caller's bookmarks,
// notes and listening history across books, newest first, a page at a time.
// The rows are the path-keyed user state of listening.go; nothing here adds a
// table (migration 0030 adds bookmarks.label, 0031 the lists' indexes).

const (
	// MaxBookmarkNote bounds a bookmark's note, in characters (runes).
	MaxBookmarkNote = 2000
	// MaxNoteBody bounds a note's body, in characters (runes).
	MaxNoteBody = 10000
)

var (
	// ErrInvalidLabel is a bookmark label that isn't a machine key (labelPattern).
	ErrInvalidLabel = errors.New("invalid label")
	// ErrBookmarkNoteTooLong is a bookmark note over MaxBookmarkNote characters.
	ErrBookmarkNoteTooLong = errors.New("note too long")
	// ErrNoteBodyTooLong is a note body over MaxNoteBody characters.
	ErrNoteBodyTooLong = errors.New("body too long")
	// ErrInvalidPosition is a note position that is negative or not finite.
	ErrInvalidPosition = errors.New("invalid position")
	// ErrNothingToChange is an edit that names no field.
	ErrNothingToChange = errors.New("nothing to change")
)

// labelPattern is a label's shape: a lowercase machine key. The server does not
// know the player's keys (quote, favourite, fell_asleep, ...), so a newer player
// can add one without a server change.
var labelPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// CheckBookmark validates a bookmark's note and label: ErrBookmarkNoteTooLong
// for a note over MaxBookmarkNote characters, ErrInvalidLabel for a label that
// is neither "" nor a machine key.
func CheckBookmark(note, label string) error {
	if utf8.RuneCountInString(note) > MaxBookmarkNote {
		return ErrBookmarkNoteTooLong
	}
	if label != "" && !labelPattern.MatchString(label) {
		return ErrInvalidLabel
	}
	return nil
}

// checkNoteBody is ErrNoteBodyTooLong for a body over MaxNoteBody characters.
func checkNoteBody(body string) error {
	if utf8.RuneCountInString(body) > MaxNoteBody {
		return ErrNoteBodyTooLong
	}
	return nil
}

// BookmarkEdit is an owner's change to a bookmark: nil leaves a field as it is.
type BookmarkEdit struct {
	Note  *string
	Label *string
}

// NoteEdit is an owner's change to a note: nil leaves a field as it is.
type NoteEdit struct {
	Body     *string
	Position *float64
}

// EditBookmark applies an edit to the user's bookmark id and returns the whole
// bookmark. ErrNotFound for an unknown id, another user's, or one whose path the
// caller's current access (scopes, see UserScopes) no longer reaches, so a
// revoked share's bookmark can't be changed through an old id; then
// ErrNothingToChange for an empty edit and CheckBookmark's errors for bad values.
// The read, the checks and the write are one writer transaction.
func (c *Catalog) EditBookmark(ctx context.Context, userID, id int64, edit BookmarkEdit, scopes []Scope) (*Bookmark, error) {
	var b Bookmark
	err := c.db.WithTx(ctx, "EditBookmark", func(tx *sql.Tx) error {
		if err := ownRow(scanBookmark(tx.QueryRowContext(ctx,
			`SELECT `+bookmarkColumns+` FROM bookmarks WHERE id = ? AND user_id = ?`, id, userID), &b),
			scopes, &b.Ref); err != nil {
			return err
		}
		if edit.Note == nil && edit.Label == nil {
			return ErrNothingToChange
		}
		if edit.Note != nil {
			b.Note = *edit.Note
		}
		if edit.Label != nil {
			b.Label = *edit.Label
		}
		if err := CheckBookmark(b.Note, b.Label); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE bookmarks SET note = ?, label = ? WHERE id = ?`, b.Note, b.Label, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// EditNote applies an edit to the user's note id, stamps updated_at and returns
// the whole note. ErrNotFound as EditBookmark; then ErrNothingToChange for an
// empty edit, ErrNoteBodyTooLong, and ErrInvalidPosition for a position that is
// negative or not finite. One writer transaction, as EditBookmark.
func (c *Catalog) EditNote(ctx context.Context, userID, id int64, edit NoteEdit, scopes []Scope) (*Note, error) {
	var n Note
	err := c.db.WithTx(ctx, "EditNote", func(tx *sql.Tx) error {
		if err := ownRow(scanNote(tx.QueryRowContext(ctx,
			`SELECT `+noteColumns+` FROM notes WHERE id = ? AND user_id = ?`, id, userID), &n),
			scopes, &n.Ref); err != nil {
			return err
		}
		if edit.Body == nil && edit.Position == nil {
			return ErrNothingToChange
		}
		if edit.Body != nil {
			if err := checkNoteBody(*edit.Body); err != nil {
				return err
			}
			n.Body = *edit.Body
		}
		if edit.Position != nil {
			p := *edit.Position
			if p < 0 || math.IsNaN(p) || math.IsInf(p, 0) {
				return ErrInvalidPosition
			}
			n.Position = p
		}
		n.UpdatedAt = c.ts()
		_, err := tx.ExecContext(ctx, `UPDATE notes SET body = ?, position = ?, updated_at = ? WHERE id = ?`,
			n.Body, n.Position, n.UpdatedAt, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// ownRow turns the read of a caller's row by id into EditBookmark/EditNote's
// answer: ErrNotFound when there is no such row of theirs (scanErr is
// sql.ErrNoRows) or its path is outside their current access.
func ownRow(scanErr error, scopes []Scope, ref *Ref) error {
	switch {
	case errors.Is(scanErr, sql.ErrNoRows):
		return ErrNotFound
	case scanErr != nil:
		return scanErr
	case !scopesAllow(scopes, *ref):
		return ErrNotFound
	}
	return nil
}

// PageOptions pages a list newest first: Limit rows (clampHistoryLimit: default
// 100, at most 500), after the opaque Cursor a previous page's NextCursor gave.
type PageOptions struct {
	Limit  int
	Cursor string
}

// MyBookmark is a bookmark in the caller's list, with the book when one is
// indexed at its path (the list shape: no description).
type MyBookmark struct {
	Bookmark
	Book *Book `json:"book,omitempty"`
}

// MyNote is a note in the caller's list, with its book as MyBookmark.
type MyNote struct {
	Note
	Book *Book `json:"book,omitempty"`
}

// HistoryEntry is a listening span in the caller's history, with its book as
// MyBookmark.
type HistoryEntry struct {
	History
	Book *Book `json:"book,omitempty"`
}

// BookmarkPage is one page of the caller's bookmarks; NextCursor is set only
// when more rows follow.
type BookmarkPage struct {
	Bookmarks  []MyBookmark `json:"bookmarks"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// NotePage is one page of the caller's notes, as BookmarkPage.
type NotePage struct {
	Notes      []MyNote `json:"notes"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// HistoryPage is one page of the caller's listening history, as BookmarkPage.
type HistoryPage struct {
	History    []HistoryEntry `json:"history"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// ListMyBookmarks returns a page of the user's bookmarks on paths their current
// access still reaches (scopes, see UserScopes; none yields none), newest first
// (created_at, then id), each with its book when one is indexed. A bookmark under
// a revoked share stays stored but is not returned. ErrInvalidCursor for a cursor
// that doesn't decode.
func (c *Catalog) ListMyBookmarks(ctx context.Context, userID int64, scopes []Scope, opt PageOptions) (*BookmarkPage, error) {
	items, next, err := userPage(ctx, c, userID, scopes, opt, pageQuery{
		cols: bookmarkColumns, from: "bookmarks", key: "created_at",
	}, func(rows *sql.Rows, b *MyBookmark) error {
		return scanBookmark(rows, &b.Bookmark)
	}, func(b *MyBookmark) (string, int64, Ref, **Book) {
		return b.CreatedAt, b.ID, b.Ref, &b.Book
	})
	if err != nil {
		return nil, err
	}
	return &BookmarkPage{Bookmarks: items, NextCursor: next}, nil
}

// ListMyNotes is ListMyBookmarks for the user's notes (newest created_at first).
func (c *Catalog) ListMyNotes(ctx context.Context, userID int64, scopes []Scope, opt PageOptions) (*NotePage, error) {
	items, next, err := userPage(ctx, c, userID, scopes, opt, pageQuery{
		cols: noteColumns, from: "notes", key: "created_at",
	}, func(rows *sql.Rows, n *MyNote) error {
		return scanNote(rows, &n.Note)
	}, func(n *MyNote) (string, int64, Ref, **Book) {
		return n.CreatedAt, n.ID, n.Ref, &n.Book
	})
	if err != nil {
		return nil, err
	}
	return &NotePage{Notes: items, NextCursor: next}, nil
}

// ListAllHistory is ListMyBookmarks for the user's listening history across
// books (newest ended_at first). The access filter is in the query, so a page
// counts only rows the caller can still reach.
func (c *Catalog) ListAllHistory(ctx context.Context, userID int64, scopes []Scope, opt PageOptions) (*HistoryPage, error) {
	items, next, err := userPage(ctx, c, userID, scopes, opt, pageQuery{
		cols: historyColumns, from: "listening_history", key: "ended_at",
	}, func(rows *sql.Rows, h *HistoryEntry) error {
		return scanHistoryRow(rows, &h.History)
	}, func(h *HistoryEntry) (string, int64, Ref, **Book) {
		return h.EndedAt, h.ID, h.Ref, &h.Book
	})
	if err != nil {
		return nil, err
	}
	return &HistoryPage{History: items, NextCursor: next}, nil
}

// pageQuery names one user-state table a userPage reads: its columns, the table
// and the text column it orders by (newest first, id breaking ties). All three are
// constants of this file, never input.
type pageQuery struct {
	cols, from, key string
}

// userPage reads one page of a user's rows of q.from newest first, keyset-paged
// on (q.key, id) so a tie in the timestamp neither repeats nor skips a row, with
// the access filter applied in the query before the page is cut. key gives a row's
// keyset values and the slot for its book, which is attached for the whole page in
// one batch (attachBooks). The cursor is opaque (encodeCursor): ErrInvalidCursor
// when it doesn't decode.
func userPage[T any](ctx context.Context, c *Catalog, userID int64, scopes []Scope, opt PageOptions, q pageQuery,
	scan func(*sql.Rows, *T) error, key func(*T) (string, int64, Ref, **Book)) ([]T, string, error) {
	filter, fargs := scopesFilterSQL("library_id", "rel_path", scopes)
	where := `user_id = ? AND ` + filter
	args := append([]any{userID}, fargs...)
	if opt.Cursor != "" {
		val, id, err := decodeCursor(opt.Cursor)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrInvalidCursor, err)
		}
		where += ` AND (` + q.key + `, id) < (?, ?)`
		args = append(args, val, id)
	}
	limit := clampHistoryLimit(opt.Limit)
	items, err := queryRows(ctx, c.db, scan, `SELECT `+q.cols+` FROM `+q.from+` WHERE `+where+
		` ORDER BY `+q.key+` DESC, id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, "", err
	}
	var next string
	if len(items) > limit {
		items = items[:limit]
		val, id, _, _ := key(&items[limit-1])
		next = encodeCursor(val, id)
	}
	err = attachBooks(ctx, c, items, func(it *T) (Ref, **Book) {
		_, _, ref, book := key(it)
		return ref, book
	})
	if err != nil {
		return nil, "", err
	}
	return items, next, nil
}

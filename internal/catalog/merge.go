package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// MaxMergeBooks bounds the books one merge changes, so its single writer
// transaction stays short on a slow disk (3,000 books take about 0.6 s on an
// SSD) and its Undo fits one request. A bigger merge is ErrMergeTooLarge.
const MaxMergeBooks = 5000

// ErrMergeTooLarge is a merge that would change, or an Undo that names, more
// than MaxMergeBooks books. Nothing is written.
var ErrMergeTooLarge = errors.New("too many books for one merge")

// Why UndoMerge left a book as it is.
const (
	// UndoGone: no book is indexed at the path now (a scan moved or removed it).
	UndoGone = "not_found"
	// UndoChanged: the book no longer carries the merge's spelling as its
	// override (edited, reverted or merged again since).
	UndoChanged = "changed"
)

// UndoneMerge is what UndoMerge did: how many books it put back, and the books
// it left as they are, each with why (UndoGone, UndoChanged).
type UndoneMerge struct {
	Restored int          `json:"restored"`
	Skipped  []UndoneSkip `json:"skipped"`
}

// UndoneSkip is one book UndoMerge left as it is.
type UndoneSkip struct {
	Ref
	Reason string `json:"reason"`
}

// MergedBook records how to undo a spelling merge on one book. With no previous
// override, reverting the field lets the book follow its tags and other layers
// again. Otherwise Undo sets the stored value with its original source.
type MergedBook struct {
	Ref
	Previous *string `json:"previous"`
	Source   string  `json:"source,omitempty"`
}

// MergeSpellings edits books whose whole credit or main series is one of the
// source spellings. A zero libraryID includes every book library; another is
// ErrNotFound when there is no such library, ErrNotBookLibrary for a podcast
// one. Selection, the Undo values and all the edits share the writer
// transaction; more than MaxMergeBooks books is ErrMergeTooLarge.
func (c *Catalog) MergeSpellings(ctx context.Context, libraryID int64, field string, sources []string, target string, userID int64) ([]MergedBook, error) {
	column, err := mergeColumn(field)
	if err != nil {
		return nil, err
	}
	if libraryID < 0 {
		return nil, invalid("library_id", "must not be negative")
	}
	edit, err := (BookEdit{Set: map[string]string{field: target}, UserID: userID}).normalize()
	if err != nil {
		return nil, err
	}
	target = edit.Set[field]
	if target == "" {
		return nil, invalid("target", "a target spelling is required")
	}
	if len(sources) == 0 {
		return nil, invalid("sources", "at least one source spelling is required")
	}
	for _, name := range sources {
		if blankIfInvisible(strings.TrimSpace(name)) == "" {
			return nil, invalid("sources", "a source spelling is empty")
		}
		if field == FieldSeries && seriesKey(name) != seriesKey(target) {
			return nil, invalid("sources", "the spellings must name the same series")
		}
	}

	// The spellings go in as one JSON list, so the statement's text (the
	// statement cache's key) is the same however many there are.
	list, err := json.Marshal(sources)
	if err != nil {
		return nil, err
	}
	where := inBookLibrary + " AND " + column + " IN (SELECT value FROM json_each(?)) AND " + column + " <> ?"
	args := []any{field, string(list), target}
	if libraryID != 0 {
		where += " AND b.library_id = ?"
		args = append(args, libraryID)
	}
	if field == FieldSeries {
		// Implied by the spellings sharing the target's key; it lets the query use idx_books_series_key.
		where += " AND b.series_key = ?"
		args = append(args, seriesKey(target))
	}
	var books []MergedBook
	err = c.db.WithTx(ctx, "MergeSpellings", func(tx *sql.Tx) error {
		if libraryID != 0 {
			var kind string
			err := tx.QueryRowContext(ctx, `SELECT kind FROM libraries WHERE id = ?`, libraryID).Scan(&kind)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				return ErrNotFound
			case err != nil:
				return err
			case kind != KindBooks:
				return ErrNotBookLibrary
			}
		}
		found, err := queryRows(ctx, tx, func(r *sql.Rows, b *MergedBook) error {
			return r.Scan(&b.LibraryID, &b.Path, &b.Previous, &b.Source)
		}, `SELECT b.library_id, b.rel_path, o.value, COALESCE(o.source, '') FROM books b
		    LEFT JOIN book_overrides o ON o.library_id = b.library_id AND o.path = b.rel_path AND o.field = ?
		    WHERE `+where+` ORDER BY b.library_id, b.rel_path LIMIT `+strconv.Itoa(MaxMergeBooks+1), args...)
		if err != nil {
			return err
		}
		if len(found) > MaxMergeBooks {
			return ErrMergeTooLarge
		}
		books = found
		refs := make([]Ref, len(books))
		for i, b := range books {
			refs[i] = b.Ref
		}
		return c.editRefsTx(ctx, tx, refs, edit)
	})
	if err != nil {
		return nil, err
	}
	if len(books) != 0 {
		c.changed()
	}
	return books, nil
}

// mergeColumn is the books column a merge of field compares.
func mergeColumn(field string) (string, error) {
	switch field {
	case FieldAuthor:
		return "b.author", nil
	case FieldNarrator:
		return "b.narrator", nil
	case FieldSeries:
		return "b.series", nil
	}
	return "", invalid("field", "must be author, narrator or series")
}

// UndoMerge puts back what MergeSpellings changed, from its reply (books) and
// the target it was asked for, in one writer transaction. A book still
// carrying target as its override gets its previous override back (with its
// source) or none; a book no longer indexed at its path, or whose override
// changed since, is left as it is and listed, and the rest still go back. A
// book named twice counts once.
func (c *Catalog) UndoMerge(ctx context.Context, field, target string, books []MergedBook, userID int64) (UndoneMerge, error) {
	done := UndoneMerge{Skipped: []UndoneSkip{}}
	if _, err := mergeColumn(field); err != nil {
		return done, err
	}
	target, err := normalizeOverride(field, target)
	if err != nil {
		return done, err
	}
	if target == "" {
		return done, invalid("target", "a target spelling is required")
	}
	if len(books) == 0 {
		return done, invalid("books", "at least one book is required")
	}
	if len(books) > MaxMergeBooks {
		return done, ErrMergeTooLarge
	}
	type undo struct {
		ref  Ref
		edit BookEdit
	}
	undos := make([]undo, 0, len(books))
	seen := make(map[Ref]bool, len(books))
	for _, b := range books {
		ref := Ref{LibraryID: b.LibraryID, Path: CleanRelPath(b.Path)}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		edit := BookEdit{Revert: []string{field}, UserID: userID}
		if b.Previous != nil {
			edit = BookEdit{Set: map[string]string{field: *b.Previous}, Source: b.Source, UserID: userID}
		}
		edit, err := edit.normalize()
		if err != nil {
			return done, err
		}
		undos = append(undos, undo{ref, edit})
	}
	err = c.db.WithTx(ctx, "UndoMerge", func(tx *sql.Tx) error {
		restored, skipped := 0, []UndoneSkip{}
		now := c.ts()
		for _, u := range undos {
			var value sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT o.value FROM books b
			    LEFT JOIN book_overrides o ON o.library_id = b.library_id AND o.path = b.rel_path AND o.field = ?
			    WHERE b.library_id = ? AND b.rel_path = ? AND `+inBookLibrary, field, u.ref.LibraryID, u.ref.Path).Scan(&value)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				skipped = append(skipped, UndoneSkip{u.ref, UndoGone})
				continue
			case err != nil:
				return err
			case !value.Valid || value.String != target:
				skipped = append(skipped, UndoneSkip{u.ref, UndoChanged})
				continue
			}
			if err := c.editRefTx(ctx, tx, u.ref, u.edit, now); err != nil {
				return err
			}
			restored++
		}
		done.Restored, done.Skipped = restored, skipped
		return nil
	})
	if err != nil {
		return UndoneMerge{Skipped: []UndoneSkip{}}, err
	}
	if done.Restored > 0 {
		c.changed()
	}
	return done, nil
}

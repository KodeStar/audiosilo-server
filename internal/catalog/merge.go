package catalog

import (
	"context"
	"database/sql"
	"strings"
)

// MergedBook records how to undo a spelling merge on one book. With no previous
// override, reverting the field lets the book follow its tags and other layers
// again. Otherwise Undo sets the stored value with its original source.
type MergedBook struct {
	Ref
	Previous *string `json:"previous"`
	Source   string  `json:"source,omitempty"`
}

// MergeSpellings edits books whose whole credit or main series is one of the
// source spellings. A zero libraryID includes every book library. Selection,
// the Undo values and all the edits share the writer transaction.
func (c *Catalog) MergeSpellings(ctx context.Context, libraryID int64, field string, sources []string, target string, userID int64) ([]MergedBook, error) {
	var column string
	switch field {
	case FieldAuthor:
		column = "b.author"
	case FieldNarrator:
		column = "b.narrator"
	case FieldSeries:
		column = "b.series"
	default:
		return nil, invalid("field", "must be author, narrator or series")
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

	where := inBookLibrary + " AND " + column + " IN (" + placeholders(len(sources)) + ") AND " + column + " <> ?"
	args := append([]any{field}, anyOf(sources)...)
	args = append(args, target)
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
		books, err = queryRows(ctx, tx, func(r *sql.Rows, b *MergedBook) error {
			return r.Scan(&b.LibraryID, &b.Path, &b.Previous, &b.Source)
		}, `SELECT b.library_id, b.rel_path, o.value, COALESCE(o.source, '') FROM books b
		    LEFT JOIN book_overrides o ON o.library_id = b.library_id AND o.path = b.rel_path AND o.field = ?
		    WHERE `+where+` ORDER BY b.library_id, b.rel_path`, args...)
		if err != nil {
			return err
		}
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

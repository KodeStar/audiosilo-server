package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
)

// A book's genres and tags are labels: a list of short names each, with no order
// that means anything beyond the one they were given in. Genres come from the
// files (the genre tag, metadata.SplitGenres) and an Audiobookshelf
// metadata.json; tags only from that file and an admin's edit (no audio tag
// carries them). Both are override fields like any other - provenance, revert,
// undo and a bulk edit work on them unchanged - whose value is the list as
// canonical JSON ("" for none), as more_series is. A bulk edit can also add
// labels to each book's own list or take some away (BookEdit.Add, Remove), since
// setting one list on many books would wipe what each had.
//
// A label is one label however it is cased ("Sci-Fi" and "sci-fi" are one tag),
// spacing aside: refreshEffective writes each book's labels to book_labels by
// that key (labelKey), which the genre= and tag= filters and the facet counts
// read by its index. Derived, like book_more_series: rebuilt with the books row.

// The label fields, which are also the kinds book_labels files them under and
// the facet dimensions of their filters.
const (
	FieldGenres = "genres"
	FieldTags   = "tags"
)

// labelFields is the label fields, in display order.
var labelFields = []string{FieldGenres, FieldTags}

// Bounds on a label list: a real book has a handful, and a label is a word or
// a short phrase.
const (
	maxLabelLen = 64
	maxLabels   = 50
)

// labelKey is the key a label is one label by: its words, lowercased.
func labelKey(name string) string { return strings.ToLower(collapseSpace(name)) }

// collapseSpace trims s and joins its words with one space.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// hasLabel says whether list holds the label name (labelKey: any case).
func hasLabel(list []string, name string) bool {
	return slices.ContainsFunc(list, func(l string) bool { return labelKey(l) == labelKey(name) })
}

// uniqueLabels is in as names: each trimmed with its inner spacing collapsed,
// blanks dropped, each label once (labelKey: the first spelling kept). Nil for
// none.
func uniqueLabels(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		if name := collapseSpace(raw); name != "" && !seen[labelKey(name)] {
			seen[labelKey(name)] = true
			out = append(out, name)
		}
	}
	return out
}

// cleanLabels is in as a label list (uniqueLabels), checked. Strict (an edit)
// refuses a label over maxLabelLen, one with a control character and a list
// over maxLabels, naming field; lenient (what a file says) leaves such a label
// out and keeps the first maxLabels, so one odd value never costs a book the
// rest. Nil for none.
func cleanLabels(field string, in []string, strict bool) ([]string, error) {
	var out []string
	for _, name := range uniqueLabels(in) {
		if err := checkText(field, name, maxLabelLen); err != nil {
			if strict {
				return nil, invalid(field, "a label must be at most 64 characters, with no control characters")
			}
			continue
		}
		out = append(out, name)
	}
	if len(out) > maxLabels {
		if strict {
			return nil, invalid(field, "too many labels (at most 50)")
		}
		out = out[:maxLabels]
	}
	return out, nil
}

// CleanLabels is a label list a file gives, as a label field stores it (the
// lenient cleanLabels): what the scan and an ABS file record.
func CleanLabels(in []string) []string {
	out, _ := cleanLabels("", in, false)
	return out
}

// normalizeLabels validates a label field's value (a JSON list of strings) and
// returns its canonical form ("" for an empty list).
func normalizeLabels(field, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	var in []string
	if err := json.Unmarshal([]byte(v), &in); err != nil {
		return "", invalid(field, "must be a list of names")
	}
	out, err := cleanLabels(field, in, true)
	if err != nil {
		return "", err
	}
	return EncodeLabels(out), nil
}

// EncodeLabels is the canonical value of a label list ("" for none;
// encodeCanonical, so "Growing Up & Facts of Life" compares equal to the
// console's).
func EncodeLabels(labels []string) string { return encodeCanonical(labels) }

// ParseLabels reads a stored label value (canonical JSON; "" for none). A value
// it can't read names no label.
func ParseLabels(v string) []string { return parseCanonical[string](v) }

// mergeLabels is the stored list cur with the labels of add that it lacks
// appended and those remove names (by labelKey) taken out: how a bulk edit
// changes each book's own list. add is already clean (cleanLabels) and names
// none of remove (BookEdit.normalize); a list that would pass maxLabels is
// refused.
func mergeLabels(field, cur string, add, remove []string) (string, error) {
	drop := map[string]bool{}
	for _, r := range remove {
		drop[labelKey(r)] = true
	}
	out := slices.DeleteFunc(ParseLabels(cur), func(l string) bool { return drop[labelKey(l)] })
	for _, a := range add {
		if !hasLabel(out, a) {
			out = append(out, a)
		}
	}
	if len(out) > maxLabels {
		return "", invalid(field, "too many labels (at most 50)")
	}
	return EncodeLabels(out), nil
}

// normalizeLabelEdits checks a BookEdit's Add and Remove and returns them
// clean: only the label fields take either, never a field the edit also sets or
// reverts, and never one label both added and removed. Added labels are clean as
// an edit stores them (the strict cleanLabels); removed ones are names
// (uniqueLabels), with no bound, since one no book could have removes nothing.
func normalizeLabelEdits(e BookEdit) (add, remove map[string][]string, err error) {
	for _, edits := range []map[string][]string{e.Add, e.Remove} {
		for field := range edits {
			if !slices.Contains(labelFields, field) {
				return nil, nil, invalid(field, "only genres and tags take add and remove")
			}
			if touches(e, field) {
				return nil, nil, invalid(field, "cannot be set or reverted and changed by add or remove at once")
			}
		}
	}
	add, remove = map[string][]string{}, map[string][]string{}
	for _, field := range labelFields {
		if _, ok := e.Add[field]; ok {
			if add[field], err = cleanLabels(field, e.Add[field], true); err != nil {
				return nil, nil, err
			}
		}
		if _, ok := e.Remove[field]; ok {
			remove[field] = uniqueLabels(e.Remove[field])
		}
		for _, name := range remove[field] {
			if hasLabel(add[field], name) {
				return nil, nil, invalid(field, "a label cannot be added and removed at once")
			}
		}
	}
	return add, remove, nil
}

// labelSets is what an edit's Add and Remove make of one book's lists: the new
// value of each list field it changes (the book's effective list, books.genres /
// tags, with the labels added and taken out), leaving out a list it leaves as it
// was, so adding a tag a book already has edits nothing on it.
func labelSets(ctx context.Context, tx *sql.Tx, bookID int64, edit BookEdit) (map[string]string, error) {
	if len(edit.Add) == 0 && len(edit.Remove) == 0 {
		return nil, nil
	}
	var genres, tags string
	if err := tx.QueryRowContext(ctx, `SELECT genres, tags FROM books WHERE id = ?`, bookID).Scan(&genres, &tags); err != nil {
		return nil, err
	}
	cur := map[string]string{FieldGenres: genres, FieldTags: tags}
	out := map[string]string{}
	for _, field := range labelFields {
		add, remove := edit.Add[field], edit.Remove[field]
		if len(add) == 0 && len(remove) == 0 {
			continue
		}
		v, err := mergeLabels(field, cur[field], add, remove)
		if err != nil {
			return nil, err
		}
		if v != cur[field] {
			out[field] = v
		}
	}
	return out, nil
}

// writeLabels writes book_labels for a book: each label of its label fields
// (value: a field's effective value) by key, with the spelling the book uses.
func writeLabels(ctx context.Context, tx *sql.Tx, bookID, libID int64, value func(field string) string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM book_labels WHERE book_id = ?`, bookID); err != nil {
		return err
	}
	for _, kind := range labelFields {
		for _, name := range ParseLabels(value(kind)) {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO book_labels(book_id, library_id, kind, label_key, name) VALUES(?, ?, ?, ?, ?)`,
				bookID, libID, kind, labelKey(name), name); err != nil {
				return err
			}
		}
	}
	return nil
}

// labelFilters is the WHERE conditions, and their args, for the genre= and tag=
// filters (genre, tag: "" for none) on the books row whose columns carry prefix,
// leaving out the facet dimension skip: the books that have the label named, in
// any case (labelKey), found by book_labels' index, in library libID (0: any).
func labelFilters(prefix string, libID int64, genre, tag, skip string) (conds []string, args []any) {
	for _, f := range []struct{ kind, name string }{{FieldGenres, genre}, {FieldTags, tag}} {
		if f.name == "" || f.kind == skip {
			continue
		}
		cond := "SELECT bl.book_id FROM book_labels bl WHERE bl.kind = ? AND bl.label_key = ?"
		args = append(args, f.kind, labelKey(f.name))
		if libID != 0 {
			cond += " AND bl.library_id = ?"
			args = append(args, libID)
		}
		conds = append(conds, prefix+"id IN ("+cond+")")
	}
	return conds, args
}

// ftsLabels is the full-text index's text for a label list: a label a line.
func ftsLabels(v string) string { return strings.Join(ParseLabels(v), "\n") }

// SetGenres records, by path, the genres a read of each book's primary file
// found (the scanner's backfill for rows indexed before migration 0049, which
// never had their genre tag read), marking each checked: the genres go into the
// book's scanned snapshot, as an index would have put them, and the book is
// re-resolved when that changes its snapshot. It commits every pathReadingBatch
// books.
func (c *Catalog) SetGenres(ctx context.Context, libraryID int64, genres map[string][]string) error {
	return c.eachPathBatch(ctx, "SetGenres", slices.Collect(maps.Keys(genres)),
		func(tx *sql.Tx, relPath string) (bool, error) {
			return setGenres(ctx, tx, libraryID, relPath, genres[relPath])
		})
}

func setGenres(ctx context.Context, tx *sql.Tx, libraryID int64, relPath string, genres []string) (bool, error) {
	id, err := bookIDByPath(ctx, tx, libraryID, relPath)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE books SET genres_checked = 1 WHERE id = ?`, id); err != nil {
		return false, err
	}
	l, err := loadLayers(ctx, tx, id)
	if err != nil {
		return false, err
	}
	v := EncodeLabels(CleanLabels(genres))
	if l.scanned[FieldGenres] == v {
		return false, nil // most books: no genre, or only a placeholder
	}
	snap := maps.Clone(l.scanned)
	snap[FieldGenres] = v
	if l.pathRev != "" {
		snap[scannedRevKey] = l.pathRev
	}
	if err := recordScanned(ctx, tx, id, l, snap); err != nil {
		return false, err
	}
	return true, refreshEffective(ctx, tx, id)
}

package catalog

import (
	"context"
	"database/sql"
	"strings"
	"unicode"
)

// ftsText is the full-text index's form of a value (server#178): each Han,
// Hiragana or Katakana character set apart by spaces, so FTS5's unicode61
// tokenizer, which splits only on spaces and punctuation, indexes a
// Chinese or Japanese title a character a token. A search for 猫 then finds
// 吾輩は猫である, which unicode61 alone reads as one token that only a search
// from its first character finds. Everything else is left as it is, and the
// result reads the same through it again (the spaces it adds stop it adding
// more), so a row already in this form is not rewritten (IndexSearchText).
func ftsText(s string) string {
	if !strings.ContainsFunc(s, ftsSplitRune) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(s)/2)
	apart := false // the last rune written is one set apart
	for i, r := range s {
		split := ftsSplitRune(r)
		if (split || apart) && i > 0 && !unicode.IsSpace(r) && !endsInSpace(&b) {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		apart = split
	}
	return b.String()
}

// endsInSpace reports whether b's text ends in a space (or is empty).
func endsInSpace(b *strings.Builder) bool {
	s := b.String()
	return s == "" || s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\t'
}

// ftsSplitRune is a character ftsText sets apart: Han (and 々, its iteration
// mark), Hiragana and Katakana (and ー / ｰ, the long-vowel marks, which Unicode
// files under no script of their own). Hangul is left whole: Korean puts
// spaces between its words.
func ftsSplitRune(r rune) bool {
	switch {
	case r < 0x2E80:
		return false
	case r == 0x3005, r == 0x30FC, r == 0xFF70:
		return true
	}
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana)
}

// searchTextKey is the server_state row holding the searchTextVersion the
// full-text index's rows were written with.
const searchTextKey = "search_text"

// searchTextVersion names what an index row was written with: ftsText's rules
// (the leading number: bump it whenever ftsText changes what it writes) and the
// Unicode tables behind ftsSplitRune.
var searchTextVersion = "1/" + unicode.Version

// IndexSearchText rewrites the full-text index's rows that ftsText reads
// otherwise: once per database for the rows written before it (server#178), and
// again whenever searchTextVersion changes. Only a row with a character it sets
// apart changes, so a library with none is read and left as it is. One
// transaction, before the server answers; it returns how many rows it rewrote,
// 0 while the index is current (server_state searchTextKey).
func (c *Catalog) IndexSearchText(ctx context.Context) (int, error) {
	if v, err := getServerState[string](ctx, c.db, searchTextKey); err != nil || v == searchTextVersion {
		return 0, err
	}
	rewritten := 0
	err := c.db.WithTx(ctx, "IndexSearchText", func(tx *sql.Tx) error {
		type row struct {
			id   int64
			cols [6]string
		}
		rows, err := queryRows(ctx, tx, func(r *sql.Rows, w *row) error {
			var cols [6]sql.NullString
			if err := r.Scan(&w.id, &cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5]); err != nil {
				return err
			}
			for i, c := range cols {
				w.cols[i] = c.String
			}
			return nil
		}, `SELECT rowid, title, author, series, narrator, genres, tags FROM books_fts`)
		if err != nil {
			return err
		}
		for _, w := range rows {
			changed := false
			var cols [6]any
			for i, c := range w.cols {
				t := ftsText(c)
				changed = changed || t != c
				cols[i] = t
			}
			if !changed {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM books_fts WHERE rowid = ?`, w.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO books_fts(rowid, title, author, series, narrator, genres, tags) VALUES(?,?,?,?,?,?,?)`,
				append([]any{w.id}, cols[:]...)...); err != nil {
				return err
			}
			rewritten++
		}
		return putServerState(ctx, tx, searchTextKey, searchTextVersion, c.stamp())
	})
	return rewritten, err
}

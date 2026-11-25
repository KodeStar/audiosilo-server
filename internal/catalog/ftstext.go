package catalog

import (
	"context"
	"database/sql"
	"slices"
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
	prev := ' '    // the last rune written (a space before the first)
	for _, r := range s {
		split := ftsSplitRune(r)
		if (split || apart) && !unicode.IsSpace(r) && prev != ' ' && prev != '\n' && prev != '\t' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		apart, prev = split, r
	}
	return b.String()
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

// IndexSearchText rebuilds outdated search text from each book's effective
// fields when the tokenizer version changes. Comparing against the index
// first avoids rewriting rows whose tokens are already current.
func (c *Catalog) IndexSearchText(ctx context.Context) (int, error) {
	if v, err := getServerState[string](ctx, c.db, searchTextKey); err != nil || v == searchTextVersion {
		return 0, err
	}
	rewritten := 0
	err := c.db.WithTx(ctx, "IndexSearchText", func(tx *sql.Tx) error {
		type row struct {
			id        int64
			cols, old [6]string
		}
		changed, err := queryRows(ctx, tx, func(r *sql.Rows, w *row) error {
			var title, author, series, more, narrator, genres, tags string
			if err := r.Scan(&w.id, &title, &author, &series, &more, &narrator, &genres, &tags,
				&w.old[0], &w.old[1], &w.old[2], &w.old[3], &w.old[4], &w.old[5]); err != nil {
				return err
			}
			w.cols = searchColumns(title, author, series, more, narrator, genres, tags)
			return nil
		}, `SELECT b.id, b.title, b.author, b.series, b.more_series, b.narrator, b.genres, b.tags,
			COALESCE(f.title,''), COALESCE(f.author,''), COALESCE(f.series,''), COALESCE(f.narrator,''), COALESCE(f.genres,''), COALESCE(f.tags,'')
			FROM books_fts f JOIN books b ON b.id = f.rowid`)
		if err != nil {
			return err
		}
		changed = slices.DeleteFunc(changed, func(w row) bool { return w.cols == w.old })
		for _, w := range changed {
			if err := writeFTSRow(ctx, tx, w.id, w.cols); err != nil {
				return err
			}
			rewritten++
		}
		return putServerState(ctx, tx, searchTextKey, searchTextVersion, c.stamp())
	})
	return rewritten, err
}

func searchColumns(title, author, series, more, narrator, genres, tags string) [6]string {
	return [6]string{ftsText(title), ftsText(author), ftsText(ftsSeries(series, more)),
		ftsText(narrator), ftsText(ftsLabels(genres)), ftsText(ftsLabels(tags))}
}

// writeFTSRow writes book id's full-text index row (delete-then-insert, keyed
// by rowid = book id) from cols, its title, author, series, narrator, genres
// and tags, each in ftsText's form (searchColumns).
func writeFTSRow(ctx context.Context, tx *sql.Tx, id int64, cols [6]string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM books_fts WHERE rowid = ?`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO books_fts(rowid, title, author, series, narrator, genres, tags) VALUES(?,?,?,?,?,?,?)`,
		id, cols[0], cols[1], cols[2], cols[3], cols[4], cols[5])
	return err
}

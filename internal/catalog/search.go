package catalog

import (
	"context"
	"strings"
	"unicode"
)

// Search runs a full-text query over the catalog (title/author/series/narrator)
// restricted to the caller's access scopes (per-library share path rules). It
// uses FTS5 so it stays fast at thousands of books; results are ranked by FTS
// relevance.
func (c *Catalog) Search(ctx context.Context, query string, scopes []Scope, limit int) ([]Book, error) {
	match := buildMatchQuery(query)
	if match == "" || len(scopes) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	args := []any{match}
	var scopeConds []string
	for _, s := range scopes {
		frag, fargs := pathFilterSQL("b.rel_path", s)
		scopeConds = append(scopeConds, "(b.library_id = ? AND "+frag+")")
		args = append(args, s.LibraryID)
		args = append(args, fargs...)
	}
	// Over-fetch ranked rows so de-dup can still return up to `limit` distinct books.
	args = append(args, dedupFetch(limit))

	q := `SELECT ` + prefixCols("b.") + dedupCols + `
		FROM books_fts f
		JOIN books b ON b.id = f.rowid` + dedupJoins + `
		WHERE books_fts MATCH ? AND (` + strings.Join(scopeConds, " OR ") + `)
		ORDER BY rank LIMIT ?`
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cands []candidate
	for rows.Next() {
		cand, err := scanCandidate(rows, len(cands))
		if err != nil {
			return nil, err
		}
		cands = append(cands, cand)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dedupBooks(cands, limit), nil
}

func prefixCols(p string) string {
	cols := strings.Split(bookCols, ",")
	for i, c := range cols {
		cols[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}

// buildMatchQuery turns free user input into a safe FTS5 prefix query. Each
// token becomes a prefix match ("foo*") and tokens are ANDed, which gives
// type-ahead behavior without exposing FTS5 operator syntax to the user.
//
// Tokens are split on anything that is not a letter or digit in ANY script, so
// non-Latin titles (Cyrillic, CJK, accented Latin) search correctly - the FTS5
// unicode61 tokenizer indexed them, and splitting on ASCII-only ranges would
// discard every non-ASCII rune and return no results. A token only ever contains
// letters/digits, so it can never carry an embedded quote or FTS operator, which
// keeps the quoted-prefix construction injection-safe.
func buildMatchQuery(input string) string {
	fields := strings.FieldsFunc(input, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	var terms []string
	for _, f := range fields {
		if len(f) == 0 {
			continue
		}
		terms = append(terms, `"`+f+`"*`)
	}
	return strings.Join(terms, " AND ")
}

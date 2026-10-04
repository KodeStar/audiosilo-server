package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// The admin catalog: the queries behind the console's Library screens. Admin-only
// and unscoped (an admin sees every library); addressed by (library_id, path) like
// everything else, with the internal book id only ever inside an opaque cursor.

// AdminBook is one row of the admin book list.
type AdminBook struct {
	id             int64
	LibraryID      int64   `json:"library_id"`
	LibraryName    string  `json:"library_name"`
	Path           string  `json:"path"`
	IsFolder       bool    `json:"is_folder"`
	Title          string  `json:"title"`
	Author         string  `json:"author"`
	Narrator       string  `json:"narrator"`
	Series         string  `json:"series"`
	SeriesIndex    float64 `json:"series_index"`
	Published      string  `json:"published"`
	Duration       float64 `json:"duration"`
	Format         string  `json:"format"`
	Codec          string  `json:"codec"`
	DirectPlayable bool    `json:"direct_playable"`
	Size           int64   `json:"size"`
	AddedAt        string  `json:"added_at"`
	HasCover       bool    `json:"has_cover"`
	CustomCover    bool    `json:"custom_cover"`
	ChapterCount   int     `json:"chapter_count"`
	FileCount      int     `json:"file_count"`
	ASIN           string  `json:"asin"`
	ISBN           string  `json:"isbn"`
	// Matched is the matched= filter's rule (an ASIN or ISBN), so the console never
	// restates it.
	Matched bool `json:"matched"`
	Edited  bool `json:"edited"`
	// EditedFields are the fields with an override (an edit or an accepted community
	// value), so a bulk change can be undone field by field: a field without one is
	// reverted to what the scan found, one with one is set back.
	EditedFields fieldList `json:"edited_fields"`
}

// fieldList scans a comma-separated SQL list (group_concat) into field names,
// empty rather than null when there are none.
type fieldList []string

func (f *fieldList) Scan(src any) error {
	*f = fieldList{}
	var s string
	switch v := src.(type) {
	case nil:
		return nil
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("fieldList: unexpected %T", src)
	}
	if s != "" {
		*f = strings.Split(s, ",")
	}
	return nil
}

// Per-row expressions over `books b`, shared by the select list, the filters and
// the facets so the three can never disagree about what "has a cover" means.
const (
	customCoverExpr = `EXISTS(SELECT 1 FROM book_covers cv WHERE cv.library_id = b.library_id AND cv.path = b.rel_path)`
	// has_cover covers sibling and embedded art (NULL until a scan checks); a
	// custom cover counts too.
	hasCoverExpr     = `(COALESCE(b.has_cover, 0) = 1 OR ` + customCoverExpr + `)`
	chapterCountExpr = `(SELECT COUNT(*) FROM chapters ch WHERE ch.book_id = b.id)`
	// A single-file book has no book_files rows; it is one file.
	fileCountExpr = `MAX(1, (SELECT COUNT(*) FROM book_files bf WHERE bf.book_id = b.id))`
	matchedExpr   = `(b.asin <> '' OR b.isbn <> '')`
	// A chapter override counts only while the book has that chapter: one whose
	// chapter a rescan dropped is dormant (it reapplies if the chapter comes back) and
	// shows nowhere on the book page, so it can't mark the book edited.
	editedExpr = `(EXISTS(SELECT 1 FROM book_overrides o WHERE o.library_id = b.library_id AND o.path = b.rel_path)
		OR EXISTS(SELECT 1 FROM chapter_overrides co JOIN chapters ON chapters.book_id = b.id AND ` + chapterOverrideMatch + `
		           WHERE co.library_id = b.library_id AND co.path = b.rel_path))`
	// The overridden fields, in field order (fieldList splits them).
	editedFieldsExpr = `(SELECT group_concat(field, ',') FROM (SELECT o.field FROM book_overrides o
		WHERE o.library_id = b.library_id AND o.path = b.rel_path ORDER BY o.field))`
	// "Has chapters" means real navigation: more than the one chapter every
	// single-part book gets.
	hasChaptersExpr = `(` + chapterCountExpr + ` > 1)`
)

// directPlayableExpr is media.DirectPlayable in SQL.
var directPlayableExpr = media.DirectPlayableSQL("b.codec")

var adminBookCols = `b.id, b.library_id, l.name, b.rel_path, b.is_folder, b.title, b.author,
	b.narrator, b.series, b.series_index, b.published, b.duration, b.format, b.codec, ` +
	`b.size, b.added_at, ` + customCoverExpr + `, ` + chapterCountExpr + `, ` + fileCountExpr + `,
	b.asin, b.isbn, ` + matchedExpr + `, ` + editedExpr + `, ` + editedFieldsExpr + `, ` + hasCoverExpr + `, ` + directPlayableExpr

// adminBookDest returns the scan destinations for adminBookCols, in order.
func adminBookDest(b *AdminBook) []any {
	return []any{&b.id, &b.LibraryID, &b.LibraryName, &b.Path, &b.IsFolder, &b.Title, &b.Author,
		&b.Narrator, &b.Series, &b.SeriesIndex, &b.Published, &b.Duration, &b.Format, &b.Codec,
		&b.Size, &b.AddedAt, &b.CustomCover, &b.ChapterCount, &b.FileCount,
		&b.ASIN, &b.ISBN, &b.Matched, &b.Edited, &b.EditedFields, &b.HasCover, &b.DirectPlayable}
}

// BookFilter narrows the admin book list (and its facet counts). Zero values
// mean "no filter"; a nil *bool is "either".
type BookFilter struct {
	LibraryID      int64
	Query          string // full-text, over title/author/series/narrator
	Author         string // exact effective value
	Series         string
	Narrator       string
	Formats        []string
	Codecs         []string
	DirectPlayable *bool
	HasCover       *bool
	HasChapters    *bool
	Matched        *bool
	Edited         *bool
	MinDuration    float64 // seconds; 0 = none
	MaxDuration    float64
	AddedAfter     string // inclusive lower bound on added_at (RFC3339 or a YYYY-MM-DD prefix)
	AddedBefore    string // exclusive upper bound
}

// Facet dimensions: the filters a facet count is computed without, so the console
// can show what each other choice would give.
const (
	facetLibrary        = "library"
	facetFormat         = "format"
	facetCodec          = "codec"
	facetDirectPlayable = "direct_playable"
	facetHasCover       = "has_cover"
	facetHasChapters    = "has_chapters"
	facetMatched        = "matched"
	facetEdited         = "edited"
)

// where builds the WHERE clause (over `books b`) for f, leaving out the facet
// dimension skip ("" applies everything).
func (f BookFilter) where(skip string) (string, []any) {
	conds := []string{"1"}
	var args []any
	add := func(cond string, a ...any) {
		conds = append(conds, cond)
		args = append(args, a...)
	}
	in := func(col string, vals []string) {
		a := make([]any, len(vals))
		for i, v := range vals {
			a[i] = v
		}
		add(col+" IN ("+placeholders(len(vals))+")", a...)
	}
	flag := func(dim, expr string, v *bool) {
		if v == nil || dim == skip {
			return
		}
		if *v {
			add(expr)
		} else {
			add("NOT " + expr)
		}
	}
	if f.LibraryID != 0 && skip != facetLibrary {
		add("b.library_id = ?", f.LibraryID)
	}
	if f.Query != "" {
		// Punctuation-only input has no terms; it filters nothing rather than all.
		if m := buildMatchQuery(f.Query); m != "" {
			add("b.id IN (SELECT rowid FROM books_fts WHERE books_fts MATCH ?)", m)
		}
	}
	if f.Author != "" {
		add("b.author = ?", f.Author)
	}
	if f.Series != "" {
		add("b.series = ?", f.Series)
	}
	if f.Narrator != "" {
		add("b.narrator = ?", f.Narrator)
	}
	if len(f.Formats) > 0 && skip != facetFormat {
		in("b.format", f.Formats)
	}
	if len(f.Codecs) > 0 && skip != facetCodec {
		in("b.codec", f.Codecs)
	}
	flag(facetDirectPlayable, directPlayableExpr, f.DirectPlayable)
	flag(facetHasCover, hasCoverExpr, f.HasCover)
	flag(facetHasChapters, hasChaptersExpr, f.HasChapters)
	flag(facetMatched, matchedExpr, f.Matched)
	flag(facetEdited, editedExpr, f.Edited)
	if f.MinDuration > 0 {
		add("b.duration >= ?", f.MinDuration)
	}
	if f.MaxDuration > 0 {
		add("b.duration <= ?", f.MaxDuration)
	}
	if f.AddedAfter != "" {
		add("b.added_at >= ?", f.AddedAfter)
	}
	if f.AddedBefore != "" {
		add("b.added_at < ?", f.AddedBefore)
	}
	return strings.Join(conds, " AND "), args
}

// sortKey is one column of an admin list ordering.
type sortKey struct {
	expr string              // ORDER BY / keyset expression
	num  bool                // numeric (cursor values decode as numbers)
	val  func(AdminBook) any // the row's value, for the next cursor
	desc *sortKey            // stands in for this key in a descending order, if set
}

func textKey(col string, val func(AdminBook) string) sortKey {
	return sortKey{expr: col + " COLLATE NOCASE", val: func(b AdminBook) any { return val(b) }}
}

// blankLast sorts rows with an empty col after the rest, in either direction, so a
// series sort doesn't open on every book that has no series. A descending order
// flips every key, so there it keys on the opposite test ("not blank"), which
// keeps the whole ordering one direction for the keyset comparison.
func blankLast(col string, val func(AdminBook) string) sortKey {
	is := func(blank bool) func(AdminBook) any {
		return func(b AdminBook) any {
			if (val(b) == "") == blank {
				return 1
			}
			return 0
		}
	}
	desc := sortKey{expr: "(" + col + " <> '')", num: true, val: is(false)}
	return sortKey{expr: "(" + col + " = '')", num: true, val: is(true), desc: &desc}
}

// directed returns the keys of an ordering for its direction: in a descending
// order a key with a stand-in is replaced by it.
func directed(keys []sortKey, desc bool) []sortKey {
	if !desc {
		return keys
	}
	out := make([]sortKey, len(keys))
	for i, k := range keys {
		if k.desc != nil {
			k = *k.desc
		}
		out[i] = k
	}
	return out
}

var (
	keyTitle = textKey("b.title", func(b AdminBook) string { return b.Title })
	keyIndex = sortKey{expr: "b.series_index", num: true, val: func(b AdminBook) any { return b.SeriesIndex }}
	getSer   = func(b AdminBook) string { return b.Series }
	getAuth  = func(b AdminBook) string { return b.Author }
	getNarr  = func(b AdminBook) string { return b.Narrator }
)

// ErrUnknownSort marks an admin list ordering that isn't one of adminSorts.
var ErrUnknownSort = errors.New("unknown sort")

// adminSorts are the admin list's orderings. Each ends in the book id (added by
// the query) so the keyset is total.
var adminSorts = map[string][]sortKey{
	"title":    {keyTitle},
	"author":   {blankLast("b.author", getAuth), textKey("b.author", getAuth), textKey("b.series", getSer), keyIndex, keyTitle},
	"series":   {blankLast("b.series", getSer), textKey("b.series", getSer), keyIndex, keyTitle},
	"narrator": {blankLast("b.narrator", getNarr), textKey("b.narrator", getNarr), keyTitle},
	"added":    {{expr: "b.added_at", val: func(b AdminBook) any { return b.AddedAt }}},
	"duration": {{expr: "b.duration", num: true, val: func(b AdminBook) any { return b.Duration }}},
	"size":     {{expr: "b.size", num: true, val: func(b AdminBook) any { return b.Size }}},
}

// AdminListOptions is one page request of the admin book list.
type AdminListOptions struct {
	Filter BookFilter
	Sort   string // title | author | series | narrator | added | duration | size; "" = title
	Desc   bool
	Limit  int
	Cursor string
}

// AdminPage is one page of the admin book list.
type AdminPage struct {
	Books      []AdminBook `json:"books"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// adminCursor is the decoded keyset position. It names its ordering so a cursor
// can't be replayed against a different sort.
type adminCursor struct {
	Sort string            `json:"s"`
	Desc bool              `json:"d,omitempty"`
	Vals []json.RawMessage `json:"v"`
	ID   int64             `json:"id"`
}

// ListAdminBooks returns a page of books for the admin console, keyset-paginated
// over the chosen ordering (never OFFSET, so a deep page costs what the first does).
func (c *Catalog) ListAdminBooks(ctx context.Context, opt AdminListOptions) (*AdminPage, error) {
	if opt.Sort == "" {
		opt.Sort = "title"
	}
	keys, ok := adminSorts[opt.Sort]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSort, opt.Sort)
	}
	keys = directed(keys, opt.Desc)
	if opt.Limit <= 0 || opt.Limit > 200 {
		opt.Limit = 60
	}
	where, args := opt.Filter.where("")
	dir, cmp := "ASC", ">"
	if opt.Desc {
		dir, cmp = "DESC", "<"
	}
	exprs := make([]string, 0, len(keys)+1)
	order := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		exprs = append(exprs, k.expr)
		order = append(order, k.expr+" "+dir)
	}
	exprs = append(exprs, "b.id")
	order = append(order, "b.id "+dir)
	orderBy := strings.Join(order, ", ")

	if opt.Cursor != "" {
		vals, err := decodeAdminCursor(opt.Cursor, opt.Sort, opt.Desc, keys)
		if err != nil {
			return nil, err
		}
		where += " AND (" + strings.Join(exprs, ", ") + ") " + cmp + " (" + placeholders(len(vals)) + ")"
		args = append(args, vals...)
	}
	// Sort and page on ids alone, then compute the row's columns (several of them
	// subqueries) for that page only, not for every matching book.
	q := `SELECT ` + adminBookCols + `
		FROM (SELECT b.id FROM books b WHERE ` + where + ` ORDER BY ` + orderBy + ` LIMIT ?) page
		JOIN books b ON b.id = page.id JOIN libraries l ON l.id = b.library_id
		ORDER BY ` + orderBy
	args = append(args, opt.Limit+1)
	books, err := queryRows(ctx, c.db, func(rows *sql.Rows, b *AdminBook) error {
		return rows.Scan(adminBookDest(b)...)
	}, q, args...)
	if err != nil {
		return nil, err
	}
	page := &AdminPage{Books: books}
	if len(page.Books) > opt.Limit {
		page.Books = page.Books[:opt.Limit]
		last := page.Books[len(page.Books)-1]
		cur := adminCursor{Sort: opt.Sort, Desc: opt.Desc, ID: last.id}
		for _, k := range keys {
			raw, err := json.Marshal(k.val(last))
			if err != nil {
				return nil, err
			}
			cur.Vals = append(cur.Vals, raw)
		}
		raw, err := json.Marshal(cur)
		if err != nil {
			return nil, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

// decodeAdminCursor turns a cursor back into the keyset's bound values (one per
// sort key, then the id), refusing one minted for another ordering.
func decodeAdminCursor(s, sortName string, desc bool, keys []sortKey) ([]any, error) {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrInvalidCursor, why) }
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, bad("not base64")
	}
	var cur adminCursor
	if err := json.Unmarshal(raw, &cur); err != nil {
		return nil, bad("malformed")
	}
	if cur.Sort != sortName || cur.Desc != desc || len(cur.Vals) != len(keys) {
		return nil, bad("minted for a different ordering")
	}
	vals := make([]any, 0, len(keys)+1)
	for i, k := range keys {
		if k.num {
			var f float64
			if err := json.Unmarshal(cur.Vals[i], &f); err != nil {
				return nil, bad("malformed value")
			}
			vals = append(vals, f)
			continue
		}
		var str string
		if err := json.Unmarshal(cur.Vals[i], &str); err != nil {
			return nil, bad("malformed value")
		}
		vals = append(vals, str)
	}
	return append(vals, cur.ID), nil
}

// placeholders returns n comma-separated SQL parameter marks.
func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// FacetCount is one value of a facet and how many books have it.
type FacetCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// LibraryCount is the library facet: books per library.
type LibraryCount struct {
	LibraryID int64 `json:"library_id"`
	Count     int   `json:"count"`
}

// BoolFacet counts the books for which a yes/no property holds and doesn't.
type BoolFacet struct {
	Yes int `json:"yes"`
	No  int `json:"no"`
}

// BookFacets is the facet panel for a filter: the total, and per dimension the
// counts with every OTHER filter applied (so each choice shows what it would give).
type BookFacets struct {
	Total          int            `json:"total"`
	Libraries      []LibraryCount `json:"libraries"`
	Formats        []FacetCount   `json:"formats"`
	Codecs         []FacetCount   `json:"codecs"`
	DirectPlayable BoolFacet      `json:"direct_playable"`
	HasCover       BoolFacet      `json:"has_cover"`
	HasChapters    BoolFacet      `json:"has_chapters"`
	Matched        BoolFacet      `json:"matched"`
	Edited         BoolFacet      `json:"edited"`
}

// BookFacets computes the facet counts for f.
func (c *Catalog) BookFacets(ctx context.Context, f BookFilter) (*BookFacets, error) {
	out := &BookFacets{}
	var err error
	where, args := f.where(facetLibrary)
	if out.Libraries, err = queryRows(ctx, c.db, func(rows *sql.Rows, lc *LibraryCount) error {
		return rows.Scan(&lc.LibraryID, &lc.Count)
	}, `SELECT b.library_id, COUNT(*) FROM books b WHERE `+where+` GROUP BY b.library_id ORDER BY b.library_id`, args...); err != nil {
		return nil, err
	}
	scanFacet := func(rows *sql.Rows, fc *FacetCount) error { return rows.Scan(&fc.Value, &fc.Count) }
	for _, d := range []struct {
		dim, col string
		dst      *[]FacetCount
	}{{facetFormat, "b.format", &out.Formats}, {facetCodec, "b.codec", &out.Codecs}} {
		where, args := f.where(d.dim)
		if *d.dst, err = queryRows(ctx, c.db, scanFacet,
			`SELECT `+d.col+`, COUNT(*) FROM books b WHERE `+where+` GROUP BY 1 ORDER BY 2 DESC, 1`, args...); err != nil {
			return nil, err
		}
	}

	bools := []struct {
		dim, expr string
		filter    *bool
		dst       *BoolFacet
	}{
		{facetDirectPlayable, directPlayableExpr, f.DirectPlayable, &out.DirectPlayable},
		{facetHasCover, hasCoverExpr, f.HasCover, &out.HasCover},
		{facetHasChapters, hasChaptersExpr, f.HasChapters, &out.HasChapters},
		{facetMatched, matchedExpr, f.Matched, &out.Matched},
		{facetEdited, editedExpr, f.Edited, &out.Edited},
	}
	countYes := func(expr string) string { return `COALESCE(SUM(CASE WHEN ` + expr + ` THEN 1 ELSE 0 END), 0)` }
	// The total and every yes/no dimension that isn't itself filtered count the same
	// row set, so one pass does them all; a filtered one is counted without its own
	// filter.
	cols := []string{"COUNT(*)"}
	var unfiltered []*BoolFacet
	for _, d := range bools {
		if d.filter == nil {
			cols = append(cols, countYes(d.expr))
			unfiltered = append(unfiltered, d.dst)
		}
	}
	yes := make([]int, len(unfiltered))
	dest := []any{&out.Total}
	for i := range yes {
		dest = append(dest, &yes[i])
	}
	where, args = f.where("")
	if err := c.db.QueryRowContext(ctx,
		`SELECT `+strings.Join(cols, ", ")+` FROM books b WHERE `+where, args...).Scan(dest...); err != nil {
		return nil, err
	}
	for i, dst := range unfiltered {
		*dst = BoolFacet{Yes: yes[i], No: out.Total - yes[i]}
	}
	for _, d := range bools {
		if d.filter == nil {
			continue
		}
		where, args := f.where(d.dim)
		var total, n int
		if err := c.db.QueryRowContext(ctx,
			`SELECT COUNT(*), `+countYes(d.expr)+` FROM books b WHERE `+where, args...).Scan(&total, &n); err != nil {
			return nil, err
		}
		*d.dst = BoolFacet{Yes: n, No: total - n}
	}
	return out, nil
}

// PersonCount is one author or narrator and their books.
type PersonCount struct {
	Name     string  `json:"name"`
	Books    int     `json:"books"`
	Duration float64 `json:"duration"`
}

// MergeSuggestion is a set of spellings that look like one person ("Sanderson,
// Brandon" / "Brandon Sanderson", "J.R.R. Tolkien" / "J. R. R. Tolkien"). Merging
// is a bulk override of the field to Suggested; the server never merges on its own.
type MergeSuggestion struct {
	Names     []string `json:"names"`
	Suggested string   `json:"suggested"`
	Books     int      `json:"books"`
}

// PeopleAggregate lists the distinct values of a people field with counts, plus
// the spellings that look like duplicates.
type PeopleAggregate struct {
	People      []PersonCount
	Suggestions []MergeSuggestion
	Unknown     int // books with the field blank
}

// The people fields an aggregate can be taken over; constant queries per field.
const (
	PeopleAuthors   = "author"
	PeopleNarrators = "narrator"
)

var peopleQueries = map[string]string{
	PeopleAuthors: `SELECT author, COUNT(*), COALESCE(SUM(duration), 0) FROM books
		WHERE (? = 0 OR library_id = ?) GROUP BY author`,
	PeopleNarrators: `SELECT narrator, COUNT(*), COALESCE(SUM(duration), 0) FROM books
		WHERE (? = 0 OR library_id = ?) GROUP BY narrator`,
}

// People aggregates the authors or narrators (field = PeopleAuthors/PeopleNarrators)
// of one library, or all of them (libraryID 0). Names are the whole effective field
// value: a "Kramer & Reading" narrator credit is one entry, matching the exact
// filter and the bulk edit that act on it.
func (c *Catalog) People(ctx context.Context, field string, libraryID int64) (*PeopleAggregate, error) {
	q, ok := peopleQueries[field]
	if !ok {
		return nil, fmt.Errorf("unknown people field %q", field)
	}
	rows, err := c.db.QueryContext(ctx, q, libraryID, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &PeopleAggregate{People: []PersonCount{}, Suggestions: []MergeSuggestion{}}
	for rows.Next() {
		var p PersonCount
		if err := rows.Scan(&p.Name, &p.Books, &p.Duration); err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.Name) == "" {
			out.Unknown += p.Books
			continue
		}
		out.People = append(out.People, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out.People, func(i, j int) bool {
		return lessFold(out.People[i].Name, out.People[j].Name)
	})
	out.Suggestions = mergeSuggestions(out.People)
	return out, nil
}

func lessFold(a, b string) bool {
	if la, lb := strings.ToLower(a), strings.ToLower(b); la != lb {
		return la < lb
	}
	return a < b
}

// personKey reduces a name to a comparison key: "Surname, Given" is turned round
// (only when the part before the comma is one word, so "Alexandre Dumas, pere"
// stays whole), then case, spacing and punctuation are dropped (match.Fold, which
// keeps the letters of every script), which also equates "J.R.R." with "J. R. R.".
// "" when no letter or digit is left.
func personKey(name string) string {
	if before, after, ok := strings.Cut(name, ","); ok && !strings.Contains(after, ",") {
		if b, a := strings.TrimSpace(before), strings.TrimSpace(after); a != "" && !strings.ContainsAny(b, " \t") {
			name = a + " " + b
		}
	}
	return match.Fold(name)
}

// mergeSuggestions groups people whose names share a personKey. The suggested
// spelling is the one with the most books (ties: alphabetical).
func mergeSuggestions(people []PersonCount) []MergeSuggestion {
	groups := map[string][]PersonCount{}
	var order []string
	for _, p := range people {
		k := personKey(p.Name)
		if k == "" {
			continue
		}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p)
	}
	out := []MergeSuggestion{}
	for _, k := range order {
		g := groups[k]
		if len(g) < 2 {
			continue
		}
		best := g[0]
		s := MergeSuggestion{}
		for _, p := range g {
			s.Names = append(s.Names, p.Name)
			s.Books += p.Books
			if p.Books > best.Books || (p.Books == best.Books && lessFold(p.Name, best.Name)) {
				best = p
			}
		}
		s.Suggested = best.Name
		out = append(out, s)
	}
	return out
}

// SeriesCount is one series and the books the server holds in it.
type SeriesCount struct {
	Name      string    `json:"name"`
	Author    string    `json:"author"` // the most common author among its books
	Books     int       `json:"books"`
	Duration  float64   `json:"duration"`
	Positions []float64 `json:"positions"` // distinct non-zero positions, ascending
}

// Series aggregates the series of one library (or all, libraryID 0): books, the
// positions held (so the console can mark gaps against community series data) and
// the dominant author.
func (c *Catalog) Series(ctx context.Context, libraryID int64) ([]SeriesCount, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT series, author, series_index, duration FROM books
		  WHERE series <> '' AND (? = 0 OR library_id = ?)`, libraryID, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		SeriesCount
		authors map[string]int
		pos     map[float64]bool
	}
	bySeries := map[string]*acc{}
	for rows.Next() {
		var name, author string
		var idx, dur float64
		if err := rows.Scan(&name, &author, &idx, &dur); err != nil {
			return nil, err
		}
		a := bySeries[name]
		if a == nil {
			a = &acc{SeriesCount: SeriesCount{Name: name, Positions: []float64{}}, authors: map[string]int{}, pos: map[float64]bool{}}
			bySeries[name] = a
		}
		a.Books++
		a.Duration += dur
		if author != "" {
			a.authors[author]++
		}
		if idx > 0 && !a.pos[idx] {
			a.pos[idx] = true
			a.Positions = append(a.Positions, idx)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]SeriesCount, 0, len(bySeries))
	for _, a := range bySeries {
		sort.Float64s(a.Positions)
		for name, n := range a.authors {
			if n > a.authors[a.Author] || (n == a.authors[a.Author] && lessFold(name, a.Author)) {
				a.Author = name
			}
		}
		out = append(out, a.SeriesCount)
	}
	sort.Slice(out, func(i, j int) bool { return lessFold(out[i].Name, out[j].Name) })
	return out, nil
}

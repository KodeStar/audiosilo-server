package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// Health issues: books that would be better with an admin's attention, computed
// from the index on request (nothing is stored but an admin's "ignore this"). Every
// book kind is an SQL predicate over `books b`, so the Health page's counts and its
// lists (GET /admin/books?issue=) can't disagree; duplicates are groups, found in
// Go. The console adds one more category the index can't know: libraries whose
// root is offline (from the scanner's availability probe).

// Issue kinds.
const (
	IssueNoCover    = "no_cover"
	IssueNoChapters = "no_chapters"
	IssueUnmatched  = "unmatched"
	IssueTranscode  = "transcode"
	IssueSuspect    = "suspect"
	IssueScanError  = "scan_error"
	IssueDuplicate  = "duplicate"
)

// IssueKinds are the kinds an issue can be ignored for, in the Health page's order.
var IssueKinds = []string{IssueScanError, IssueSuspect, IssueDuplicate, IssueNoCover,
	IssueUnmatched, IssueNoChapters, IssueTranscode}

// ErrUnknownIssue marks an issue kind that isn't one of IssueKinds.
var ErrUnknownIssue = errors.New("unknown issue kind")

// longWithoutChapters is how long a book with at most one chapter must be before
// that's worth fixing (a short story needs no navigation).
const longWithoutChapters = 2 * 60 * 60 // seconds

// issuePredicates are the book kinds' conditions over `books b`.
var issuePredicates = map[string]string{
	// Only books a scan has checked: has_cover is NULL until then.
	IssueNoCover:    `(b.has_cover IS NOT NULL AND NOT ` + hasCoverExpr + `)`,
	IssueNoChapters: fmt.Sprintf(`(b.duration > %d AND NOT %s)`, longWithoutChapters, hasChaptersExpr),
	IssueUnmatched:  `(NOT ` + matchedExpr + `)`,
	IssueTranscode:  `(NOT ` + directPlayableExpr + `)`,
	IssueSuspect:    `(COALESCE(b.suspect_parts, 0) >= 2)`,
	IssueScanError:  `(b.scan_error <> '')`,
}

// ignoredExpr is "an admin ignored this book for the kind bound to its parameter".
const ignoredExpr = `EXISTS(SELECT 1 FROM issue_ignores ii
	WHERE ii.library_id = b.library_id AND ii.path = b.rel_path AND ii.kind = ?)`

// ValidIssue reports whether kind is one of IssueKinds.
func ValidIssue(kind string) bool { return slices.Contains(IssueKinds, kind) }

// IssueSample is a book shown on a category's card (its cover).
type IssueSample struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	Title     string `json:"title"`
}

// IssueCount is one category's numbers: how many need attention (for duplicates,
// how many groups) and how many an admin ignored, plus a few to show.
type IssueCount struct {
	Kind    string        `json:"kind"`
	Count   int           `json:"count"`
	Ignored int           `json:"ignored"`
	Samples []IssueSample `json:"samples"`
}

// maxIssueSamples is how many covers a category card fans out.
const maxIssueSamples = 3

// IssueCounts computes each of kinds (book kinds and duplicates, in that order).
func (c *Catalog) IssueCounts(ctx context.Context, kinds []string) ([]IssueCount, error) {
	out := make([]IssueCount, 0, len(kinds))
	for _, kind := range kinds {
		ic := IssueCount{Kind: kind, Samples: []IssueSample{}}
		if kind == IssueDuplicate {
			groups, err := c.DuplicateGroups(ctx, 0, true)
			if err != nil {
				return nil, err
			}
			for _, g := range groups {
				if g.Ignored {
					ic.Ignored++
					continue
				}
				ic.Count++
				if len(ic.Samples) < maxIssueSamples {
					m := g.Books[0]
					ic.Samples = append(ic.Samples, IssueSample{LibraryID: m.LibraryID, Path: m.Path, Title: m.Title})
				}
			}
			out = append(out, ic)
			continue
		}
		pred, ok := issuePredicates[kind]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownIssue, kind)
		}
		if err := c.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(CASE WHEN `+ignoredExpr+` THEN 0 ELSE 1 END), 0),
			        COALESCE(SUM(CASE WHEN `+ignoredExpr+` THEN 1 ELSE 0 END), 0)
			   FROM books b WHERE `+pred, kind, kind).Scan(&ic.Count, &ic.Ignored); err != nil {
			return nil, err
		}
		samples, err := queryRows(ctx, c.db, func(rows *sql.Rows, s *IssueSample) error {
			return rows.Scan(&s.LibraryID, &s.Path, &s.Title)
		}, `SELECT b.library_id, b.rel_path, b.title FROM books b
		     WHERE `+pred+` AND NOT `+ignoredExpr+`
		     ORDER BY b.added_at DESC, b.id DESC LIMIT ?`, kind, maxIssueSamples)
		if err != nil {
			return nil, err
		}
		ic.Samples = samples
		out = append(out, ic)
	}
	return out, nil
}

// DuplicateGroup is a set of books in one library that look like the same book.
// Books[0] is the copy worth keeping (format, single file, bitrate, listeners).
type DuplicateGroup struct {
	// Reason is "same_files" when the copies' audio is byte-identical (fingerprint
	// and size), else "same_book" (identifiers, or author/title/narrator with a
	// matching length).
	Reason  string            `json:"reason"`
	Ignored bool              `json:"ignored"`
	Books   []DuplicateMember `json:"books"`
}

// DuplicateMember is one copy, with how many people have progress on it.
type DuplicateMember struct {
	AdminBook
	Listeners int `json:"listeners"`
}

// maxDuplicateGroups bounds one duplicates answer.
const maxDuplicateGroups = 500

// dupRow is what duplicate detection reads per book.
type dupRow struct {
	id, libraryID                 int64
	title, author, narrator, path string
	asin, isbn, hash, format      string
	duration                      float64
	size                          int64
	files, listeners              int
	ignored                       bool
}

// sameLength reports whether two durations could be one recording (within a
// minute or 2%, whichever is more). An unknown length (0, no ffprobe) matches.
func sameLength(a, b float64) bool {
	if a <= 0 || b <= 0 {
		return true
	}
	return math.Abs(a-b) <= math.Max(60, 0.02*math.Max(a, b))
}

// DuplicateGroups finds books that look like the same book within each library
// (copies in different libraries are deliberate, and players already show one), in
// one library (or all, libraryID 0). A group every member of which an admin
// ignored is left out unless withIgnored; a new copy brings it back.
func (c *Catalog) DuplicateGroups(ctx context.Context, libraryID int64, withIgnored bool) ([]DuplicateGroup, error) {
	rows, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *dupRow) error {
		return rows.Scan(&r.id, &r.libraryID, &r.path, &r.title, &r.author, &r.narrator, &r.asin,
			&r.isbn, &r.hash, &r.format, &r.duration, &r.size, &r.files, &r.listeners, &r.ignored)
	}, `SELECT b.id, b.library_id, b.rel_path, b.title, b.author, b.narrator, b.asin, b.isbn,
	           b.content_hash, b.format, b.duration, b.size, `+fileCountExpr+`,
	           (SELECT COUNT(*) FROM progress p WHERE p.library_id = b.library_id AND p.rel_path = b.rel_path),
	           `+strings.Replace(ignoredExpr, "?", "'"+IssueDuplicate+"'", 1)+`
	      FROM books b WHERE (?1 = 0 OR b.library_id = ?1)`, libraryID)
	if err != nil {
		return nil, err
	}

	parent := make([]int, len(rows))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	sameFiles := map[int]bool{} // roots of groups joined by identical audio
	union := func(a, b int, files bool) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
			if sameFiles[rb] {
				sameFiles[ra] = true
			}
		}
		if files {
			sameFiles[ra] = true
		}
	}
	// Exact keys (identical audio, identifiers) join outright; author|title|narrator
	// joins only copies of a matching length, so an abridged edition isn't a copy.
	exact := map[string]int{}
	byMeta := map[string][]int{}
	for i, r := range rows {
		lib := fmt.Sprint(r.libraryID) + ":"
		keys := []string{}
		if r.hash != "" {
			keys = append(keys, lib+"h:"+r.hash+":"+fmt.Sprint(r.size))
		}
		if v := norm(r.asin); v != "" {
			keys = append(keys, lib+"a:"+v)
		}
		if v := norm(r.isbn); v != "" {
			keys = append(keys, lib+"i:"+v)
		}
		for _, k := range keys {
			if j, ok := exact[k]; ok {
				union(j, i, strings.Contains(k, ":h:"))
			} else {
				exact[k] = i
			}
		}
		if m := metaKey(Book{Author: r.author, Title: r.title, Narrator: r.narrator}); m != "" {
			byMeta[lib+m] = append(byMeta[lib+m], i)
		}
	}
	for _, idx := range byMeta {
		for x := range idx {
			for y := x + 1; y < len(idx); y++ {
				if sameLength(rows[idx[x]].duration, rows[idx[y]].duration) {
					union(idx[x], idx[y], false)
				}
			}
		}
	}

	members := map[int][]int{}
	var roots []int
	for i := range rows {
		r := find(i)
		if _, seen := members[r]; !seen {
			roots = append(roots, r)
		}
		members[r] = append(members[r], i)
	}
	type group struct {
		reason  string
		ignored bool
		idx     []int
	}
	var groups []group
	for _, root := range roots {
		idx := members[root]
		if len(idx) < 2 {
			continue
		}
		g := group{reason: "same_book", ignored: true, idx: idx}
		if sameFiles[root] {
			g.reason = "same_files"
		}
		for _, i := range idx {
			g.ignored = g.ignored && rows[i].ignored
		}
		if g.ignored && !withIgnored {
			continue
		}
		sort.SliceStable(g.idx, func(a, b int) bool { return betterCopy(rows[g.idx[a]], rows[g.idx[b]]) })
		groups = append(groups, g)
	}
	// Newest-looking problems first is meaningless here; order by the kept copy's path.
	sort.Slice(groups, func(a, b int) bool {
		ra, rb := rows[groups[a].idx[0]], rows[groups[b].idx[0]]
		if ra.libraryID != rb.libraryID {
			return ra.libraryID < rb.libraryID
		}
		return lessFold(ra.path, rb.path)
	})
	if len(groups) > maxDuplicateGroups {
		groups = groups[:maxDuplicateGroups]
	}

	var ids []int64
	for _, g := range groups {
		for _, i := range g.idx {
			ids = append(ids, rows[i].id)
		}
	}
	books, err := c.adminBooksByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]DuplicateGroup, 0, len(groups))
	for _, g := range groups {
		dg := DuplicateGroup{Reason: g.reason, Ignored: g.ignored}
		for _, i := range g.idx {
			if b, ok := books[rows[i].id]; ok {
				dg.Books = append(dg.Books, DuplicateMember{AdminBook: b, Listeners: rows[i].listeners})
			}
		}
		if len(dg.Books) >= 2 {
			out = append(out, dg)
		}
	}
	return out, nil
}

// betterCopy reports whether a is the copy worth keeping over b: the better format,
// then a single file, then the higher bitrate, then the one people listen to.
func betterCopy(a, b dupRow) bool {
	if x, y := formatTier(a.format), formatTier(b.format); x != y {
		return x > y
	}
	if x, y := a.files <= 1, b.files <= 1; x != y {
		return x
	}
	if x, y := bitrate(Book{Size: a.size, Duration: a.duration}), bitrate(Book{Size: b.size, Duration: b.duration}); x != y {
		return x > y
	}
	return a.listeners > b.listeners
}

// adminBooksByID returns admin rows for the given book ids, by id.
func (c *Catalog) adminBooksByID(ctx context.Context, ids []int64) (map[int64]AdminBook, error) {
	out := map[int64]AdminBook{}
	const chunk = 500 // stay well under SQLite's bound-parameter limit
	for start := 0; start < len(ids); start += chunk {
		part := ids[start:min(start+chunk, len(ids))]
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		books, err := queryRows(ctx, c.db, func(rows *sql.Rows, b *AdminBook) error {
			return rows.Scan(adminBookDest(b)...)
		}, `SELECT `+adminBookCols+` FROM books b JOIN libraries l ON l.id = b.library_id
		     WHERE b.id IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for _, b := range books {
			out[b.id] = b
		}
	}
	return out, nil
}

// ListenersByLibrary counts, per library, the people with listening progress in it
// (what an offline library is keeping safe).
func (c *Catalog) ListenersByLibrary(ctx context.Context) (map[int64]int, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT library_id, COUNT(DISTINCT user_id) FROM progress GROUP BY library_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// maxIgnoreBatch bounds one ignore or un-ignore request.
const maxIgnoreBatch = 1000

// ErrTooMany marks a batch over its limit.
var ErrTooMany = errors.New("too many items")

// IgnoreIssue records that an admin ignored kind for each book (idempotent). The
// books needn't be indexed: the rows are path-keyed, like the index they outlive.
func (c *Catalog) IgnoreIssue(ctx context.Context, kind string, refs []Ref, userID int64) error {
	if err := checkIgnoreBatch(kind, refs); err != nil {
		return err
	}
	now := c.ts()
	return c.db.WithTx(ctx, "IgnoreIssue", func(tx *sql.Tx) error {
		for _, r := range refs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO issue_ignores(library_id, path, kind, created_by, created_at)
				 SELECT ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM libraries WHERE id = ?)
				 ON CONFLICT DO NOTHING`,
				r.LibraryID, r.Path, kind, userID, now, r.LibraryID); err != nil {
				return err
			}
		}
		return nil
	})
}

// UnignoreIssue removes the admin's ignore of kind for each book.
func (c *Catalog) UnignoreIssue(ctx context.Context, kind string, refs []Ref) error {
	if err := checkIgnoreBatch(kind, refs); err != nil {
		return err
	}
	return c.db.WithTx(ctx, "UnignoreIssue", func(tx *sql.Tx) error {
		for _, r := range refs {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM issue_ignores WHERE library_id = ? AND path = ? AND kind = ?`,
				r.LibraryID, r.Path, kind); err != nil {
				return err
			}
		}
		return nil
	})
}

func checkIgnoreBatch(kind string, refs []Ref) error {
	if !ValidIssue(kind) {
		return fmt.Errorf("%w: %q", ErrUnknownIssue, kind)
	}
	if len(refs) > maxIgnoreBatch {
		return fmt.Errorf("%w: at most %d books", ErrTooMany, maxIgnoreBatch)
	}
	return nil
}

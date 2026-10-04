package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
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
	// A folder an admin set to "one book" (the category's own fix) is settled.
	IssueSuspect: `(COALESCE(b.suspect_parts, 0) >= 2 AND NOT EXISTS(SELECT 1 FROM folder_overrides fo
		WHERE fo.library_id = b.library_id AND fo.path = b.rel_path AND fo.mode = '` + OverrideBook + `'))`,
	IssueScanError: `(b.scan_error <> '')`,
}

// ignoredExpr is "an admin ignored this book for the kind bound to its parameter".
const ignoredExpr = `EXISTS(SELECT 1 FROM issue_ignores ii
	WHERE ii.library_id = b.library_id AND ii.path = b.rel_path AND ii.kind = ?)`

// ValidIssue reports whether kind is one of IssueKinds.
func ValidIssue(kind string) bool { return slices.Contains(IssueKinds, kind) }

// ValidBookIssue reports whether kind lists books (every kind but duplicates,
// which come as groups): what GET /admin/books?issue= accepts.
func ValidBookIssue(kind string) bool {
	_, ok := issuePredicates[kind]
	return ok
}

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

// IssueCounts computes each of kinds, in that order. Every book kind is counted
// in one pass over the books, with the ignores pivoted once; samples are only
// looked up for a kind that has any.
func (c *Catalog) IssueCounts(ctx context.Context, kinds []string) ([]IssueCount, error) {
	var bookKinds []string
	for _, k := range kinds {
		if k != IssueDuplicate && !ValidBookIssue(k) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownIssue, k)
		}
		if k != IssueDuplicate {
			bookKinds = append(bookKinds, k)
		}
	}
	counts := map[string]*IssueCount{}
	if len(bookKinds) > 0 {
		// The kinds are this file's constants, never request input, so they can name
		// columns.
		var cols, flags []string
		dest := make([]any, 0, 2*len(bookKinds))
		for _, k := range bookKinds {
			ic := &IssueCount{Kind: k, Samples: []IssueSample{}}
			counts[k] = ic
			pred := issuePredicates[k]
			cols = append(cols,
				`COALESCE(SUM(CASE WHEN `+pred+` AND ig.`+k+` IS NOT 1 THEN 1 ELSE 0 END), 0)`,
				`COALESCE(SUM(CASE WHEN `+pred+` AND ig.`+k+` IS 1 THEN 1 ELSE 0 END), 0)`)
			flags = append(flags, `MAX(kind = '`+k+`') AS `+k)
			dest = append(dest, &ic.Count, &ic.Ignored)
		}
		q := `SELECT ` + strings.Join(cols, ", ") + ` FROM books b
			LEFT JOIN (SELECT library_id, path, ` + strings.Join(flags, ", ") + `
			             FROM issue_ignores GROUP BY library_id, path) ig
			       ON ig.library_id = b.library_id AND ig.path = b.rel_path`
		if err := c.db.QueryRowContext(ctx, q).Scan(dest...); err != nil {
			return nil, err
		}
		for _, k := range bookKinds {
			if counts[k].Count == 0 {
				continue
			}
			samples, err := queryRows(ctx, c.db, func(rows *sql.Rows, s *IssueSample) error {
				return rows.Scan(&s.LibraryID, &s.Path, &s.Title)
			}, `SELECT b.library_id, b.rel_path, b.title FROM books b
			     WHERE `+issuePredicates[k]+` AND NOT `+ignoredExpr+`
			     ORDER BY b.added_at DESC, b.id DESC LIMIT ?`, k, maxIssueSamples)
			if err != nil {
				return nil, err
			}
			counts[k].Samples = samples
		}
	}
	out := make([]IssueCount, 0, len(kinds))
	for _, k := range kinds {
		if k != IssueDuplicate {
			out = append(out, *counts[k])
			continue
		}
		ic := IssueCount{Kind: k, Samples: []IssueSample{}}
		rows, sets, err := c.duplicateSets(ctx, 0)
		if err != nil {
			return nil, err
		}
		for _, set := range sets {
			if set.ignored {
				ic.Ignored++
				continue
			}
			ic.Count++
			if len(ic.Samples) < maxIssueSamples {
				r := rows[set.members[0]]
				ic.Samples = append(ic.Samples, IssueSample{LibraryID: r.libraryID, Path: r.path, Title: r.title})
			}
		}
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

// maxDuplicateGroups bounds one duplicates answer (the count is not bounded).
const maxDuplicateGroups = 500

// dupRow is what duplicate detection reads per book. Only the fields the grouping
// and the ranking need, since it is read for every book of the library.
type dupRow struct {
	id, libraryID                      int64
	path, title, format                string
	author, narrator, asin, isbn, hash string // identity: cleared once grouped
	duration                           float64
	size                               int64
	files, listeners                   int
	ignored                            bool
}

// quality is the row as the Book betterQuality and identitySignals read.
func (r *dupRow) quality() Book {
	return Book{Title: r.title, Author: r.author, Narrator: r.narrator, ASIN: r.asin, ISBN: r.isbn,
		ContentHash: r.hash, Format: r.format, Duration: r.duration, Size: r.size}
}

// dupSet is one group of copies: indexes into the rows, best copy first.
type dupSet struct {
	members   []int
	sameFiles bool
	ignored   bool // every member ignored as a duplicate
}

// sameLength reports whether two durations could be one recording (within a
// minute or 2%, whichever is more). Unknown lengths (0, no ffprobe) match each
// other but not a known one: a failed probe mustn't join an abridged edition to an
// unabridged one through itself.
func sameLength(a, b float64) bool {
	if a <= 0 || b <= 0 {
		return a <= 0 && b <= 0
	}
	return math.Abs(a-b) <= math.Max(60, 0.02*math.Max(a, b))
}

// duplicateSets finds the groups of copies within each library (of one library,
// or all with libraryID 0): books sharing an identity signal (identitySignals),
// except that audio must also match in size (an identical first part is not a
// copy) and author/title/narrator only joins copies of a matching length (an
// abridged edition is not a copy). Copies in different libraries are deliberate,
// and players already show one, so they never group.
func (c *Catalog) duplicateSets(ctx context.Context, libraryID int64) ([]dupRow, []dupSet, error) {
	rows, err := queryRows(ctx, c.db, func(rows *sql.Rows, r *dupRow) error {
		return rows.Scan(&r.id, &r.libraryID, &r.path, &r.title, &r.author, &r.narrator, &r.asin,
			&r.isbn, &r.hash, &r.format, &r.duration, &r.size, &r.files, &r.listeners, &r.ignored)
	}, `SELECT b.id, b.library_id, b.rel_path, b.title, b.author, b.narrator, b.asin, b.isbn,
	           b.content_hash, b.format, b.duration, b.size, MAX(1, COALESCE(bf.n, 0)),
	           COALESCE(p.n, 0), ii.path IS NOT NULL
	      FROM books b
	      LEFT JOIN (SELECT book_id, COUNT(*) AS n FROM book_files GROUP BY book_id) bf ON bf.book_id = b.id
	      LEFT JOIN (SELECT library_id, rel_path, COUNT(*) AS n FROM progress GROUP BY library_id, rel_path) p
	             ON p.library_id = b.library_id AND p.rel_path = b.rel_path
	      LEFT JOIN issue_ignores ii ON ii.library_id = b.library_id AND ii.path = b.rel_path AND ii.kind = ?2
	     WHERE (?1 = 0 OR b.library_id = ?1)`, libraryID, IssueDuplicate)
	if err != nil {
		return nil, nil, err
	}
	set := newDisjointSet(len(rows))
	viaFiles := make([]bool, len(rows)) // joined to another copy by identical audio
	exact := map[string]int{}
	byMeta := map[string][]int{}
	for i := range rows {
		r := &rows[i]
		lib := strconv.FormatInt(r.libraryID, 10) + ":"
		for _, sig := range identitySignals(r.quality()) {
			files := strings.HasPrefix(sig, "h:")
			switch {
			case strings.HasPrefix(sig, "m:"):
				byMeta[lib+sig] = append(byMeta[lib+sig], i)
				continue
			case files:
				sig += ":" + strconv.FormatInt(r.size, 10)
			}
			if j, ok := exact[lib+sig]; ok {
				set.union(i, j)
				viaFiles[i], viaFiles[j] = viaFiles[i] || files, viaFiles[j] || files
			} else {
				exact[lib+sig] = i
			}
		}
		r.author, r.narrator, r.asin, r.isbn, r.hash = "", "", "", "", ""
	}
	for _, idx := range byMeta {
		for x := range idx {
			for _, y := range idx[x+1:] {
				if sameLength(rows[idx[x]].duration, rows[y].duration) {
					set.union(idx[x], y)
				}
			}
		}
	}
	var sets []dupSet
	for _, members := range set.groups() {
		if len(members) < 2 {
			continue
		}
		ds := dupSet{members: members, ignored: true}
		for _, i := range members {
			ds.sameFiles = ds.sameFiles || viaFiles[i]
			ds.ignored = ds.ignored && rows[i].ignored
		}
		sort.SliceStable(ds.members, func(a, b int) bool { return betterCopy(rows[ds.members[a]], rows[ds.members[b]]) })
		sets = append(sets, ds)
	}
	// In a stable order: by library, then the kept copy's path.
	sort.Slice(sets, func(a, b int) bool {
		ra, rb := rows[sets[a].members[0]], rows[sets[b].members[0]]
		if ra.libraryID != rb.libraryID {
			return ra.libraryID < rb.libraryID
		}
		return lessFold(ra.path, rb.path)
	})
	return rows, sets, nil
}

// betterCopy reports whether a is the copy worth keeping over b: betterQuality,
// then the one people listen to.
func betterCopy(a, b dupRow) bool {
	if better, ok := betterQuality(a.quality(), a.files, b.quality(), b.files); ok {
		return better
	}
	return a.listeners > b.listeners
}

// DuplicateGroups returns the groups of copies (duplicateSets) with each copy's
// admin row, at most maxDuplicateGroups: the open ones, or with ignored only the
// ones every member of which an admin ignored (a new copy brings a group back).
func (c *Catalog) DuplicateGroups(ctx context.Context, libraryID int64, ignored bool) ([]DuplicateGroup, error) {
	rows, sets, err := c.duplicateSets(ctx, libraryID)
	if err != nil {
		return nil, err
	}
	// Filtered before the cap, so every ignored group can be reached to show again.
	sets = slices.DeleteFunc(sets, func(s dupSet) bool { return s.ignored != ignored })
	if len(sets) > maxDuplicateGroups {
		sets = sets[:maxDuplicateGroups]
	}
	var ids []int64
	for _, s := range sets {
		for _, i := range s.members {
			ids = append(ids, rows[i].id)
		}
	}
	books, err := c.adminBooksByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]DuplicateGroup, 0, len(sets))
	for _, s := range sets {
		g := DuplicateGroup{Reason: "same_book", Ignored: s.ignored}
		if s.sameFiles {
			g.Reason = "same_files"
		}
		for _, i := range s.members {
			if b, ok := books[rows[i].id]; ok {
				g.Books = append(g.Books, DuplicateMember{AdminBook: b, Listeners: rows[i].listeners})
			}
		}
		if len(g.Books) >= 2 {
			out = append(out, g)
		}
	}
	return out, nil
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

// libraryCount is one library's number in a per-library tally.
type libraryCount struct {
	id int64
	n  int
}

// ListenersByLibrary counts, per library, the people with listening progress in it
// (what an offline library is keeping safe).
func (c *Catalog) ListenersByLibrary(ctx context.Context) (map[int64]int, error) {
	rows, err := queryRows(ctx, c.db, func(rows *sql.Rows, lc *libraryCount) error {
		return rows.Scan(&lc.id, &lc.n)
	}, `SELECT library_id, COUNT(DISTINCT user_id) FROM progress GROUP BY library_id`)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int, len(rows))
	for _, r := range rows {
		out[r.id] = r.n
	}
	return out, nil
}

// IgnoreIssue records that an admin ignored kind for each book (idempotent). The
// books needn't be indexed: the rows are path-keyed, like the index they outlive.
func (c *Catalog) IgnoreIssue(ctx context.Context, kind string, refs []Ref, userID int64) error {
	if !ValidIssue(kind) {
		return fmt.Errorf("%w: %q", ErrUnknownIssue, kind)
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
	if !ValidIssue(kind) {
		return fmt.Errorf("%w: %q", ErrUnknownIssue, kind)
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

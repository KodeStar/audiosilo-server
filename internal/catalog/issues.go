package catalog

import (
	"cmp"
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
	// IssueDetailedChapters: the book's own chapters are coarse, and the
	// community's fit with more of them (a refine the admin hasn't decided on).
	IssueDetailedChapters = "detailed_chapters"
	IssueUnmatched        = "unmatched"
	IssueTranscode        = "transcode"
	IssueSuspect          = "suspect"
	IssueSplitDiscs       = "split_discs"
	IssueScanError        = "scan_error"
	IssueDuplicate        = "duplicate"
	// IssueSeriesSpellings: the book's series is spelled another way by other
	// books of its library (seriesspellings.go).
	IssueSeriesSpellings = "series_spellings"
)

// IssueKinds are the kinds an issue can be ignored for, in the Health page's order.
var IssueKinds = []string{IssueScanError, IssueSuspect, IssueSplitDiscs, IssueDuplicate, IssueNoCover,
	IssueUnmatched, IssueSeriesSpellings, IssueNoChapters, IssueDetailedChapters, IssueTranscode}

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
	// A current check only (a stale one is never used, so choosing it would change
	// nothing); settled by picking either source (a chapter_choices row).
	IssueDetailedChapters: `(EXISTS(SELECT 1 FROM community_chapters cc
		WHERE cc.library_id = b.library_id AND cc.path = b.rel_path AND cc.status = 'refine'
		  AND cc.basis = ` + chapterBasisExpr + `)
		AND NOT EXISTS(SELECT 1 FROM chapter_choices ch WHERE ch.library_id = b.library_id AND ch.path = b.rel_path))`,
	IssueUnmatched: `(NOT ` + matchedExpr + `)`,
	IssueTranscode: `(NOT ` + directPlayableExpr + `)`,
	// A folder an admin set to "one book" (the category's own fix) is settled.
	IssueSuspect: `(COALESCE(b.suspect_parts, 0) >= 2 AND NOT EXISTS(SELECT 1 FROM folder_overrides fo
		WHERE fo.library_id = b.library_id AND fo.path = b.rel_path AND fo.mode = '` + OverrideBook + `'))`,
	IssueScanError:       `(b.scan_error <> '')`,
	IssueSeriesSpellings: seriesSpellingsExpr,
	// One book split across disc folders, listed once: by its first disc.
	IssueSplitDiscs: `(` + splitDiscExpr + ` AND NOT EXISTS(SELECT 1 FROM books s
		WHERE s.library_id = b.library_id AND s.split_parent <> '' AND s.split_parent = b.split_parent
		  AND s.rel_path < b.rel_path))`,
}

// splitDiscExpr is "b is one disc of a book split across disc folders": a CD rip
// ("Book/CD1", "Book/CD2", the tracks in each, none in "Book") reads as one book per
// disc until an admin sets the folder holding them to "Always one book" (a `book`
// override, which joins them; library.joinedBook). The scan tells such a disc
// (books.split_parent, the folder holding it); any override on that folder settles
// it (one sets how it reads: joined, or deliberately not).
const splitDiscExpr = `(b.split_parent <> '' AND NOT EXISTS(SELECT 1 FROM folder_overrides fo
		WHERE fo.library_id = b.library_id AND fo.path = b.split_parent))`

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
				// The ignore first: a costly predicate (series_spellings' seeks) then
				// runs once per book, not twice.
				`COALESCE(SUM(CASE WHEN ig.`+k+` IS NOT 1 AND `+pred+` THEN 1 ELSE 0 END), 0)`,
				`COALESCE(SUM(CASE WHEN ig.`+k+` IS 1 AND `+pred+` THEN 1 ELSE 0 END), 0)`)
			flags = append(flags, `MAX(kind = '`+k+`') AS `+k)
			dest = append(dest, &ic.Count, &ic.Ignored)
		}
		q := `SELECT ` + strings.Join(cols, ", ") + ` FROM books b
			LEFT JOIN (SELECT library_id, path, ` + strings.Join(flags, ", ") + `
			             FROM issue_ignores GROUP BY library_id, path) ig
			       ON ig.library_id = b.library_id AND ig.path = b.rel_path WHERE ` + inBookLibrary
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
			     WHERE `+inBookLibrary+` AND `+issuePredicates[k]+` AND NOT `+ignoredExpr+`
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
		rows, sets, err := c.duplicateSets(ctx, 0, false)
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

// DuplicateGroup is a set of books that look like the same book: in one library,
// or with across (?across=1) in any (each copy's AdminBook names its library).
// Books[0] is the copy worth keeping (format, single file, bitrate, listeners),
// except under dupIDLengthDiffers, where the order says nothing about which to keep.
type DuplicateGroup struct {
	// Reason is the strongest claim the group's joins support (dupSet.reason): one
	// of dupIDLengthDiffers, dupSameFiles, dupSameID or dupSameBook.
	Reason  string            `json:"reason"`
	Ignored bool              `json:"ignored"`
	Books   []DuplicateMember `json:"books"`
}

// DuplicateMember is one copy, with how many people have progress on it.
type DuplicateMember struct {
	AdminBook
	Listeners int `json:"listeners"`
}

// maxDuplicateGroups bounds one duplicates page, and is the size of a page asked
// for without a limit (the count is not bounded).
const maxDuplicateGroups = 500

// maxDuplicatePageBooks bounds the copies on one duplicates page: a page ends
// early, before the group that would take it past this, once it holds one group,
// so a few very large groups can't make a page megabytes (each copy is an admin
// row). A single group larger than this is still one page.
const maxDuplicatePageBooks = 300

// DuplicateQuery is what GET /admin/issues/duplicates asks: the groups of one
// library (0 = all), the open or only the ignored ones, across libraries or not,
// a page of at most Limit groups (0 or more than maxDuplicateGroups =
// maxDuplicateGroups) after Cursor (a NextCursor; "" = the first page).
type DuplicateQuery struct {
	LibraryID       int64
	Ignored, Across bool
	Limit           int
	Cursor          string
}

// DuplicatePage is one page of duplicate groups, in the groups' stable order (by
// library, then the kept copy's path). Total is how many groups there are in all
// (every page's); NextCursor is "" on the last page.
type DuplicatePage struct {
	Groups     []DuplicateGroup `json:"groups"`
	Total      int              `json:"total"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

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

// The reasons a duplicate group gives (DuplicateGroup.Reason), from the joins
// that formed it (dupJoin).
const (
	// dupSameFiles: byte-identical audio (fingerprint and size).
	dupSameFiles = "same_files"
	// dupSameID: a shared ASIN or ISBN whose holders' known lengths match
	// (sameLength; an unknown length is no evidence they differ).
	dupSameID = "same_id"
	// dupIDLengthDiffers: a shared ASIN or ISBN of lengths that don't match: not
	// plain copies (one may be matched to the wrong recording, or is incomplete).
	dupIDLengthDiffers = "id_length_differs"
	// dupSameBook: author/title/narrator (metaKey) of a matching length.
	dupSameBook = "same_book"
)

// dupJoin is a bit set of the kinds of join that put a copy in its group, marked on
// one copy of each join, so a group's joins are its members' bits ORed together. A
// join by author/title/narrator marks nothing: it is the reason when no other is.
type dupJoin uint8

const (
	joinFiles dupJoin = 1 << iota // identical audio: hash and size
	joinID                        // a shared ASIN/ISBN, whatever the lengths
)

// dupSet is one group of copies: indexes into the rows, best copy first.
type dupSet struct {
	members []int
	joins   dupJoin
	// lengthsDiffer: the group's shortest and longest known lengths are not
	// sameLength (so some two are not); an unknown length (no ffprobe) is no
	// evidence either way.
	lengthsDiffer bool
	ignored       bool // every member ignored as a duplicate
}

// reason is the group's DuplicateGroup.Reason. A group a shared identifier joined
// whose lengths differ wins, even over identical audio elsewhere in the group: it
// then holds a different recording, and naming it a copy would invite deleting it.
// The lengths are the whole group's, not one identifier's holders', so a book of
// unknown length sharing one identifier with each of two different recordings
// can't make them look like copies. Then identical audio, a shared identifier,
// and author/title/narrator, strongest evidence first.
func (s dupSet) reason() string {
	switch {
	case s.joins&joinID != 0 && s.lengthsDiffer:
		return dupIDLengthDiffers
	case s.joins&joinFiles != 0:
		return dupSameFiles
	case s.joins&joinID != 0:
		return dupSameID
	default:
		return dupSameBook
	}
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
// abridged edition is not a copy). A shared ASIN or ISBN joins whatever the
// lengths, and the group's reason says when any two of its holders differ
// (dupSet.reason). Copies in different libraries are usually deliberate, and
// players already show one, so they group only with across, which an admin asks
// for (#165): then the signals are not kept per library, and with a libraryID
// only the groups holding one of its books are kept (the rows are read from
// every library). The Health count never asks for across. The discs of a book
// split across disc folders look alike (one title, similar lengths) but are
// listed under their own issue, whose fix joins them, so they never group either.
func (c *Catalog) duplicateSets(ctx context.Context, libraryID int64, across bool) ([]dupRow, []dupSet, error) {
	readLib := libraryID
	if across {
		readLib = 0
	}
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
	     WHERE `+inBookLibrary+` AND (?1 = 0 OR b.library_id = ?1) AND NOT `+splitDiscExpr, readLib, IssueDuplicate)
	if err != nil {
		return nil, nil, err
	}
	set := newDisjointSet(len(rows))
	joins := make([]dupJoin, len(rows))
	files := map[string]int{}    // the first book with this audio
	byID := map[string]int{}     // the first book with this ASIN/ISBN
	byMeta := map[string][]int{} // every book with this author/title/narrator
	for i := range rows {
		r := &rows[i]
		lib := strconv.FormatInt(r.libraryID, 10) + ":"
		if across {
			lib = ""
		}
		for _, sig := range identitySignals(r.quality()) {
			switch {
			case strings.HasPrefix(sig, "m:"):
				byMeta[lib+sig] = append(byMeta[lib+sig], i)
			case strings.HasPrefix(sig, "h:"):
				key := lib + sig + ":" + strconv.FormatInt(r.size, 10)
				if j, ok := files[key]; ok {
					set.union(i, j)
					joins[i] |= joinFiles
				} else {
					files[key] = i
				}
			default: // "a:" or "i:"
				if j, ok := byID[lib+sig]; ok {
					set.union(i, j)
					joins[i] |= joinID
				} else {
					byID[lib+sig] = i
				}
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
		if across && libraryID != 0 && !slices.ContainsFunc(members, func(i int) bool { return rows[i].libraryID == libraryID }) {
			continue
		}
		ds := dupSet{members: members, ignored: true}
		shortest, longest := 0.0, 0.0 // known lengths; 0 = none known
		for _, i := range members {
			ds.joins |= joins[i]
			ds.ignored = ds.ignored && rows[i].ignored
			if d := rows[i].duration; d > 0 {
				if longest == 0 {
					shortest = d
				}
				shortest, longest = min(shortest, d), max(longest, d)
			}
		}
		ds.lengthsDiffer = longest > 0 && !sameLength(shortest, longest)
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

// DuplicateGroups returns the first maxDuplicateGroups groups of copies
// (DuplicatePage without a cursor).
func (c *Catalog) DuplicateGroups(ctx context.Context, libraryID int64, ignored, across bool) ([]DuplicateGroup, error) {
	page, err := c.DuplicatePage(ctx, DuplicateQuery{LibraryID: libraryID, Ignored: ignored, Across: across})
	return page.Groups, err
}

// DuplicatePage returns a page of the groups of copies (duplicateSets) with each
// copy's admin row: the open ones, or with q.Ignored only the ones every member
// of which an admin ignored (a new copy brings a group back). q.Across also groups
// copies in different libraries (duplicateSets). The groups are found afresh for
// every page (it reads the library's rows, not the admin rows, which only the
// page's copies need) and paged by keyset on their order, the kept copy's library
// and path: a cursor names the last group's, so a group that comes or goes
// between pages moves nothing else. ErrInvalidCursor for a cursor that isn't one.
func (c *Catalog) DuplicatePage(ctx context.Context, q DuplicateQuery) (DuplicatePage, error) {
	limit := q.Limit
	if limit <= 0 || limit > maxDuplicateGroups {
		limit = maxDuplicateGroups
	}
	var afterLib int64
	var afterPath string
	if q.Cursor != "" {
		var err error
		if afterPath, afterLib, err = decodeCursor(q.Cursor); err != nil {
			return DuplicatePage{}, ErrInvalidCursor
		}
	}
	rows, sets, err := c.duplicateSets(ctx, q.LibraryID, q.Across)
	if err != nil {
		return DuplicatePage{}, err
	}
	// Filtered before paging, so every ignored group can be reached to show again.
	sets = slices.DeleteFunc(sets, func(s dupSet) bool { return s.ignored != q.Ignored })
	page := DuplicatePage{Total: len(sets), Groups: []DuplicateGroup{}}
	if q.Cursor != "" {
		// The first group past the cursor, in duplicateSets' order.
		start, _ := slices.BinarySearchFunc(sets, 0, func(s dupSet, _ int) int {
			keep := rows[s.members[0]]
			if d := cmp.Compare(keep.libraryID, afterLib); d != 0 {
				return d
			}
			if compareFold(keep.path, afterPath) <= 0 {
				return -1
			}
			return 1
		})
		sets = sets[start:]
	}
	end, books := 0, 0
	for end < len(sets) && end < limit {
		n := len(sets[end].members)
		if end > 0 && books+n > maxDuplicatePageBooks {
			break
		}
		books += n
		end++
	}
	if end < len(sets) {
		last := rows[sets[end-1].members[0]]
		page.NextCursor = encodeCursor(last.path, last.libraryID)
	}
	sets = sets[:end]
	ids := make([]int64, 0, books)
	for _, s := range sets {
		for _, i := range s.members {
			ids = append(ids, rows[i].id)
		}
	}
	byID, err := c.adminBooksByID(ctx, ids)
	if err != nil {
		return DuplicatePage{}, err
	}
	for _, s := range sets {
		g := DuplicateGroup{Reason: s.reason(), Ignored: s.ignored}
		for _, i := range s.members {
			if b, ok := byID[rows[i].id]; ok {
				g.Books = append(g.Books, DuplicateMember{AdminBook: b, Listeners: rows[i].listeners})
			}
		}
		if len(g.Books) >= 2 {
			page.Groups = append(page.Groups, g)
		}
	}
	return page, nil
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
		books, err := queryRows(ctx, c.db, scanAdminBook, `SELECT `+adminBookCols+` FROM books b JOIN libraries l ON l.id = b.library_id
		     WHERE l.kind='books' AND b.id IN (`+placeholders(len(part))+`)`, args...)
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

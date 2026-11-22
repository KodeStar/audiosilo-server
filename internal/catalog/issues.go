package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path"
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
	// IssueUnknownASIN: the book's own ASIN is one the community database doesn't
	// know (as the unknown-ASIN report last found it), so it counts as matched
	// but gets no community details (server#370).
	IssueUnknownASIN = "unknown_asin"
	IssueTranscode   = "transcode"
	IssueSuspect     = "suspect"
	IssueSplitDiscs  = "split_discs"
	IssueScanError   = "scan_error"
	IssueDuplicate   = "duplicate"
	// IssueSeriesSpellings: the book's series is spelled another way by other
	// books of its library (seriesspellings.go).
	IssueSeriesSpellings = "series_spellings"
)

// IssueKinds are the kinds an issue can be ignored for, in the Health page's order.
var IssueKinds = []string{IssueScanError, IssueSuspect, IssueSplitDiscs, IssueDuplicate, IssueNoCover,
	IssueUnmatched, IssueUnknownASIN, IssueSeriesSpellings, IssueNoChapters, IssueDetailedChapters, IssueTranscode}

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
	// nothing); settled by picking either source (a chapter_choices row) or by
	// editing the chapters (a chapter_edits row, applied or dormant).
	IssueDetailedChapters: `(EXISTS(SELECT 1 FROM community_chapters cc
		WHERE cc.library_id = b.library_id AND cc.path = b.rel_path AND cc.status = 'refine'
		  AND cc.basis = ` + chapterBasisExpr + `)
		AND NOT EXISTS(SELECT 1 FROM chapter_choices ch WHERE ch.library_id = b.library_id AND ch.path = b.rel_path)
		AND NOT EXISTS(SELECT 1 FROM chapter_edits ce WHERE ce.library_id = b.library_id AND ce.path = b.rel_path))`,
	IssueUnmatched:   `(NOT ` + matchedExpr + `)`,
	IssueUnknownASIN: unknownASINExpr,
	IssueTranscode:   `(NOT ` + directPlayableExpr + `)`,
	// A folder an admin set to "one book" (the category's own fix) is settled.
	IssueSuspect: `(COALESCE(b.suspect_parts, 0) >= 2 AND NOT EXISTS(SELECT 1 FROM folder_overrides fo
		WHERE fo.library_id = b.library_id AND fo.path = b.rel_path AND fo.mode = '` + OverrideBook + `'))`,
	IssueScanError:       `(b.scan_error <> '')`,
	IssueSeriesSpellings: seriesSpellingsExpr,
	// One book split across disc folders, listed once: by its first disc.
	IssueSplitDiscs: `(` + splitDiscExpr + ` AND NOT EXISTS(SELECT 1 FROM books s
		WHERE s.library_id = b.library_id AND s.split_parent <> '' AND s.split_parent = b.split_parent
		  AND disc_before(s.rel_path, b.rel_path)))`,
}

// unknownASINExpr is "b has an ASIN of its own that the community database
// doesn't know": the opt-in unknown-ASIN report (internal/coverage, 0068)
// looked it up and the answer was a 404 (unknown_asin_reports.unknown, keyed by
// the upper-cased ASIN, match.NormalizeASIN). Such a book counts as matched
// (matchedExpr) and Match automatically skips it, yet /meta has nothing for it:
// a lookup asks by the ASIN alone when the book has one, so an ISBN beside it
// is never asked (meta.Service.Enrich), and a book with both is listed too
// (server#392). Only a book with no recorded community match (book_matches:
// /meta answers by that) and an ASIN that isn't the community's own (a match
// gave it: book_overrides of source community), so matching it by hand settles
// it. The table is filled only while the report runs, so the Health summary
// lists the kind only then (api.handleIssues). The ASIN is compared as
// match.NormalizeASIN (normASINSQL) keys the table.
var unknownASINExpr = `(` + normASINSQL("b.asin") + ` <> ''
	AND ` + asinHeldUnknownExpr + `
	AND NOT EXISTS(SELECT 1 FROM book_matches m WHERE m.library_id = b.library_id AND m.path = b.rel_path)
	AND NOT EXISTS(SELECT 1 FROM book_overrides o WHERE o.library_id = b.library_id AND o.path = b.rel_path
		AND o.field = '` + FieldASIN + `' AND o.source = '` + SourceCommunity + `'))`

// asinHeldUnknownExpr is "the unknown-ASIN report holds b's ASIN as unknown":
// its last lookup of it was a 404, and no lookup has found it since
// (ClearUnknownASIN, the report's own recheck).
var asinHeldUnknownExpr = `EXISTS(SELECT 1 FROM unknown_asin_reports u WHERE u.asin = ` + normASINSQL("b.asin") + ` AND u.unknown = 1)`

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

// Older unplaceable disc rows retain their original path. The parent decision
// covers every representative of the same set.
const discIgnoredExpr = `(b.split_parent <> '' AND (EXISTS (SELECT 1 FROM issue_ignores ii WHERE ii.library_id = b.library_id
	AND ii.path = b.split_parent AND ii.kind = 'split_disc_set') OR EXISTS (
	SELECT 1 FROM books s JOIN issue_ignores ii ON ii.library_id = s.library_id AND ii.path = s.rel_path
	WHERE s.library_id = b.library_id AND s.split_parent = b.split_parent AND ii.kind = 'split_discs')))`

func bookIgnoredExpr(kind string) (string, []any) {
	if kind == IssueSplitDiscs {
		return discIgnoredExpr, nil
	}
	return ignoredExpr, []any{kind}
}

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

// issueCountsSQL is the one pass over the books counting each of kinds (book
// kinds, never duplicates): two columns a kind, its count and its ignored, with
// the ignores pivoted once. The kinds are this file's constants, never request
// input, so they can name columns.
func issueCountsSQL(kinds []string) string {
	var cols, flags []string
	for _, k := range kinds {
		pred := issuePredicates[k]
		ignored := "ig." + k
		if k == IssueSplitDiscs {
			ignored = discIgnoredExpr
		}
		cols = append(cols,
			// The ignore first: a costly predicate (series_spellings' seeks) then
			// runs once per book, not twice.
			`COALESCE(SUM(CASE WHEN `+ignored+` IS NOT 1 AND `+pred+` THEN 1 ELSE 0 END), 0)`,
			`COALESCE(SUM(CASE WHEN `+ignored+` IS 1 AND `+pred+` THEN 1 ELSE 0 END), 0)`)
		flags = append(flags, `MAX(kind = '`+k+`') AS `+k)
	}
	return `SELECT ` + strings.Join(cols, ", ") + ` FROM books b
			LEFT JOIN (SELECT library_id, path, ` + strings.Join(flags, ", ") + `
			             FROM issue_ignores GROUP BY library_id, path) ig
			       ON ig.library_id = b.library_id AND ig.path = b.rel_path WHERE ` + inBookLibrary
}

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
		dest := make([]any, 0, 2*len(bookKinds))
		for _, k := range bookKinds {
			ic := &IssueCount{Kind: k, Samples: []IssueSample{}}
			counts[k] = ic
			dest = append(dest, &ic.Count, &ic.Ignored)
		}
		if err := c.db.QueryRowContext(ctx, issueCountsSQL(bookKinds)).Scan(dest...); err != nil {
			return nil, err
		}
		for _, k := range bookKinds {
			if counts[k].Count == 0 {
				continue
			}
			ignored, args := bookIgnoredExpr(k)
			samples, err := queryRows(ctx, c.db, func(rows *sql.Rows, s *IssueSample) error {
				return rows.Scan(&s.LibraryID, &s.Path, &s.Title)
			}, `SELECT b.library_id, b.rel_path, b.title FROM books b
			     WHERE `+inBookLibrary+` AND `+issuePredicates[k]+` AND NOT `+ignored+`
			     ORDER BY b.added_at DESC, b.id DESC LIMIT ?`, append(args, maxIssueSamples)...)
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
	Key     string            `json:"key"` // digest of the exact library/path membership
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

// maxDuplicatePageBooks bounds the copies on one duplicates page asked for with
// a limit: it ends early, before the group that would take it past this, once
// it holds one group, so a few very large groups can't make a page megabytes
// (each copy is an admin row). A single group larger than this is still one
// page. A request without a limit (a caller that doesn't page) gets its 500.
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
	// legacyIgnore is the book's pre-0092 duplicate flag: "duplicate" while
	// still unmapped, "duplicate_mapped" once MapLegacyDuplicateIgnores placed
	// it, "" for none. MIN prefers the unmapped one when a book has both.
	legacyIgnore string
}

// ref is the row's book as a decision names it.
func (r *dupRow) ref() Ref { return Ref{r.libraryID, r.path} }

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
// join by author/title/narrator marks nothing: it is the reason when no other is,
// and identical audio is read off the members (dupSet.sameFiles).
type dupJoin uint8

const (
	joinID dupJoin = 1 << iota // a shared ASIN/ISBN, whatever the lengths
)

// dupSet is one group of copies: indexes into the rows, best copy first.
type dupSet struct {
	members []int
	joins   dupJoin
	// sameFiles: every member holds the same audio (fingerprint and size), so
	// the group is byte-identical copies, not just some of them (ADMIN-04).
	sameFiles bool
	// lengthsDiffer: the group's shortest and longest known lengths are not
	// sameLength (so some two are not); an unknown length (no ffprobe) is no
	// evidence either way.
	lengthsDiffer bool
	ignored       bool
	key           string // comparison identity, independent of ranking
}

// reason is the group's DuplicateGroup.Reason. A group a shared identifier joined
// whose lengths differ wins, even over identical audio elsewhere in the group: it
// then holds a different recording, and naming it a copy would invite deleting it.
// The lengths are the whole group's, not one identifier's holders', so a book of
// unknown length sharing one identifier with each of two different recordings
// can't make them look like copies. Then identical audio (only when every copy
// is: a group that identical audio joins only in part says what joins the rest),
// a shared identifier, and author/title/narrator, strongest evidence first.
func (s dupSet) reason() string {
	switch {
	case s.joins&joinID != 0 && s.lengthsDiffer:
		return dupIDLengthDiffers
	case s.sameFiles:
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

// findDuplicateSets groups a library's copies by their identity signals. Audio
// also matches in size, and title/author/narrator in duration. With across,
// include direct cross-library pairs touching the requested library. Split discs
// have their own issue. The Health summary counts only in-library groups.
func findDuplicateSets(ctx context.Context, db rowQuerier, libraryID int64, across bool) ([]dupRow, []dupSet, error) {
	readLib := libraryID
	if across {
		readLib = 0
	}
	rows, err := queryRows(ctx, db, func(rows *sql.Rows, r *dupRow) error {
		return rows.Scan(&r.id, &r.libraryID, &r.path, &r.title, &r.author, &r.narrator, &r.asin,
			&r.isbn, &r.hash, &r.format, &r.duration, &r.size, &r.files, &r.listeners, &r.legacyIgnore)
	}, `SELECT b.id, b.library_id, b.rel_path, b.title, b.author, b.narrator, b.asin, b.isbn,
	           b.content_hash, b.format, b.duration, b.size, MAX(1, COALESCE(bf.n, 0)),
	           COALESCE(p.n, 0), COALESCE((SELECT MIN(ii.kind) FROM issue_ignores ii WHERE
	               ii.library_id=b.library_id AND ii.path=b.rel_path AND ii.kind IN ('duplicate','duplicate_mapped')), '')
	      FROM books b
	      LEFT JOIN (SELECT book_id, COUNT(*) AS n FROM book_files GROUP BY book_id) bf ON bf.book_id = b.id
	      LEFT JOIN (SELECT library_id, rel_path, COUNT(*) AS n FROM progress GROUP BY library_id, rel_path) p
	             ON p.library_id = b.library_id AND p.rel_path = b.rel_path
	     WHERE `+inBookLibrary+` AND (?1 = 0 OR b.library_id = ?1) AND NOT `+splitDiscExpr, readLib)
	if err != nil {
		return nil, nil, err
	}
	set := newDisjointSet(len(rows))
	joins := make([]dupJoin, len(rows))
	files := map[string]int{}       // the first book with this audio
	audio := make([]int, len(rows)) // the first book with each row's audio, -1 for none
	byID := map[string]int{}        // the first book with this ASIN/ISBN
	byMeta := map[string][]int{}    // every book with this author/title/narrator
	// Across-library comparisons are direct pairs. Keep each signal's copies by
	// library so a large group within one library still takes a single join.
	cross := crossMatches{}
	for i := range rows {
		r := &rows[i]
		lib := strconv.FormatInt(r.libraryID, 10) + ":"
		audio[i] = -1
		for _, sig := range identitySignals(r.quality()) {
			if across {
				cross.add(rows, i, sig, libraryID)
			}
			switch {
			case strings.HasPrefix(sig, "m:"):
				byMeta[lib+sig] = append(byMeta[lib+sig], i)
			case strings.HasPrefix(sig, "h:"):
				key := lib + sig + ":" + strconv.FormatInt(r.size, 10)
				if j, ok := files[key]; ok {
					set.union(i, j)
					audio[i] = j
				} else {
					files[key] = i
					audio[i] = i
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
	sets := cross.sets()
	for _, members := range set.groups() {
		if len(members) < 2 {
			continue
		}
		if libraryID != 0 && rows[members[0]].libraryID != libraryID {
			continue
		}
		ds := dupSet{members: members, sameFiles: true}
		shortest, longest := 0.0, 0.0 // known lengths; 0 = none known
		for _, i := range members {
			ds.joins |= joins[i]
			ds.sameFiles = ds.sameFiles && audio[i] >= 0 && audio[i] == audio[members[0]]
			if d := rows[i].duration; d > 0 {
				if longest == 0 {
					shortest = d
				}
				shortest, longest = min(shortest, d), max(longest, d)
			}
		}
		ds.lengthsDiffer = longest > 0 && !sameLength(shortest, longest)
		sets = append(sets, ds)
	}
	decisions, err := readDuplicateDecisions(ctx, db, libraryID)
	if err != nil {
		return nil, nil, err
	}
	for i := range sets {
		s := &sets[i]
		refs := make([]Ref, 0, len(s.members))
		allOld, unresolved := true, false
		for _, m := range s.members {
			r := rows[m]
			refs = append(refs, r.ref())
			allOld = allOld && r.legacyIgnore != ""
			unresolved = unresolved || r.legacyIgnore == IssueDuplicate
		}
		s.key = comparisonKey(refs)
		if d, found := decisions[s.key]; found {
			s.ignored = d.ignored
		} else {
			s.ignored = allOld && unresolved
		}
		sort.SliceStable(s.members, func(a, b int) bool {
			return betterCopy(rows[s.members[a]], rows[s.members[b]])
		})
	}
	// In a stable order: by library, then the kept copy's path.
	sort.Slice(sets, func(a, b int) bool {
		ra, rb := rows[sets[a].members[0]], rows[sets[b].members[0]]
		if ra.libraryID != rb.libraryID {
			return ra.libraryID < rb.libraryID
		}
		return cmp.Or(compareFold(ra.path, rb.path), strings.Compare(sets[a].key, sets[b].key)) < 0
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

// DuplicatePage returns a page of the groups of copies (duplicateSets) with each
// copy's admin row. q.Ignored selects dismissed comparisons. q.Across adds
// direct cross-library pairs. The groups are found once a
// generation (duplicateSets keeps them; they read the library's rows, not the
// admin rows, which only the page's copies need) and paged by keyset on their
// order: the kept copy's library and path, then the exact comparison key. A group
// that comes or goes between pages moves nothing else. ErrInvalidCursor for a cursor that isn't one.
func (c *Catalog) DuplicatePage(ctx context.Context, q DuplicateQuery) (DuplicatePage, error) {
	limit := q.Limit
	if limit <= 0 || limit > maxDuplicateGroups {
		limit = maxDuplicateGroups
	}
	var afterLib int64
	var afterPath, afterKey string
	if q.Cursor != "" {
		var err error
		if afterPath, afterLib, err = decodeCursor(q.Cursor); err != nil {
			return DuplicatePage{}, ErrInvalidCursor
		}
		afterPath, afterKey, _ = strings.Cut(afterPath, "\x00")
	}
	rows, sets, err := c.duplicateSets(ctx, q.LibraryID, q.Across)
	if err != nil {
		return DuplicatePage{}, err
	}
	// Filtered before paging, so every ignored group can be reached to show
	// again (into a slice of its own: sets may be the cache's).
	sets = slices.DeleteFunc(slices.Clone(sets), func(s dupSet) bool { return s.ignored != q.Ignored })
	page := DuplicatePage{Total: len(sets), Groups: []DuplicateGroup{}}
	if q.Cursor != "" {
		// The first group past the cursor, in duplicateSets' order.
		start := slices.IndexFunc(sets, func(s dupSet) bool {
			keep := rows[s.members[0]]
			if d := cmp.Compare(keep.libraryID, afterLib); d != 0 {
				return d > 0
			}
			return compareFold(keep.path, afterPath) > 0 || (keep.path == afterPath && afterKey != "" && s.key > afterKey)
		})
		if start < 0 {
			start = len(sets)
		}
		sets = sets[start:]
	}
	end, books := 0, 0
	for end < len(sets) && end < limit {
		n := len(sets[end].members)
		if q.Limit > 0 && end > 0 && books+n > maxDuplicatePageBooks {
			break
		}
		books += n
		end++
	}
	if end < len(sets) {
		last := rows[sets[end-1].members[0]]
		page.NextCursor = encodeCursor(last.path+"\x00"+sets[end-1].key, last.libraryID)
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
		g := DuplicateGroup{Key: s.key, Reason: s.reason(), Ignored: s.ignored}
		for _, i := range s.members {
			if b, ok := byID[rows[i].id]; ok {
				g.Books = append(g.Books, DuplicateMember{AdminBook: b, Listeners: rows[i].listeners})
			}
		}
		if len(g.Books) == len(s.members) {
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
	if kind == IssueDuplicate {
		return c.setDuplicateDecision(ctx, refs, userID, true)
	}
	now := c.ts()
	return c.db.WithTx(ctx, "IgnoreIssue", func(tx *sql.Tx) error {
		for _, r := range refs {
			storedKind, storedPath := issueStorage(kind, r.Path)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO issue_ignores(library_id, path, kind, created_by, created_at)
				 SELECT ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM libraries WHERE id = ?)
				 ON CONFLICT DO NOTHING`,
				r.LibraryID, storedPath, storedKind, userID, now, r.LibraryID); err != nil {
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
	if kind == IssueDuplicate {
		return c.setDuplicateDecision(ctx, refs, 0, false)
	}
	return c.db.WithTx(ctx, "UnignoreIssue", func(tx *sql.Tx) error {
		for _, r := range refs {
			storedKind, storedPath := issueStorage(kind, r.Path)
			if kind == IssueSplitDiscs {
				if _, err := tx.ExecContext(ctx, `DELETE FROM issue_ignores WHERE library_id=? AND kind='split_discs'
					AND (path=? OR path IN (SELECT rel_path FROM books WHERE library_id=? AND split_parent=?))`, r.LibraryID, r.Path, r.LibraryID, storedPath); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM issue_ignores WHERE library_id = ? AND path = ? AND kind = ?`,
				r.LibraryID, storedPath, storedKind); err != nil {
				return err
			}
		}
		return nil
	})
}

// The console addresses a split issue by its displayed disc, including for Undo.
func issueStorage(kind, p string) (string, string) {
	if kind == IssueSplitDiscs && path.Dir(p) != "." {
		return "split_disc_set", path.Dir(p)
	}
	return kind, p
}

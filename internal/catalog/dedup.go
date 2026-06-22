package catalog

import (
	"database/sql"
	"sort"
	"strings"
	"unicode"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// De-duplication of "the same book" across libraries (and, later, servers).
//
// A library is addressed by (library_id, rel_path), so the same audiobook copied
// into two libraries is two distinct rows and shows up twice in cross-library
// lists (search, recently-added). We collapse those to a single entry, keeping
// the best-quality copy and listing the rest as OtherLocations.
//
// Two signals decide grouping and which copy wins, both computed identically on
// the client (hand-mirrored, no codegen) so cross-server de-dup agrees:
//
//   - identity (grouping): books are "the same" if they share ANY of asin, isbn,
//     content fingerprint, or normalized author|title|narrator. Transitively
//     unioned, so a chain of partial matches collapses to one group.
//   - quality (winner): format tier (M4B/AAC > MP3 > other) → single-file over
//     multipart → bitrate (size÷duration) → library sort_order. The last is the
//     decisive tiebreaker for otherwise-identical copies.

// candidate is a book row enriched with the per-library/-file facts needed to
// de-duplicate and rank it. rankIdx is its position in the source ordering (FTS
// rank for search, added_at for recent) — lower is better — and is the final,
// always-unique tiebreaker so ranking is a total order.
type candidate struct {
	book      Book
	sortOrder int
	libName   string
	fileCount int
	rankIdx   int
}

// dedupFetch is how many ranked/ordered rows to pull before de-duplicating, so
// the result can still hold up to limit distinct books when duplicates exist.
func dedupFetch(limit int) int {
	n := limit * 4
	if n > 200 {
		n = 200
	}
	if n < limit {
		n = limit
	}
	return n
}

// dedupCols is the SELECT list a de-duplicating query must use: the book columns
// (prefixed b.) plus the library order/name and the book's audio-file count.
const dedupCols = `, l.sort_order, l.name, COALESCE(bf.n, 0)`

// dedupJoins joins the library (order + name) and a per-book audio-file count so a
// single query yields everything de-dup needs.
const dedupJoins = `
	JOIN libraries l ON l.id = b.library_id
	LEFT JOIN (SELECT book_id, COUNT(*) AS n FROM book_files GROUP BY book_id) bf
	       ON bf.book_id = b.id`

// scanCandidate scans a row selected as prefixCols("b.") + dedupCols.
func scanCandidate(rows *sql.Rows, rankIdx int) (candidate, error) {
	var c candidate
	b := &c.book
	if err := rows.Scan(&b.ID, &b.LibraryID, &b.RelPath, &b.IsFolder, &b.Title, &b.Author,
		&b.Series, &b.SeriesIndex, &b.Narrator, &b.Duration, &b.ASIN, &b.ISBN,
		&b.CoverPath, &b.Format, &b.Codec, &b.Size, &b.MTime, &b.AddedAt, &b.ContentHash,
		&c.sortOrder, &c.libName, &c.fileCount); err != nil {
		return candidate{}, err
	}
	c.rankIdx = rankIdx
	return c, nil
}

// dedupBooks collapses candidates that refer to the same book, returning at most
// limit winners in source order (a group sits at its earliest member's position),
// each annotated with DedupKey, MultiFile and OtherLocations.
func dedupBooks(cands []candidate, limit int) []Book {
	parent := make([]int, len(cands))
	for i := range parent {
		parent[i] = i
	}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]] // path-halving
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		if ra, rb := find(a), find(b); ra != rb {
			parent[ra] = rb
		}
	}
	// Union any two candidates that share an identity signal.
	firstWith := map[string]int{}
	for i := range cands {
		for _, s := range identitySignals(cands[i].book) {
			if j, ok := firstWith[s]; ok {
				union(i, j)
			} else {
				firstWith[s] = i
			}
		}
	}

	groupOrder := []int{} // roots, in first-occurrence order
	members := map[int][]int{}
	for i := range cands {
		r := find(i)
		if _, ok := members[r]; !ok {
			groupOrder = append(groupOrder, r)
		}
		members[r] = append(members[r], i)
	}

	out := make([]Book, 0, len(groupOrder))
	for _, r := range groupOrder {
		if limit > 0 && len(out) >= limit {
			break
		}
		ms := members[r]
		win := ms[0]
		for _, m := range ms[1:] {
			if cands[m].betterThan(cands[win]) {
				win = m
			}
		}
		b := cands[win].book
		mf := cands[win].fileCount > 1
		b.MultiFile = &mf
		b.DedupKey = exposedDedupKey(b)
		b.OtherLocations = otherLocations(cands, ms, win)
		out = append(out, b)
	}
	return out
}

// otherLocations returns the same book's best copy in each OTHER library — one
// entry per distinct library, never the winner's own — ordered by library order
// then name. This keeps "also in A, B" clean: no repeated libraries even when a
// library holds several copies of the book.
func otherLocations(cands []candidate, members []int, win int) []BookLocation {
	winLib := cands[win].book.LibraryID
	best := map[int64]int{}
	for _, m := range members {
		lib := cands[m].book.LibraryID
		if m == win || lib == winLib {
			continue
		}
		if cur, ok := best[lib]; !ok || cands[m].betterThan(cands[cur]) {
			best[lib] = m
		}
	}
	if len(best) == 0 {
		return nil
	}
	picked := make([]int, 0, len(best))
	for _, m := range best {
		picked = append(picked, m)
	}
	sort.Slice(picked, func(i, j int) bool {
		ci, cj := cands[picked[i]], cands[picked[j]]
		if ci.sortOrder != cj.sortOrder {
			return ci.sortOrder < cj.sortOrder
		}
		return ci.libName < cj.libName
	})
	out := make([]BookLocation, 0, len(picked))
	for _, m := range picked {
		out = append(out, BookLocation{
			LibraryID:   cands[m].book.LibraryID,
			LibraryName: cands[m].libName,
			Path:        cands[m].book.RelPath,
			Format:      cands[m].book.Format,
			Size:        cands[m].book.Size,
			MultiFile:   cands[m].fileCount > 1,
		})
	}
	return out
}

// betterThan reports whether c is a better copy to surface than o. Compared in
// order: format tier, single vs multipart, bitrate, library order, source rank.
func (c candidate) betterThan(o candidate) bool {
	if a, b := formatTier(c.book.Format), formatTier(o.book.Format); a != b {
		return a > b
	}
	if a, b := c.fileCount <= 1, o.fileCount <= 1; a != b {
		return a // single-file beats multipart
	}
	if a, b := bitrate(c.book), bitrate(o.book); a != b {
		return a > b
	}
	if c.sortOrder != o.sortOrder {
		return c.sortOrder < o.sortOrder
	}
	return c.rankIdx < o.rankIdx
}

// formatTier ranks containers by audiobook quality: AAC/MP4 family (chaptered,
// efficient) over MP3 over everything else. Cross-codec bitrate is unreliable
// (AAC ≠ MP3 at equal kbps), so format leads the ranking.
func formatTier(format string) int {
	switch strings.TrimPrefix(strings.ToLower(strings.TrimSpace(format)), ".") {
	case "m4b", "m4a", "mp4", "aac", "m4p", "m4v":
		return 3
	case "mp3":
		return 2
	default:
		return 1
	}
}

// bitrate approximates audio quality as bytes per second. Falls back to raw size
// when duration is unknown (so a larger file still ranks higher).
func bitrate(b Book) float64 {
	if b.Duration > 0 {
		return float64(b.Size) / b.Duration
	}
	return float64(b.Size)
}

// identitySignals returns every identity token a book carries. Two books sharing
// any token are treated as the same book. The metadata token is included only
// when there's a title or author to match on (so untitled books don't all merge).
func identitySignals(b Book) []string {
	var s []string
	if v := norm(b.ASIN); v != "" {
		s = append(s, "a:"+v)
	}
	if v := norm(b.ISBN); v != "" {
		s = append(s, "i:"+v)
	}
	if b.ContentHash != "" {
		s = append(s, "h:"+b.ContentHash)
	}
	if m := metaKey(b); m != "" {
		s = append(s, m)
	}
	return s
}

// exposedDedupKey is the group key sent on the wire. It prefers signals a client
// can also compute from the book's fields (asin/isbn/metadata) over the
// server-only content fingerprint, so client-side cross-server de-dup agrees.
func exposedDedupKey(b Book) string {
	if v := norm(b.ASIN); v != "" {
		return "a:" + v
	}
	if v := norm(b.ISBN); v != "" {
		return "i:" + v
	}
	if m := metaKey(b); m != "" {
		return m
	}
	if b.ContentHash != "" {
		return "h:" + b.ContentHash
	}
	return ""
}

// metaKey is the normalized author|title|narrator identity, or "" when the
// metadata is too weak to safely merge two books on. It requires a real author
// AND a non-generic title — without that guard, generically-tagged files (every
// "Track 01", every "Chapter 1") collide into one giant false group. Such books
// still de-dup via the content fingerprint / asin / isbn.
func metaKey(b Book) string {
	a, t := norm(b.Author), norm(b.Title)
	if a == "" || t == "" || metadata.IsGenericTitle(b.Title) {
		return ""
	}
	return "m:" + a + "|" + t + "|" + norm(b.Narrator)
}

// norm lowercases and collapses any run of non-alphanumerics to a single space so
// trivial punctuation/spacing/case differences don't split a group.
func norm(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			space = false
		case !space:
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

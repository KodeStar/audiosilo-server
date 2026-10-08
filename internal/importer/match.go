package importer

import (
	"math"
	"path"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// How an ABS item matched a book here (catalog.ImportMatched counts them).
const (
	TierPath  = "path"
	TierASIN  = "asin"
	TierISBN  = "isbn"
	TierTitle = "title"
)

// Match is where an ABS item's history goes: a book (Tier set), or nowhere
// (Reason, a catalog.Reason*).
type Match struct {
	Ref      catalog.Ref
	Tier     string
	Reason   string
	Duration float64 // the book's, 0 when unknown
}

// Matching, best evidence first:
//
//  1. Path. Most people point ABS and AudioSilo at the same folders, so an
//     item's path names the book. Paths compare as whole components from the
//     end: the same relative path, or one a suffix of the other (the two
//     libraries rooted at different depths; the ABS side compares its absolute
//     path, so the folders above its library folder count too). A book one side
//     keeps as a folder and the other as the single file in it matches as well
//     (when the file is alone there). The closest unique candidate wins.
//  2. The ASIN, then the ISBN.
//  3. Title, author and series (pkg/match.Best), only among the books of a
//     matching author.
//
// Every tier but the identifiers checks the lengths agree (5% + 2 minutes, the
// manager's tolerance), considers only the books the person can access, and
// reports no_access when the only match is a book they can't. An item ABS deleted
// has no path, only its last session's metadata (tiers 2 and 3). Two items that
// resolve to one book are both skipped (contested: one of them is wrong).

// bookIndex is the indexed books arranged for matching, whoever they are matched
// for: a fetch builds it once for every user it imports. Every lookup is through
// an index, so a large library costs no item-by-book scans (only the fuzzy tier
// reads the books of a matching author).
type bookIndex struct {
	books []catalog.Book
	comps [][]string // rel_path's components
	// byName: books by their last path component; byFolder: single-file books by
	// the name of the folder holding them; inFolder counts the books directly in
	// a folder.
	byName   map[string][]int
	byFolder map[string][]int
	inFolder map[folderRef]int
	// splits: the folders holding a book split across disc folders, by name.
	splits map[string][]splitSet
	byASIN map[string][]int
	byISBN map[string][]int
	// authors maps a normalized author to their books.
	authors map[string][]int
}

// matcher matches ABS items for one person: the index, and which of its books
// they can access.
type matcher struct {
	*bookIndex
	ok []bool // the person can access the book
	// byAuthor memoizes a query author's candidates.
	byAuthor map[string][]int
}

type folderRef struct {
	lib int64
	dir string
}

type splitSet struct {
	ref   catalog.Ref
	comps []string
	book  int // one of its disc books (they share its access)
}

// splitPath is p's components, Unicode-normalized (NFC: a macOS share may hand
// one side decomposed names).
func splitPath(p string) []string {
	p = norm.NFC.String(strings.ReplaceAll(p, `\`, "/"))
	var out []string
	for c := range strings.SplitSeq(p, "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

func last(c []string) string {
	if len(c) == 0 {
		return ""
	}
	return c[len(c)-1]
}

func newBookIndex(books []catalog.Book) *bookIndex {
	m := &bookIndex{books: books, comps: make([][]string, len(books)),
		byName: map[string][]int{}, byFolder: map[string][]int{}, inFolder: map[folderRef]int{},
		splits: map[string][]splitSet{}, byASIN: map[string][]int{}, byISBN: map[string][]int{},
		authors: map[string][]int{}}
	seenSplit := map[catalog.Ref]bool{}
	for i, b := range books {
		c := splitPath(b.RelPath)
		m.comps[i] = c
		m.byName[last(c)] = append(m.byName[last(c)], i)
		if len(c) > 1 {
			m.inFolder[folderRef{b.LibraryID, path.Dir(b.RelPath)}]++
			if !b.IsFolder {
				parent := c[len(c)-2]
				m.byFolder[parent] = append(m.byFolder[parent], i)
			}
		}
		if b.SplitParent != "" {
			sref := catalog.Ref{LibraryID: b.LibraryID, Path: b.SplitParent}
			if !seenSplit[sref] {
				seenSplit[sref] = true
				sc := splitPath(b.SplitParent)
				m.splits[last(sc)] = append(m.splits[last(sc)], splitSet{sref, sc, i})
			}
		}
		if k := match.NormalizeASIN(b.ASIN); k != "" {
			m.byASIN[k] = append(m.byASIN[k], i)
		}
		if k := match.NormalizeISBN(b.ISBN); k != "" {
			m.byISBN[k] = append(m.byISBN[k], i)
		}
		if a := match.Normalize(b.Author); a != "" {
			m.authors[a] = append(m.authors[a], i)
		}
	}
	return m
}

// matcher is the index for one person (allow: they can access the book).
func (x *bookIndex) matcher(allow func(catalog.Ref) bool) *matcher {
	m := &matcher{bookIndex: x, ok: make([]bool, len(x.books)), byAuthor: map[string][]int{}}
	for i, b := range x.books {
		m.ok[i] = allow(catalog.Ref{LibraryID: b.LibraryID, Path: b.RelPath})
	}
	return m
}

// lengthsAgree reports whether two lengths (seconds; 0 = unknown, which agrees
// with anything) can be one book's: within 5% of the longer, plus 2 minutes.
func lengthsAgree(a, b float64) bool {
	return a <= 0 || b <= 0 || math.Abs(a-b) <= 0.05*math.Max(a, b)+120
}

// pathScore is how many components a book path (a) shares from the end with an
// ABS path (full: its absolute components, the last relLen of them its path
// relative to its library folder), when one names the other: a is a suffix of
// the absolute path, or the relative path a suffix of a. 0 when neither.
func pathScore(a, full []string, relLen int) int {
	k := 0
	for k < len(a) && k < len(full) && a[len(a)-1-k] == full[len(full)-1-k] {
		k++
	}
	if k > 0 && (k == len(a) || (relLen > 0 && k >= relLen)) {
		return k
	}
	return 0
}

// candidate is a book a tier found, ranked by exact (the same relative path)
// then score.
type candidate struct {
	idx   int
	exact bool
	score int
}

// pick is the one best candidate the person can access (ok); -1 when there is
// none or two tie. denied: a candidate fits but only ones they can't access do.
func (m *matcher) pick(cands []candidate) (idx int, denied bool) {
	best, tie := -1, false
	var top candidate
	for _, c := range cands {
		if !m.ok[c.idx] {
			denied = true
			continue
		}
		switch {
		case best < 0 || c.exact && !top.exact || c.exact == top.exact && c.score > top.score:
			best, top, tie = c.idx, c, false
		case c.exact == top.exact && c.score == top.score && c.idx != best:
			tie = true
		}
	}
	if best >= 0 {
		denied = false
	}
	if tie {
		return -1, false
	}
	return best, denied
}

// matchPath is the path tier for a (present) item.
func (m *matcher) matchPath(it Item) (idx int, denied, split bool) {
	full, rel := splitPath(it.FullPath), splitPath(it.RelPath)
	if len(full) < len(rel) || !strings.HasSuffix("/"+strings.Join(full, "/"), "/"+strings.Join(rel, "/")) {
		full = rel // no usable absolute path
	}
	if len(rel) == 0 {
		return -1, false, false
	}
	var cands []candidate
	add := func(i, score int, exact bool) {
		if score > 0 && lengthsAgree(it.Duration, m.books[i].Duration) {
			cands = append(cands, candidate{i, exact, score})
		}
	}
	for _, i := range m.byName[last(full)] {
		a := m.comps[i]
		score := pathScore(a, full, len(rel))
		add(i, score, len(a) == len(rel) && score >= len(rel))
	}
	if idx, denied = m.pick(cands); idx >= 0 {
		return idx, false, false
	}
	if !it.IsFile {
		// A book this side splits into one per disc folder: the history can't be
		// divided between them.
		for _, s := range m.splits[last(full)] {
			if m.ok[s.book] && pathScore(s.comps, full, len(rel)) > 0 {
				return -1, false, true
			}
		}
		// The ABS folder is a single file here, alone in that folder.
		cands = cands[:0]
		for _, i := range m.byFolder[last(full)] {
			a := m.comps[i]
			if m.inFolder[folderRef{m.books[i].LibraryID, path.Dir(m.books[i].RelPath)}] == 1 {
				add(i, pathScore(a[:len(a)-1], full, len(rel)), false)
			}
		}
	} else if it.Siblings == 1 && len(full) > 1 {
		// The ABS file is alone in its folder, and that folder is a book here.
		cands = cands[:0]
		dir := full[:len(full)-1]
		for _, i := range m.byName[last(dir)] {
			if m.books[i].IsFolder {
				add(i, pathScore(m.comps[i], dir, len(rel)-1), false)
			}
		}
	}
	idx, d := m.pick(cands)
	return idx, denied || d, false
}

// matchIdentifier is the ASIN or ISBN tier: the first book (in library order)
// carrying it that the person can access.
func (m *matcher) matchIdentifier(index map[string][]int, key string) (idx int, denied bool) {
	if key == "" {
		return -1, false
	}
	for _, i := range index[key] {
		if m.ok[i] {
			return i, false
		}
		denied = true
	}
	return -1, denied
}

// matchTitle is the fuzzy tier: pkg/match.Best among the books of a matching
// author, then the length check.
func (m *matcher) matchTitle(it Item) (idx int, denied bool) {
	na := match.Normalize(it.Author)
	if na == "" || strings.TrimSpace(it.Title) == "" {
		return -1, false
	}
	pool, seen := m.byAuthor[na]
	if !seen {
		// pkg/match's person gate, over the distinct authors rather than every
		// book.
		for a, idxs := range m.authors {
			if match.NamesMatch(a, na) {
				pool = append(pool, idxs...)
			}
		}
		// Library order, not the map's: Best keeps the first of equal scores, so
		// the review and the apply (and every re-import) pick the same book.
		slices.Sort(pool)
		m.byAuthor[na] = pool
	}
	q := match.Query{Title: it.Title, Author: it.Author, Series: it.Series}
	if seq, err := strconv.ParseFloat(it.Sequence, 64); err == nil {
		q.Sequence, q.HasSequence = seq, true
	}
	best := func(want bool) int {
		var idxs []int
		var cands []match.Book
		for _, i := range pool {
			if m.ok[i] == want {
				b := m.books[i]
				idxs = append(idxs, i)
				cands = append(cands, match.Book{Title: b.Title, Author: b.Author, Series: b.Series,
					SeriesIndex: b.SeriesIndex})
			}
		}
		if j, ok := match.Best(cands, q); ok && lengthsAgree(it.Duration, m.books[idxs[j]].Duration) {
			return idxs[j]
		}
		return -1
	}
	if i := best(true); i >= 0 {
		return i, false
	}
	return -1, best(false) >= 0
}

// match finds where one item's history goes (contested is settled by matchAll).
func (m *matcher) match(it Item) Match {
	denied := false
	found := func(i int, tier string) Match {
		b := m.books[i]
		return Match{Ref: catalog.Ref{LibraryID: b.LibraryID, Path: b.RelPath}, Tier: tier, Duration: b.Duration}
	}
	if !it.Gone {
		i, d, s := m.matchPath(it)
		if i >= 0 {
			return found(i, TierPath)
		}
		if s {
			return Match{Reason: catalog.ReasonSplit}
		}
		denied = d
	}
	for _, tier := range []struct {
		name  string
		index map[string][]int
		key   string
	}{{TierASIN, m.byASIN, match.NormalizeASIN(it.ASIN)}, {TierISBN, m.byISBN, match.NormalizeISBN(it.ISBN)}} {
		i, d := m.matchIdentifier(tier.index, tier.key)
		if i >= 0 {
			return found(i, tier.name)
		}
		denied = denied || d
	}
	i, d := m.matchTitle(it)
	if i >= 0 {
		return found(i, TierTitle)
	}
	if denied || d {
		return Match{Reason: catalog.ReasonNoAccess}
	}
	return Match{Reason: catalog.ReasonNoMatch}
}

// matchAll matches every item, keyed by item id. Items that resolve to the same
// book are all left unmatched (contested) when more than one of them is present
// in ABS: each has its own progress and bookmarks, and only one can be the
// book's. A gone item has neither (ABS deleted them with it; buildPayload drops
// any left), only sessions, so gone items on one book all match it with the one
// present item there, if any: their sessions merge.
func (m *matcher) matchAll(items []Item) map[string]Match {
	out := make(map[string]Match, len(items))
	present := map[catalog.Ref]int{}
	for _, it := range items {
		r := m.match(it)
		out[it.ID] = r
		if r.Tier != "" && !it.Gone {
			present[r.Ref]++
		}
	}
	for id, r := range out {
		if r.Tier != "" && present[r.Ref] > 1 {
			out[id] = Match{Reason: catalog.ReasonContest}
		}
	}
	return out
}

package catalog

import "context"

// The duplicates cache (Catalog.dups, a store.GenMap) keeps the groups of copies
// duplicate detection found (findDuplicateSets: every book of the library read,
// grouped and ranked, tens of milliseconds at 5,000 books), so the Health
// summary's count and each page of Health > Duplicates share one detection, and
// a console that refetches every loaded page (an ignore's Undo, a refocus) does
// not run it once a page (server#270). The groups are a function of the
// database alone, read from the books, their files, the issue ignores and the
// progress rows (each copy's listeners rank it), so they are kept under the
// content generation and keyed on the listener generation too (a new or removed
// progress row moves only that one: store.DB.ListenerGeneration), each read
// before the detection reads anything.

// maxDuplicateCaches bounds the cache: one entry per library asked for, across
// or not, and the summary's.
const maxDuplicateCaches = 16

// dupKey is what one detection was asked: a library (0 = all), across
// libraries or not, at a listener generation.
type dupKey struct {
	libraryID int64
	across    bool
	listeners uint64
}

// dupFound is one detection's groups and the rows their members index: only
// the grouped books' rows (compactDuplicates), not the library's.
type dupFound struct {
	rows []dupRow
	sets []dupSet
}

// duplicateSets is findDuplicateSets through the duplicates cache. The rows and
// sets returned may be the cache's: callers never change them (DuplicatePage
// filters a clone of sets).
func (c *Catalog) duplicateSets(ctx context.Context, libraryID int64, across bool) ([]dupRow, []dupSet, error) {
	gen, cacheable := c.ContentGeneration() // before the read (store.GenMap)
	listeners, _ := c.ListenerGeneration()
	key := dupKey{libraryID: libraryID, across: across, listeners: listeners}
	if cacheable {
		if f, ok := c.dups.Get(gen, key); ok {
			return f.rows, f.sets, nil
		}
	}
	rows, sets, err := c.findDuplicateSets(ctx, libraryID, across)
	if err != nil {
		return nil, nil, err
	}
	rows, sets = compactDuplicates(rows, sets)
	if cacheable {
		c.dups.Put(gen, key, dupFound{rows: rows, sets: sets})
	}
	return rows, sets, nil
}

// compactDuplicates keeps only the rows sets' members index (a library with a
// few copies holds a few rows, not every book's), renumbering the members.
func compactDuplicates(rows []dupRow, sets []dupSet) ([]dupRow, []dupSet) {
	n := 0
	for _, s := range sets {
		n += len(s.members)
	}
	kept := make([]dupRow, 0, n)
	for i := range sets {
		members := make([]int, len(sets[i].members))
		for j, m := range sets[i].members {
			members[j] = len(kept)
			kept = append(kept, rows[m])
		}
		sets[i].members = members
	}
	return kept, sets
}

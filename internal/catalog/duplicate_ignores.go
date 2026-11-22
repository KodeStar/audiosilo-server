package catalog

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// Each pair needs its own evidence: a third copy can share an identifier with
// only one of the other two. In-library groups keep their transitive joins.
type crossMatches struct {
	signals map[string]map[int64][]int
	pairs   map[[2]int]dupSet
}

func (c *crossMatches) add(rows []dupRow, i int, sig string, libraryID int64) {
	if c.signals == nil {
		c.signals = make(map[string]map[int64][]int)
		c.pairs = make(map[[2]int]dupSet)
	}
	r := rows[i]
	if sig[0] == 'h' {
		sig += ":" + strconv.FormatInt(r.size, 10)
	}
	libraries := c.signals[sig]
	if libraries == nil {
		libraries = make(map[int64][]int)
		c.signals[sig] = libraries
	}
	for lib, others := range libraries {
		if lib == r.libraryID || (libraryID != 0 && lib != libraryID && r.libraryID != libraryID) {
			continue
		}
		for _, j := range others {
			other := rows[j]
			matchLength := sameLength(r.duration, other.duration)
			if sig[0] == 'm' && !matchLength {
				continue
			}
			key := [2]int{j, i}
			s := c.pairs[key]
			if s.members == nil {
				s.members = []int{j, i}
			}
			switch sig[0] {
			case 'h':
				s.sameFiles = true
			case 'a', 'i':
				s.joins |= joinID
				s.lengthsDiffer = r.duration > 0 && other.duration > 0 && !matchLength
			}
			c.pairs[key] = s
		}
	}
	libraries[r.libraryID] = append(libraries[r.libraryID], i)
}

func (c *crossMatches) sets() []dupSet {
	sets := make([]dupSet, 0, len(c.pairs))
	for _, s := range c.pairs {
		sets = append(sets, s)
	}
	return sets
}

func sortComparison(refs []Ref) {
	slices.SortFunc(refs, func(a, b Ref) int {
		return cmp.Or(cmp.Compare(a.LibraryID, b.LibraryID), strings.Compare(a.Path, b.Path))
	})
}

// comparisonKey sorts its scratch references into storage order. JSON frames
// the paths, including literal quotes and separators.
func comparisonKey(refs []Ref) string {
	sortComparison(refs)
	b, _ := json.Marshal(refs)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type duplicateDecision struct {
	first, last int64
	paths       string
	ignored     bool
	by          sql.NullInt64
	at          string
}

func (d duplicateDecision) refs() ([]Ref, error) {
	var paths []string
	if err := json.Unmarshal([]byte(d.paths), &paths); err != nil {
		return nil, err
	}
	refs := make([]Ref, len(paths))
	for i, p := range paths {
		refs[i] = Ref{d.first, p}
	}
	refs[len(refs)-1].LibraryID = d.last
	return refs, nil
}

func scanDecision(rows *sql.Rows, d *duplicateDecision) error {
	return rows.Scan(&d.first, &d.last, &d.paths, &d.ignored, &d.by, &d.at)
}

const decisionCols = "library_id, other_library_id, paths, ignored, created_by, created_at"

func readDuplicateDecisions(ctx context.Context, db rowQuerier, lib int64) (map[string]bool, error) {
	rows, err := queryRows(ctx, db, scanDecision, `SELECT `+decisionCols+` FROM duplicate_decisions
		WHERE ?1=0 OR library_id=?1 OR other_library_id=?1`, lib)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, d := range rows {
		refs, err := d.refs()
		if err != nil {
			return nil, err
		}
		out[comparisonKey(refs)] = d.ignored
	}
	return out, nil
}

var ErrDuplicateSelection = errors.New("select a duplicate group in one library or a pair in different libraries")

// Re-encode individual strings in SQLite's JSON spelling, the same one 0092
// used. The Go encoder can spell e.g. an ampersand as a Unicode escape.
const decisionPaths = `(SELECT json_group_array(value ORDER BY key) FROM json_each(?3))`

func storeDecision(ctx context.Context, tx *sql.Tx, refs []Ref, ignored bool, by sql.NullInt64, at string) error {
	first, last := refs[0].LibraryID, refs[len(refs)-1].LibraryID
	paths := make([]string, len(refs))
	for i, r := range refs {
		paths[i] = r.Path
	}
	b, _ := json.Marshal(paths)
	if !ignored {
		// Only unresolved pre-upgrade rows need an explicit "suggest again".
		// Removing a member flag here would also reopen its other comparisons.
		if _, err := tx.ExecContext(ctx, `DELETE FROM duplicate_decisions WHERE library_id=?1 AND other_library_id=?2 AND paths=`+decisionPaths, first, last, string(b)); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO duplicate_decisions(`+decisionCols+`)
		SELECT ?1,?2,`+decisionPaths+`,?4,?5,?6
		WHERE EXISTS (SELECT 1 FROM libraries WHERE id=?1) AND EXISTS (SELECT 1 FROM libraries WHERE id=?2)
        AND (?4 OR EXISTS(SELECT 1 FROM json_each(?3) p JOIN issue_ignores ii
			ON ii.path=p.value AND ii.library_id=CASE WHEN p.key=0 THEN ?1 ELSE ?2 END AND ii.kind='duplicate'))
		ON CONFLICT(library_id,other_library_id,paths) DO UPDATE SET ignored=excluded.ignored,
			created_by=excluded.created_by,created_at=excluded.created_at WHERE ignored<>excluded.ignored`,
		first, last, string(b), ignored, by, at)
	return err
}

func (c *Catalog) setDuplicateDecision(ctx context.Context, refs []Ref, userID int64, ignored bool) error {
	refs = slices.Clone(refs)
	sortComparison(refs)
	if len(refs) < 2 || (len(refs) > 2 && refs[0].LibraryID != refs[len(refs)-1].LibraryID) || len(slices.Compact(refs)) != len(refs) {
		return ErrDuplicateSelection
	}
	return c.db.WithTx(ctx, "DuplicateDecision", func(tx *sql.Tx) error {
		return storeDecision(ctx, tx, refs, ignored, sql.NullInt64{Int64: userID, Valid: userID != 0}, c.ts())
	})
}

// Large in-library groups use their membership digest and one member instead
// of splitting the decision across requests. An Undo can also find the saved
// selection after a rescan has changed the live group. A missing selection
// returns no refs, so deleting a decision can be retried.
func (c *Catalog) DuplicateSelection(ctx context.Context, key string, anchor Ref) ([]Ref, error) {
	decisions, err := queryRows(ctx, c.db, scanDecision, `SELECT `+decisionCols+` FROM duplicate_decisions
		WHERE library_id=? AND other_library_id=?`, anchor.LibraryID, anchor.LibraryID)
	if err != nil {
		return nil, err
	}
	for _, d := range decisions {
		refs, err := d.refs()
		if err != nil {
			return nil, err
		}
		if comparisonKey(refs) == key {
			if !slices.Contains(refs, anchor) {
				return nil, ErrDuplicateSelection
			}
			return refs, nil
		}
	}
	rows, sets, err := c.duplicateSets(ctx, anchor.LibraryID, false)
	if err != nil {
		return nil, err
	}
	for _, s := range sets {
		if s.key != key {
			continue
		}
		refs := make([]Ref, len(s.members))
		for i, m := range s.members {
			refs[i] = Ref{rows[m].libraryID, rows[m].path}
		}
		if !slices.Contains(refs, anchor) {
			return nil, ErrDuplicateSelection
		}
		return refs, nil
	}
	return nil, nil
}

// Moves and joins carry both endpoints. A join that collapses a group to a
// single path settles that comparison.
func moveDuplicateDecisions(ctx context.Context, tx *sql.Tx, lib int64, oldPath, newPath string) error {
	rows, err := queryRows(ctx, tx, scanDecision, `SELECT `+decisionCols+` FROM duplicate_decisions d
		WHERE (library_id=?1 OR other_library_id=?1) AND EXISTS (SELECT 1 FROM json_each(d.paths)
			WHERE value=?2 AND CASE WHEN key=0 THEN library_id ELSE other_library_id END=?1)`, lib, oldPath)
	if err != nil {
		return err
	}
	for _, d := range rows {
		refs, err := d.refs()
		if err != nil {
			return err
		}
		for i, r := range refs {
			if r.LibraryID == lib && r.Path == oldPath {
				refs[i].Path = newPath
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM duplicate_decisions WHERE library_id=? AND other_library_id=? AND paths=?`, d.first, d.last, d.paths); err != nil {
			return err
		}
		sortComparison(refs)
		if refs = slices.Compact(refs); len(refs) > 1 {
			if err := storeDecision(ctx, tx, refs, d.ignored, d.by, d.at); err != nil {
				return err
			}
		}
	}
	return nil
}

// MapLegacyDuplicateIgnores completes 0092 before the first scan. Recover the
// exact groups the old index hid, including groups the console sent in chunks.
// Deleting the migration marker commits with the decisions, so a failed start
// retries the same index snapshot.
func (c *Catalog) MapLegacyDuplicateIgnores(ctx context.Context) error {
	const marker = "map_duplicate_ignores_0092"
	var pending bool
	if err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM server_state WHERE key=?)`, marker).Scan(&pending); err != nil || !pending {
		return err
	}
	return c.db.WithTx(ctx, "MapLegacyDuplicateIgnores", func(tx *sql.Tx) error {
		rows, sets, err := findDuplicateSets(ctx, tx, 0, true)
		if err != nil {
			return err
		}
		// The old across view joined a mixed component transitively. Its pairs
		// were hidden only when every book in that component had a legacy flag.
		components := newDisjointSet(len(rows))
		for _, s := range sets {
			for _, m := range s.members[1:] {
				components.union(s.members[0], m)
			}
		}
		open := map[int]bool{}
		for i, r := range rows {
			if r.legacyIgnore == "" {
				open[components.find(i)] = true
			}
		}
		mapped := map[Ref]bool{}
		for _, s := range sets {
			first := rows[s.members[0]]
			cross := rows[s.members[1]].libraryID != first.libraryID
			if cross && open[components.find(s.members[0])] {
				continue
			}
			refs := make([]Ref, 0, len(s.members))
			for _, m := range s.members {
				r := rows[m]
				if r.legacyIgnore == "" {
					break
				}
				refs = append(refs, Ref{r.libraryID, r.path})
			}
			if len(refs) != len(s.members) {
				continue
			}
			sortComparison(refs)
			if err := storeDecision(ctx, tx, refs, true, sql.NullInt64{}, c.ts()); err != nil {
				return err
			}
			for _, r := range refs {
				mapped[r] = true
			}
		}
		for r := range mapped {
			if _, err := tx.ExecContext(ctx, `UPDATE issue_ignores SET kind='duplicate_mapped' WHERE library_id=? AND path=? AND kind='duplicate'`, r.LibraryID, r.Path); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM server_state WHERE key=?`, marker)
		return err
	})
}

package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"strings"
)

// The re-key (server#210). A listener's rows a client stored before the book
// path rule (B2) or the spelling rule (B1) applied sit under a path no book
// has: a track of a folder book (a player before 2.4.1 saved everything there),
// another Unicode spelling of a book's path (a shared link, an API-key tool), or
// a disc folder of a joined book. RekeyPathState moves them to the book's own
// path, the place the server reads them from now, and copies every moved row
// whole into path_rekeys first, in the same transaction. Anything doubtful stays
// where it is: a listener's rows where they can't reach the book, a path a
// guard holds, a library whose folder does not answer at the start.

// rekeyStateKey is the server_state row holding the pathKeysVersion the start's
// re-key last ran through under: it runs over every path again only when that
// changes.
const rekeyStateKey = "path_rekey"

// rekeyFailedKey is the server_state row naming the paths that run failed on
// (a []Ref; no row when none did): the only paths a later start looks at, so
// one path that keeps failing never costs every start the whole run.
const rekeyFailedKey = "path_rekey_failed"

// rekeyUnreachedKey is the server_state row naming the books libraries that
// run left whole because their folder did not answer (a []int64; no row when
// none): a later start runs over each of them again once it answers.
const rekeyUnreachedKey = "path_rekey_unreached"

// maxRekeyFailed is how many failed paths a run records. One with more (the
// database refusing every write, not a path's own trouble) is not recorded at
// all, and the next start runs over every path again.
const maxRekeyFailed = 200

// The reasons a path's rows move, as path_rekeys.reason records them.
const (
	// rekeySpelling: another Unicode spelling of an indexed book's path; every
	// row moves, favourites included. Also the rows another spelling of a
	// track or folder keeps (rekeyMoves): they move to its index spelling.
	rekeySpelling = "spelling"
	// rekeyFile: a file of a folder book (any spelling); every row but the
	// favourites moves (a track row's heart stays on the track, in the index's
	// spelling, server#210).
	rekeyFile = "file"
	// rekeyFolder: a folder inside a book (a disc folder of a joined book); only
	// the rows about the book as a whole move (lists, ratings, the reader's
	// place: read at BookPathAt), the timeline rows keep their own key, in the
	// index's spelling (a disc download plays the disc's own timeline).
	rekeyFolder = "folder"
)

// The guards that leave a path's rows where they are, as a report names them
// (scope: only the rows of the listeners it names).
const (
	// RekeyGuardScope: a listener with a row on the path can't reach the book
	// (a grant on the track only, access since removed), so their rows would
	// vanish from their view: theirs stay (the other listeners' move), each
	// logged once in path_rekeys with this reason.
	RekeyGuardScope = "scope"
	// RekeyGuardTimeline: a progress row on a track reads as a place in that
	// track, not the book (a tripwire: no shipped client wrote one).
	RekeyGuardTimeline = "timeline"
)

// timelineSlack is how close (seconds) a progress row's length must be to a
// track's to read as that track's (the timeline guard).
const timelineSlack = 2.0

// rekeyTable is one table of carryListeningState as the re-key reads it: which
// carry group moves it, whether the scan-end run looks for candidates in it,
// and its candidate and row reads (spelled out per table, see
// carryListeningState on gosec G202).
type rekeyTable struct {
	name string
	set  carrySet
	// primary: a table a client writes directly; the scan-end run looks only at
	// these (the derived tables follow in the carry).
	primary bool
	// cands lists the paths with rows and no indexed book (?1: the library),
	// each once.
	cands string
	// rows reads every row on a path (?1 library, ?2 path), its listener first
	// (a collection item's: the collection's owner), then every column.
	rows string
}

// rekeyNoBook ends a candidate read: of the library's distinct paths in a
// table (alias t, read first from the table's path index, so the books lookup
// is one a path, not one a row: a listener's history holds thousands of rows a
// book), those no book is indexed at exactly.
const rekeyNoBook = ` WHERE NOT EXISTS (
    SELECT 1 FROM books b WHERE b.library_id = ?1 AND b.rel_path = t.rel_path)`

// rekeyTables is every table carryListeningState carries (add a table there ->
// add it here, so a moved row is never missing from the log).
var rekeyTables = []rekeyTable{
	{"progress", carryTimeline, true,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM progress WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM progress WHERE library_id = ?1 AND rel_path = ?2`},
	{"bookmarks", carryTimeline, true,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM bookmarks WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM bookmarks WHERE library_id = ?1 AND rel_path = ?2`},
	{"notes", carryTimeline, true,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM notes WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM notes WHERE library_id = ?1 AND rel_path = ?2`},
	{"listening_history", carryTimeline, true,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM listening_history WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM listening_history WHERE library_id = ?1 AND rel_path = ?2`},
	{"listening_sessions", carryTimeline, false,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM listening_sessions WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM listening_sessions WHERE library_id = ?1 AND rel_path = ?2`},
	{"listening_daily", carryTimeline, false,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM listening_daily WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM listening_daily WHERE library_id = ?1 AND rel_path = ?2`},
	{"listening_events", carryTimeline, false,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM listening_events WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM listening_events WHERE library_id = ?1 AND rel_path = ?2`},
	{"import_progress_prior", carryTimeline, false,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM import_progress_prior WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM import_progress_prior WHERE library_id = ?1 AND rel_path = ?2`},
	{"up_next", carryBookLevel, true,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM up_next WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM up_next WHERE library_id = ?1 AND rel_path = ?2`},
	{"collection_items", carryBookLevel, true,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM collection_items WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT c.user_id, i.* FROM collection_items i JOIN collections c ON c.id = i.collection_id
		  WHERE i.library_id = ?1 AND i.rel_path = ?2`},
	{"ratings", carryBookLevel, true,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM ratings WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM ratings WHERE library_id = ?1 AND rel_path = ?2`},
	{"reading_progress", carryBookLevel, true,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM reading_progress WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM reading_progress WHERE library_id = ?1 AND rel_path = ?2`},
	{"favourites", carryFavs, true,
		`SELECT t.rel_path FROM (SELECT DISTINCT rel_path FROM favourites WHERE library_id = ?1) t` + rekeyNoBook,
		`SELECT user_id, * FROM favourites WHERE library_id = ?1 AND rel_path = ?2`},
}

// RekeyPath is one path a re-key left where it was: a guard's (Why: the
// guard's name) or a failed move's (Why: the error).
type RekeyPath struct {
	LibraryID    int64
	Path, Target string
	Why          string
}

// RekeyReport is what a re-key did.
type RekeyReport struct {
	// Moved counts the paths moved, by reason (spelling, file, folder).
	Moved map[string]int
	// Rows is how many rows it moved, each logged in path_rekeys.
	Rows int
	// Kept is how many rows the scope guard left with their listener (each
	// logged once, reason scope).
	Kept int
	// Skipped are the paths a guard left as they were (tried again next time):
	// the timeline guard's whole, the scope guard's the rows it kept.
	Skipped []RekeyPath
	// Unreached are the books libraries the start's run left whole because
	// their folder did not answer (tried again at the next start).
	Unreached []int64
	// Failed are the paths whose move failed (nothing moved; tried again).
	Failed []RekeyPath
	// Retried are the paths an earlier start failed on and the libraries it
	// left unreached (Path ""), the only ones this start looked at
	// (RekeyPathState).
	Retried []Ref
}

func (r *RekeyReport) add(o RekeyReport) {
	for k, v := range o.Moved {
		r.moved(k, v)
	}
	r.Rows += o.Rows
	r.Kept += o.Kept
	r.Skipped = append(r.Skipped, o.Skipped...)
	r.Failed = append(r.Failed, o.Failed...)
	r.Retried = append(r.Retried, o.Retried...)
}

// moved counts paths moved for reason.
func (r *RekeyReport) moved(reason string, paths int) {
	if r.Moved == nil {
		r.Moved = map[string]int{}
	}
	r.Moved[reason] += paths
}

// RekeyPathState is the start's re-key: every books library, every table, once
// per pathKeysVersion (run it after KeyPaths: Spelling reads the keys). The
// run is recorded once it has been over every path, with the paths whose move
// failed (rekeyFailedKey); a later start under the same version tries only
// those again (Retried), until none is left. A library whose folder does not
// answer (answers false: an unmounted share, a drive asleep) is left whole,
// never re-keyed from the index alone (Spelling's look at the disk keeps a
// sibling folder of another spelling, not indexed yet, apart from the book):
// it is in the report's Unreached and recorded (rekeyUnreachedKey), and a later
// start runs over it again once it answers. answers nil counts every library's
// folder as answering. The start that first runs it
// applies migration 0084, so a before-upgrade copy of the database was taken
// just before (backup.BeforeUpgrade). progress, when not nil, is told after
// each path how many are done of how many. The error is a failure to read the
// state, the libraries or a library's candidates, or ctx's (the report then
// holds what moved before it, and nothing is recorded: the next start runs
// again); a path's own failure is in the report.
func (c *Catalog) RekeyPathState(ctx context.Context, answers func(Library) bool,
	progress func(done, total int)) (RekeyReport, error) {
	var rep RekeyReport
	if answers == nil {
		answers = func(Library) bool { return true }
	}
	v, err := getServerState[string](ctx, c.db, rekeyStateKey)
	if err != nil {
		return rep, err
	}
	var todo []rekeyWork
	if v == pathKeysVersion {
		todo, rep.Retried, rep.Unreached, err = c.rekeyRetries(ctx, answers)
		if err != nil || len(rep.Retried)+len(rep.Unreached) == 0 {
			return rep, err
		}
	} else if todo, rep.Unreached, err = c.rekeyEvery(ctx, answers); err != nil {
		return rep, err
	}
	done, total := 0, 0
	for _, w := range todo {
		total += len(w.cands)
	}
	tick := func(string, bool) {
		done++
		if progress != nil {
			progress(done, total)
		}
	}
	for _, w := range todo {
		r, err := c.rekeyPaths(ctx, w.library, w.cands, tick)
		rep.add(r) // what a run stopped part way moved is in the report too
		if err != nil {
			return rep, err
		}
	}
	return rep, c.db.WithTx(ctx, "RekeyPathState", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM server_state WHERE key IN (?, ?)`,
			rekeyFailedKey, rekeyUnreachedKey); err != nil {
			return err
		}
		if len(rep.Failed) > maxRekeyFailed {
			// Too many to be the paths' own trouble: every path again at the
			// next start.
			_, err := tx.ExecContext(ctx, `DELETE FROM server_state WHERE key = ?`, rekeyStateKey)
			return err
		}
		if len(rep.Failed) > 0 {
			failed := make([]Ref, len(rep.Failed))
			for i, f := range rep.Failed {
				failed[i] = Ref{LibraryID: f.LibraryID, Path: f.Path}
			}
			if err := putServerState(ctx, tx, rekeyFailedKey, failed, c.stamp()); err != nil {
				return err
			}
		}
		if len(rep.Unreached) > 0 {
			if err := putServerState(ctx, tx, rekeyUnreachedKey, rep.Unreached, c.stamp()); err != nil {
				return err
			}
		}
		return putServerState(ctx, tx, rekeyStateKey, pathKeysVersion, c.stamp())
	})
}

// rekeyWork is one library's paths for a run of the start's re-key, each with
// the carry groups its rows are in.
type rekeyWork struct {
	library int64
	cands   map[string]carrySet
}

// rekeyEvery is a run over every path: each books library's candidates, but
// for the libraries whose folder does not answer (unreached).
func (c *Catalog) rekeyEvery(ctx context.Context, answers func(Library) bool) ([]rekeyWork, []int64, error) {
	libs, err := c.ListLibraries(ctx)
	if err != nil {
		return nil, nil, err
	}
	var (
		todo      []rekeyWork
		unreached []int64
	)
	for _, lib := range libs {
		if lib.Kind != KindBooks {
			continue // a podcast episode is its own book; a newer server's kind is left alone
		}
		if !answers(lib) {
			unreached = append(unreached, lib.ID)
			continue
		}
		cands, err := c.rekeyCandidates(ctx, lib.ID, false)
		if err != nil {
			return nil, nil, err
		}
		todo = append(todo, rekeyWork{lib.ID, cands})
	}
	return todo, unreached, nil
}

// rekeyRetries is a run over what an earlier one left: every path of the
// libraries it found unreached (rekeyUnreachedKey) and the paths it failed on
// (rekeyFailedKey) that still have rows, in a books library still there. It
// returns the work, the paths as the report names them (a whole library's
// with Path ""), and the libraries whose folder still does not answer (their
// failed paths with them: the library's every path is run again later).
func (c *Catalog) rekeyRetries(ctx context.Context, answers func(Library) bool) ([]rekeyWork, []Ref, []int64, error) {
	failed, err := getServerState[[]Ref](ctx, c.db, rekeyFailedKey)
	if err != nil {
		return nil, nil, nil, err
	}
	libs, err := getServerState[[]int64](ctx, c.db, rekeyUnreachedKey)
	if err != nil {
		return nil, nil, nil, err
	}
	var (
		todo      []rekeyWork
		retried   []Ref
		unreached []int64
	)
	byLib := map[int64]map[string]carrySet{}
	whole := map[int64]bool{}
	seen := map[int64]bool{}
	// usable is the library to retry id in, false when it is gone (so are its
	// rows) or its folder does not answer (then it is unreached); read once a
	// library.
	usable := func(id int64) (bool, error) {
		if ok, known := seen[id]; known {
			return ok, nil
		}
		lib, err := c.GetLibrary(ctx, id)
		if errors.Is(err, ErrNotFound) || (err == nil && lib.Kind != KindBooks) {
			seen[id] = false
			return false, nil
		}
		if err != nil {
			return false, err
		}
		seen[id] = answers(*lib)
		if !seen[id] {
			unreached = append(unreached, id)
		}
		return seen[id], nil
	}
	for _, id := range libs {
		if ok, err := usable(id); err != nil {
			return nil, nil, nil, err
		} else if !ok {
			continue
		}
		cands, err := c.rekeyCandidates(ctx, id, false)
		if err != nil {
			return nil, nil, nil, err
		}
		whole[id] = true
		todo = append(todo, rekeyWork{id, cands})
		retried = append(retried, Ref{LibraryID: id})
	}
	for _, r := range failed {
		if whole[r.LibraryID] {
			continue // run with its library's every path
		}
		if ok, err := usable(r.LibraryID); err != nil {
			return nil, nil, nil, err
		} else if !ok {
			continue
		}
		set, err := c.rekeyRowsAt(ctx, r)
		if err != nil {
			return nil, nil, nil, err
		}
		if byLib[r.LibraryID] == nil {
			byLib[r.LibraryID] = map[string]carrySet{}
			todo = append(todo, rekeyWork{r.LibraryID, byLib[r.LibraryID]})
		}
		byLib[r.LibraryID][r.Path] = set // none left: nothing to move, and off the list
		retried = append(retried, r)
	}
	return todo, retried, unreached, nil
}

// rekeyRowsAt is the carry groups ref's rows are in, over every table (none
// when it has no row left): a candidate's, for a path an earlier run failed on.
func (c *Catalog) rekeyRowsAt(ctx context.Context, ref Ref) (carrySet, error) {
	has := func(t rekeyTable) (bool, error) {
		rs, err := c.db.QueryContext(ctx, t.rows, ref.LibraryID, ref.Path)
		if err != nil {
			return false, err
		}
		defer rs.Close()
		found := rs.Next()
		return found, rs.Err()
	}
	var set carrySet
	for _, t := range rekeyTables {
		if set&t.set != 0 {
			continue // its group is known to have rows
		}
		found, err := has(t)
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", t.name, err)
		}
		if found {
			set |= t.set
		}
	}
	return set, nil
}

// RekeyLibrary is the scan-end re-key of one library (a no-op for a podcast
// library): rows written while their book was not indexed yet move once it is.
// It looks for candidates in the tables clients write (rekeyTable.primary)
// and on the paths the scope guard kept rows on (rekeyCandidates); the start's
// run covers the rest. With an error (ctx's, a run stopped
// part way) the report holds what moved before it.
//
// A library with many paths no book will take (a client's stray rows: tens of
// thousands cost seconds a scan) looks again only at what may have changed: a
// candidate its last run found nothing to do for (rekeyOne's idle), with rows
// in the same tables, is passed by while the index's books and files are as
// they were then (indexPrint). Only at rekeyMemoFrom candidates or more, and
// never for a path with another Unicode spelling (Spelling looks at the disk
// for it) nor one a guard kept rows on (scope: a grant since lets them follow;
// it is never idle). In memory: a start looks at every path once.
func (c *Catalog) RekeyLibrary(ctx context.Context, libraryID int64) (RekeyReport, error) {
	lib, err := c.GetLibrary(ctx, libraryID)
	if err != nil || lib.Kind != KindBooks {
		return RekeyReport{}, err
	}
	cands, err := c.rekeyCandidates(ctx, libraryID, true)
	if err != nil {
		return RekeyReport{}, err
	}
	if len(cands) < cmp.Or(c.memoRekeyFrom, rekeyMemoFrom) {
		c.forgetRekeyMemo(libraryID)
		return c.rekeyPaths(ctx, libraryID, cands, nil)
	}
	// Read before any path is looked at: an index change during the run
	// leaves a print the next run's won't match, so it looks at every path.
	printed, err := c.indexPrint(ctx, libraryID)
	if err != nil {
		return RekeyReport{}, err
	}
	idle := map[string]carrySet{}
	c.rekeyMemosMu.Lock()
	memo, ok := c.rekeyMemos[libraryID]
	c.rekeyMemosMu.Unlock()
	if ok && memo.print == printed {
		for p, set := range cands {
			if memo.idle[p] == set {
				idle[p] = set
				delete(cands, p)
			}
		}
	}
	rep, err := c.rekeyPaths(ctx, libraryID, cands, func(p string, nothing bool) {
		if nothing && !HasOtherSpelling(p) {
			idle[p] = cands[p]
		}
	})
	c.rekeyMemosMu.Lock()
	if c.rekeyMemos == nil {
		c.rekeyMemos = map[int64]rekeyMemo{}
	}
	c.rekeyMemos[libraryID] = rekeyMemo{print: printed, idle: idle}
	c.rekeyMemosMu.Unlock()
	return rep, err
}

// forgetRekeyMemo drops a library's scan-end re-key memo (RekeyLibrary): its
// run under rekeyMemoFrom keeps none, and a deleted library's goes with it.
// A memo dropped wrongly costs only a full look at the next scan end.
func (c *Catalog) forgetRekeyMemo(libraryID int64) {
	c.rekeyMemosMu.Lock()
	delete(c.rekeyMemos, libraryID)
	c.rekeyMemosMu.Unlock()
}

// rekeyMemoFrom is how many candidates a scan-end run has before it keeps a
// memo of the idle ones (RekeyLibrary): below, looking again at each costs
// less than reading the index's print.
const rekeyMemoFrom = 500

// rekeyMemo is a library's last scan-end run: the index's print it read
// (indexPrint) and the candidates it found nothing to do for, each with the
// carry groups its rows were in.
type rekeyMemo struct {
	print uint64
	idle  map[string]carrySet
}

// indexPrint is a print of what rekeyTarget reads of a library's index: each
// book's id, path, key and whether it is a folder, and each file's book and
// path, summed as hashes (no order needed): any book or file added, removed,
// moved or re-keyed changes it.
func (c *Catalog) indexPrint(ctx context.Context, libraryID int64) (uint64, error) {
	books, err := c.printRows(ctx, `SELECT 'b', id, rel_path, COALESCE(path_key, '-'), is_folder FROM books
		WHERE library_id = ?`, libraryID)
	if err != nil {
		return 0, err
	}
	files, err := c.printRows(ctx, `SELECT 'f', f.book_id, f.rel_path FROM book_files f JOIN books b ON b.id = f.book_id
		WHERE b.library_id = ?`, libraryID)
	return books + files, err
}

// printRows is the sum of a hash of each row q reads (every column, in turn).
func (c *Catalog) printRows(ctx context.Context, q string, args ...any) (uint64, error) {
	rs, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return 0, err
	}
	vals := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range vals {
		dest[i] = &vals[i]
	}
	var sum uint64
	for rs.Next() {
		if err := rs.Scan(dest...); err != nil {
			return 0, err
		}
		h := fnv.New64a()
		for _, v := range vals {
			_, _ = h.Write(v)
			_, _ = h.Write([]byte{0})
		}
		sum += h.Sum64()
	}
	return sum, rs.Err()
}

// rekeyPaths re-keys one books library's candidate paths (each with the carry
// groups its rows are in), each in its own transaction, in path order. done,
// when not nil, runs after each, told whether it found nothing to do there
// (rekeyOne's idle). Stopped (ctx ended), it returns ctx's error and what
// moved before: the path it was on is no failure (rekeyOne).
func (c *Catalog) rekeyPaths(ctx context.Context, libraryID int64, cands map[string]carrySet,
	done func(p string, idle bool)) (RekeyReport, error) {
	var rep RekeyReport
	paths := make([]string, 0, len(cands))
	for p := range cands {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	reach := map[int64][]Scope{} // each listener's access, read once a run (outOfReach)
	for _, p := range paths {
		// A run stopped part way (the server stopping) leaves the rest for the
		// next one, rather than a failure a path.
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		idle := c.rekeyOne(ctx, libraryID, p, cands[p], reach, &rep)
		if done != nil {
			done(p, idle)
		}
	}
	return rep, ctx.Err()
}

// rekeyOne re-keys the candidate path p, whose rows are in the carry groups
// has, into rep: moved, kept by a guard, failed, or nothing to do. idle is the
// last, from the index alone (p, has and the books and files it reads): with
// those as they were, it would find nothing to do again.
func (c *Catalog) rekeyOne(ctx context.Context, libraryID int64, p string, has carrySet, reach map[int64][]Scope,
	rep *RekeyReport) (idle bool) {
	if c.rekeyLooked != nil {
		c.rekeyLooked(p)
	}
	if has == 0 || (has == carryFavs && !HasOtherSpelling(p)) {
		return true // no row left; or favourites, which move only to another spelling, and p has none
	}
	reason, spelled, target, err := c.rekeyTarget(ctx, libraryID, p)
	if err != nil {
		if ctx.Err() == nil { // stopped (the server stopping): the path is no failure, the next run takes it
			rep.Failed = append(rep.Failed, RekeyPath{LibraryID: libraryID, Path: p, Why: err.Error()})
		}
		return false
	}
	moves := rekeyMoves(p, reason, spelled, target)
	var set carrySet
	for _, mv := range moves {
		set |= mv.set
	}
	if set&has == 0 {
		return true // nothing its moves carry (no target, or a track's favourites only)
	}
	res, err := c.rekeyPath(ctx, libraryID, p, spelled, target, reason, moves, reach)
	at := RekeyPath{LibraryID: libraryID, Path: p, Target: moves[0].to}
	switch {
	case err != nil && ctx.Err() != nil:
		return false // stopped: its transaction rolled back, the next run takes it
	case err != nil:
		at.Why = err.Error()
		rep.Failed = append(rep.Failed, at)
		return false
	case res.guard != "":
		at.Why = res.guard
		rep.Skipped = append(rep.Skipped, at)
		return false
	}
	if res.moved > 0 {
		rep.moved(res.via, 1)
		rep.Rows += res.moved
	}
	if res.kept > 0 {
		at.Why = RekeyGuardScope
		rep.Skipped = append(rep.Skipped, at)
		rep.Kept += res.kept
	}
	return false
}

// rekeyMove is one of a path's moves: its rows in the carry groups set go to
// the path to, logged with reason.
type rekeyMove struct {
	reason, to string
	set        carrySet
}

// rekeyMoves are p's moves: the rows reason moves (rekeySet) to the book at
// target, and, for p in another spelling than the index's (spelled), every
// other row to spelled, the same file or folder in the index's spelling (a
// track row's heart, a disc folder's timeline rows, a navigation folder's
// favourite), so no row stays on a spelling every read now passes by. None
// when p is no book's and has no other spelling.
func rekeyMoves(p, reason, spelled, target string) []rekeyMove {
	var out []rekeyMove
	set := rekeySet(reason)
	if set != 0 {
		out = append(out, rekeyMove{reason: reason, to: target, set: set})
	}
	if rest := carryAll &^ set; spelled != p && rest != 0 {
		out = append(out, rekeyMove{reason: rekeySpelling, to: spelled, set: rest})
	}
	return out
}

// rekeySet is the carry groups a reason moves ("" moves none).
func rekeySet(reason string) carrySet {
	switch reason {
	case rekeySpelling:
		return carryAll
	case rekeyFile:
		return carryTimeline | carryBookLevel
	case rekeyFolder:
		return carryBookLevel
	}
	return 0
}

// rekeyCandidates are the library's paths with rows and no indexed book, each
// with the carry groups its rows are in. A path no indexed book lies above, in
// a spelling with no other, can be no book's (an orphan of a removed book, a
// navigation folder's favourite): heldRefs leaves it out without its own
// lookups, as listableRefs does.
func (c *Catalog) rekeyCandidates(ctx context.Context, libraryID int64, primaryOnly bool) (map[string]carrySet, error) {
	cands := map[string]carrySet{}
	for _, t := range rekeyTables {
		if primaryOnly && !t.primary {
			continue
		}
		paths, err := queryRows(ctx, c.db, func(r *sql.Rows, p *string) error { return r.Scan(p) }, t.cands, libraryID)
		if err != nil {
			return nil, fmt.Errorf("re-key candidates in %s: %w", t.name, err)
		}
		for _, p := range paths {
			cands[p] |= t.set
		}
	}
	if primaryOnly {
		// A path the scope guard kept a listener's rows on (logged, reason
		// scope) is looked at again too, in every table: once the other
		// listeners' rows moved, the kept ones may be only in a derived table
		// (a session, a day's total), and a grant since lets them follow.
		kept, err := queryRows(ctx, c.db, func(r *sql.Rows, p *string) error { return r.Scan(p) },
			`SELECT DISTINCT k.from_path FROM path_rekeys k WHERE k.library_id = ?1 AND k.reason = ?2
			   AND NOT EXISTS (SELECT 1 FROM books b WHERE b.library_id = ?1 AND b.rel_path = k.from_path)`,
			libraryID, RekeyGuardScope)
		if err != nil {
			return nil, fmt.Errorf("re-key candidates kept by scope: %w", err)
		}
		for _, p := range kept {
			if _, ok := cands[p]; ok {
				continue // rekeyPath reads every table its moves carry
			}
			set, err := c.rekeyRowsAt(ctx, Ref{LibraryID: libraryID, Path: p})
			if err != nil {
				return nil, err
			}
			if set != 0 {
				cands[p] = set
			}
		}
	}
	var plain []Ref
	for p := range cands {
		if !HasOtherSpelling(p) {
			plain = append(plain, Ref{LibraryID: libraryID, Path: p})
		}
	}
	held, err := c.heldRefs(ctx, plain)
	if err != nil {
		return nil, err
	}
	for _, r := range plain {
		if !held[r] {
			delete(cands, r.Path)
		}
	}
	return cands, nil
}

// rekeyTarget is where p's rows belong, from the index (and Spelling's one look
// at the disk, for a path with another spelling only): another spelling
// of an indexed book is that book (spelling); a file of a folder book, in any
// spelling, is the book (file); a folder inside a book is the book for its
// book-level rows (folder). reason is "" when p is none of them (a navigation
// folder, an orphan, a path two books' keys collide on: Spelling never guesses).
// spelled is p in the index's spelling (Spelling), where the rows reason
// doesn't move go (rekeyMoves).
func (c *Catalog) rekeyTarget(ctx context.Context, libraryID int64, p string) (reason, spelled, target string, err error) {
	if spelled, _, err = c.Spelling(ctx, libraryID, p, nil); err != nil { // each owner's reach is the scope guard's
		return "", "", "", err
	}
	if spelled != p {
		if _, err := bookIDByPath(ctx, c.db, libraryID, spelled); err == nil {
			return rekeySpelling, spelled, spelled, nil
		} else if !errors.Is(err, ErrNotFound) {
			return "", "", "", err
		}
	}
	// spelled is no book (a candidate, or checked above): only a book holding it.
	for _, h := range []struct {
		reason   string
		fileOnly bool
	}{{rekeyFile, true}, {rekeyFolder, false}} {
		if _, book, err := c.bookHolding(ctx, libraryID, spelled, h.fileOnly); err == nil {
			return h.reason, spelled, book, nil
		} else if !errors.Is(err, ErrNotFound) {
			return "", "", "", err
		}
	}
	return "", spelled, "", nil
}

// rekeyRow is one row a re-key moves, as path_rekeys logs it.
type rekeyRow struct {
	table string
	owner sql.NullInt64
	cols  map[string]any
}

// rekeyResult is what rekeyPath did with a path: the rows it moved (with the
// reason of the first move that had any: via), the rows it left because their
// listener can't reach where they would go (kept), or the guard that left the
// path whole (nothing written).
type rekeyResult struct {
	moved, kept int
	via, guard  string
}

// rekeyPath makes p's moves (rekeyMoves; spelled: p in the index's spelling)
// in one transaction, all or nothing: every row is copied into path_rekeys
// first, then carried as a move carries it (carryState: progress by
// mergeNewest, a list entry or favourite once, a rating or reader's place the
// newer). It checks again, inside the transaction, that p is still no book and
// that the book at target is still one holding spelled (a scan may have run
// since the candidates were read). The scope guard is per listener: the rows
// of one who can't reach where a move takes them stay on p (logged once,
// rekeyKept), the other listeners' move. via is a track in another spelling
// holding only a heart moved for its spelling, not as a file. reach caches
// each listener's access (outOfReach).
func (c *Catalog) rekeyPath(ctx context.Context, libraryID int64, p, spelled, target, reason string, moves []rekeyMove,
	reach map[int64][]Scope) (rekeyResult, error) {
	var res rekeyResult
	err := c.db.WithTx(ctx, "RekeyPathState", func(tx *sql.Tx) error {
		if _, err := bookIDByPath(ctx, tx, libraryID, p); err == nil {
			return nil // indexed since: its rows are its own
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		var total float64
		if reason != "" {
			if ok, err := bookStillHolds(ctx, tx, libraryID, target, spelled, reason, &total); err != nil || !ok {
				return err // removed or re-read since
			}
		}
		type step struct {
			rekeyMove
			rows, kept []rekeyRow
			keep       []int64
		}
		steps := make([]step, 0, len(moves))
		n := 0
		for _, mv := range moves {
			st := step{rekeyMove: mv}
			var rows []rekeyRow
			for _, t := range rekeyTables {
				if t.set&mv.set == 0 {
					continue
				}
				got, err := readRekeyRows(ctx, tx, t, libraryID, p)
				if err != nil {
					return err
				}
				rows = append(rows, got...)
			}
			out, err := c.outOfReach(ctx, tx, rows, Ref{LibraryID: libraryID, Path: mv.to}, reach)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if r.owner.Valid && out[r.owner.Int64] {
					st.kept = append(st.kept, r)
				} else {
					st.rows = append(st.rows, r)
				}
			}
			for u := range out {
				st.keep = append(st.keep, u)
			}
			// A guard returns before anything is written: the commit is a rollback's.
			if mv.reason == rekeyFile {
				if tripped, err := fileRelative(ctx, tx, libraryID, spelled, target, st.rows); err != nil {
					return err
				} else if tripped {
					res.guard = RekeyGuardTimeline
					return nil
				}
			}
			steps = append(steps, st)
			if n == 0 && len(st.rows) > 0 {
				res.via = mv.reason
			}
			n += len(st.rows)
		}
		at := c.stamp()
		for _, st := range steps {
			if err := rekeyKept(ctx, tx, at, libraryID, p, st.to, st.kept); err != nil {
				return err
			}
			res.kept += len(st.kept)
			if len(st.rows) == 0 {
				continue
			}
			for _, r := range st.rows {
				if err := logRekeyRow(ctx, tx, at, st.reason, libraryID, p, st.to, r); err != nil {
					return err
				}
			}
			// A respelling (to another spelling of p, the same file or folder)
			// keeps each row's own length: total is the book's.
			length := total
			if st.to != target {
				length = 0
			}
			if err := carryState(ctx, tx, libraryID, JoinPart{Path: p, Last: true}, st.to, length, mergeNewest, st.set,
				st.keep); err != nil {
				return err
			}
		}
		res.moved = n
		return nil
	})
	if err != nil {
		return rekeyResult{}, err
	}
	return res, nil
}

// logRekeyRow copies r, a row on from, into path_rekeys whole, with reason and
// where it goes.
func logRekeyRow(ctx context.Context, tx *sql.Tx, at, reason string, libraryID int64, from, to string, r rekeyRow) error {
	js, err := json.Marshal(r.cols)
	if err != nil {
		return fmt.Errorf("log a %s row: %w", r.table, err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO path_rekeys(at, reason, user_id, library_id, table_name, from_path, to_path, row_json)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		at, reason, r.owner, libraryID, r.table, from, to, string(js))
	return err
}

// rekeyKept logs the rows a listener keeps on from because they can't reach
// to (the scope guard, reason scope; nothing moves), once a listener, table
// and path: a scan-end run that keeps them again, or rows the listener added
// since, adds nothing, so the log never grows with the runs.
func rekeyKept(ctx context.Context, tx *sql.Tx, at string, libraryID int64, from, to string, rows []rekeyRow) error {
	type group struct {
		owner int64
		table string
	}
	logged := map[group]bool{}
	for _, r := range rows {
		g := group{r.owner.Int64, r.table}
		known, ok := logged[g]
		if !ok {
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM path_rekeys WHERE user_id = ? AND reason = ? AND library_id = ?
				     AND table_name = ? AND from_path = ? AND to_path = ?)`,
				r.owner.Int64, RekeyGuardScope, libraryID, r.table, from, to).Scan(&known); err != nil {
				return err
			}
			logged[g] = known
		}
		if known {
			continue
		}
		if err := logRekeyRow(ctx, tx, at, RekeyGuardScope, libraryID, from, to, r); err != nil {
			return err
		}
	}
	return nil
}

// bookStillHolds reports whether the book at target is still indexed and, for
// reason, still holds spelled: as one of its files (file) or a folder of them
// (folder); the book itself for spelling. total is set to its length.
func bookStillHolds(ctx context.Context, tx *sql.Tx, libraryID int64, target, spelled, reason string, total *float64) (bool, error) {
	held, args := `1`, []any{libraryID, target}
	switch reason {
	case rekeyFile:
		held = `EXISTS (SELECT 1 FROM book_files f WHERE f.book_id = b.id AND f.rel_path = ?)`
		args = append([]any{spelled}, args...)
	case rekeyFolder:
		held = `EXISTS (SELECT 1 FROM book_files f WHERE f.book_id = b.id AND f.rel_path LIKE ? ESCAPE '\')`
		args = append([]any{escapeLike(spelled) + "/%"}, args...)
	}
	var ok bool
	err := tx.QueryRowContext(ctx, `SELECT b.duration, `+held+` FROM books b WHERE b.library_id = ? AND b.rel_path = ?`,
		args...).Scan(total, &ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// readRekeyRows reads every row of t on p, each column by name.
func readRekeyRows(ctx context.Context, tx *sql.Tx, t rekeyTable, libraryID int64, p string) ([]rekeyRow, error) {
	rs, err := tx.QueryContext(ctx, t.rows, libraryID, p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.name, err)
	}
	defer rs.Close()
	names, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	var out []rekeyRow
	for rs.Next() {
		r := rekeyRow{table: t.name, cols: make(map[string]any, len(names)-1)}
		vals := make([]any, len(names)-1)
		dest := make([]any, len(names))
		dest[0] = &r.owner
		for i := range vals {
			dest[i+1] = &vals[i]
		}
		if err := rs.Scan(dest...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b) // no carried table holds a blob: a driver's text
			}
			r.cols[names[i+1]] = v
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// outOfReach is the listeners with a row in rows who can't reach ref with
// their current access (admins always can), as the book path rule asks of a
// write (api.timelinePath): a listener granted only a track, or whose access
// was removed, would otherwise lose sight of their own row. reach holds the
// access already read, by listener (one run's: their scopes, read once).
func (c *Catalog) outOfReach(ctx context.Context, tx *sql.Tx, rows []rekeyRow, ref Ref, reach map[int64][]Scope) (map[int64]bool, error) {
	out := map[int64]bool{}
	for _, r := range rows {
		if !r.owner.Valid {
			continue
		}
		scopes, ok := reach[r.owner.Int64]
		if !ok {
			var admin bool
			if err := tx.QueryRowContext(ctx, `SELECT role = 'admin' FROM users WHERE id = ?`, r.owner.Int64).
				Scan(&admin); err != nil {
				return nil, err
			}
			var err error
			if scopes, err = c.UserScopes(ctx, r.owner.Int64, admin); err != nil {
				return nil, err
			}
			reach[r.owner.Int64] = scopes
		}
		if !ScopesAllow(scopes, ref) {
			out[r.owner.Int64] = true
		}
	}
	return out, nil
}

// fileRelative is the timeline guard: whether a progress row in rows, on a
// track of book (file: its spelling in the index), reads as a place in a part
// of the book rather than the book: its length is the part's (within
// timelineSlack) and not the book's, for a part that doesn't start the book.
// A part is the track, or the folder inside the book holding it (a disc of a
// joined book, its own book before the join: a player then saved the disc's
// place on its track, which the join, carrying only the disc folder's rows,
// left there). A player always saved the whole book's place, so the track's
// should never hold; it stays as a tripwire.
func fileRelative(ctx context.Context, tx *sql.Tx, libraryID int64, file, book string, rows []rekeyRow) (bool, error) {
	var (
		bookID           int64
		fileLen, bookLen float64
		first            bool
	)
	err := tx.QueryRowContext(ctx,
		`SELECT b.id, f.duration, b.duration,
		        f.id = (SELECT id FROM book_files WHERE book_id = b.id ORDER BY seq, id LIMIT 1)
		   FROM books b JOIN book_files f ON f.book_id = b.id
		  WHERE b.library_id = ? AND b.rel_path = ? AND f.rel_path = ?`,
		libraryID, book, file).Scan(&bookID, &fileLen, &bookLen, &first)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || first {
		return false, err
	}
	parts := []float64{fileLen}
	if rest := strings.TrimPrefix(file, book+"/"); rest != file {
		if i := strings.IndexByte(rest, '/'); i > 0 {
			var (
				discLen   float64
				discFirst bool
			)
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(SUM(duration), 0),
				        COALESCE((SELECT rel_path FROM book_files WHERE book_id = ?1 ORDER BY seq, id LIMIT 1), '') LIKE ?2 ESCAPE '\'
				   FROM book_files WHERE book_id = ?1 AND rel_path LIKE ?2 ESCAPE '\'`,
				bookID, escapeLike(book+"/"+rest[:i])+"/%").Scan(&discLen, &discFirst); err != nil {
				return false, err
			}
			if !discFirst {
				parts = append(parts, discLen)
			}
		}
	}
	for _, r := range rows {
		if r.table != "progress" {
			continue
		}
		d, ok := r.cols["duration"].(float64) // REAL NOT NULL: always a float
		if !ok || math.Abs(d-bookLen) <= timelineSlack {
			continue
		}
		for _, n := range parts {
			if n > 0 && math.Abs(d-n) <= timelineSlack {
				return true, nil
			}
		}
	}
	return false, nil
}

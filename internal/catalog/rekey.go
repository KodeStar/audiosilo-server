package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
)

// The re-key (server#210). A listener's rows a client stored before the book
// path rule (B2) or the spelling rule (B1) applied sit under a path no book
// has: a track of a folder book (a player before 2.4.1 saved everything there),
// another Unicode spelling of a book's path (a shared link, an API-key tool), or
// a disc folder of a joined book. RekeyPathState moves them to the book's own
// path, the place the server reads them from now, and copies every moved row
// whole into path_rekeys first, in the same transaction. Anything doubtful stays
// where it is.

// rekeyStateKey is the server_state row holding the pathKeysVersion the start's
// re-key last ran clean under: it runs again only when that changes.
const rekeyStateKey = "path_rekey"

// The reasons a path's rows move, as path_rekeys.reason records them.
const (
	// rekeySpelling: another Unicode spelling of an indexed book's path; every
	// row moves, favourites included.
	rekeySpelling = "spelling"
	// rekeyFile: a file of a folder book (any spelling); every row but the
	// favourites moves (a track row's heart stays on the track, server#210).
	rekeyFile = "file"
	// rekeyFolder: a folder inside a book (a disc folder of a joined book); only
	// the rows about the book as a whole move (lists, ratings, the reader's
	// place: read at BookPathAt), the timeline rows keep their own key (a disc
	// download plays the disc's own timeline).
	rekeyFolder = "folder"
)

// The guards that leave a path's rows where they are, as a report names them.
const (
	// RekeyGuardScope: a listener with a row on the path can't reach the book
	// (a grant on the track only), so the row would vanish from their view.
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
	// cands lists the paths with rows and no indexed book (?1: the library).
	cands string
	// rows reads every row on a path (?1 library, ?2 path), its listener first
	// (a collection item's: the collection's owner), then every column.
	rows string
}

// rekeyNoBook is the candidate reads' condition: the library's rows (alias t)
// at a path no book is indexed at exactly.
const rekeyNoBook = ` WHERE t.library_id = ?1 AND NOT EXISTS (
    SELECT 1 FROM books b WHERE b.library_id = t.library_id AND b.rel_path = t.rel_path)`

// rekeyTables is every table carryListeningState carries (add a table there ->
// add it here, so a moved row is never missing from the log).
var rekeyTables = []rekeyTable{
	{"progress", carryTimeline, true,
		`SELECT DISTINCT t.rel_path FROM progress t` + rekeyNoBook,
		`SELECT user_id, * FROM progress WHERE library_id = ?1 AND rel_path = ?2`},
	{"bookmarks", carryTimeline, true,
		`SELECT DISTINCT t.rel_path FROM bookmarks t` + rekeyNoBook,
		`SELECT user_id, * FROM bookmarks WHERE library_id = ?1 AND rel_path = ?2`},
	{"notes", carryTimeline, true,
		`SELECT DISTINCT t.rel_path FROM notes t` + rekeyNoBook,
		`SELECT user_id, * FROM notes WHERE library_id = ?1 AND rel_path = ?2`},
	{"listening_history", carryTimeline, true,
		`SELECT DISTINCT t.rel_path FROM listening_history t` + rekeyNoBook,
		`SELECT user_id, * FROM listening_history WHERE library_id = ?1 AND rel_path = ?2`},
	{"listening_sessions", carryTimeline, false,
		`SELECT DISTINCT t.rel_path FROM listening_sessions t` + rekeyNoBook,
		`SELECT user_id, * FROM listening_sessions WHERE library_id = ?1 AND rel_path = ?2`},
	{"listening_daily", carryTimeline, false,
		`SELECT DISTINCT t.rel_path FROM listening_daily t` + rekeyNoBook,
		`SELECT user_id, * FROM listening_daily WHERE library_id = ?1 AND rel_path = ?2`},
	{"listening_events", carryTimeline, false,
		`SELECT DISTINCT t.rel_path FROM listening_events t` + rekeyNoBook,
		`SELECT user_id, * FROM listening_events WHERE library_id = ?1 AND rel_path = ?2`},
	{"import_progress_prior", carryTimeline, false,
		`SELECT DISTINCT t.rel_path FROM import_progress_prior t` + rekeyNoBook,
		`SELECT user_id, * FROM import_progress_prior WHERE library_id = ?1 AND rel_path = ?2`},
	{"up_next", carryBookLevel, true,
		`SELECT DISTINCT t.rel_path FROM up_next t` + rekeyNoBook,
		`SELECT user_id, * FROM up_next WHERE library_id = ?1 AND rel_path = ?2`},
	{"collection_items", carryBookLevel, true,
		`SELECT DISTINCT t.rel_path FROM collection_items t` + rekeyNoBook,
		`SELECT c.user_id, i.* FROM collection_items i JOIN collections c ON c.id = i.collection_id
		  WHERE i.library_id = ?1 AND i.rel_path = ?2`},
	{"ratings", carryBookLevel, true,
		`SELECT DISTINCT t.rel_path FROM ratings t` + rekeyNoBook,
		`SELECT user_id, * FROM ratings WHERE library_id = ?1 AND rel_path = ?2`},
	{"reading_progress", carryBookLevel, true,
		`SELECT DISTINCT t.rel_path FROM reading_progress t` + rekeyNoBook,
		`SELECT user_id, * FROM reading_progress WHERE library_id = ?1 AND rel_path = ?2`},
	{"favourites", carryFavs, true,
		`SELECT DISTINCT t.rel_path FROM favourites t` + rekeyNoBook,
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
	// Skipped are the paths a guard left as they were (tried again next time).
	Skipped []RekeyPath
	// Failed are the paths whose move failed (nothing moved; tried again).
	Failed []RekeyPath
}

func (r *RekeyReport) add(o RekeyReport) {
	for k, v := range o.Moved {
		r.moved(k, v)
	}
	r.Rows += o.Rows
	r.Skipped = append(r.Skipped, o.Skipped...)
	r.Failed = append(r.Failed, o.Failed...)
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
// version is recorded only when no path failed, so a failed path is tried at
// the next start. The start that first runs it applies migration 0084, so a
// before-upgrade copy of the database was taken just before (backup.BeforeUpgrade).
// The error is a failure to read the state or the libraries; a path's own
// failure is in the report.
func (c *Catalog) RekeyPathState(ctx context.Context) (RekeyReport, error) {
	var rep RekeyReport
	if v, err := getServerState[string](ctx, c.db, rekeyStateKey); err != nil || v == pathKeysVersion {
		return rep, err
	}
	libs, err := c.ListLibraries(ctx)
	if err != nil {
		return rep, err
	}
	for _, lib := range libs {
		if lib.Kind != KindBooks {
			continue // a podcast episode is its own book; a newer server's kind is left alone
		}
		r, err := c.rekeyLibrary(ctx, lib.ID, false)
		if err != nil {
			return rep, err
		}
		rep.add(r)
	}
	if len(rep.Failed) > 0 {
		return rep, nil
	}
	return rep, c.db.WithTx(ctx, "RekeyPathState", func(tx *sql.Tx) error {
		return putServerState(ctx, tx, rekeyStateKey, pathKeysVersion, c.stamp())
	})
}

// RekeyLibrary is the scan-end re-key of one library (a no-op for a podcast
// library): rows written while their book was not indexed yet move once it is.
// It looks for candidates in the tables clients write (rekeyTable.primary)
// only; the start's run covers the rest.
func (c *Catalog) RekeyLibrary(ctx context.Context, libraryID int64) (RekeyReport, error) {
	lib, err := c.GetLibrary(ctx, libraryID)
	if err != nil || lib.Kind != KindBooks {
		return RekeyReport{}, err
	}
	return c.rekeyLibrary(ctx, libraryID, true)
}

// rekeyLibrary re-keys one books library's candidate paths, each in its own
// transaction.
func (c *Catalog) rekeyLibrary(ctx context.Context, libraryID int64, primaryOnly bool) (RekeyReport, error) {
	var rep RekeyReport
	cands, err := c.rekeyCandidates(ctx, libraryID, primaryOnly)
	if err != nil || len(cands) == 0 {
		return rep, err
	}
	paths := make([]string, 0, len(cands))
	for p := range cands {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	reach := map[int64][]Scope{} // each listener's access, read once a run (ownersReach)
	for _, p := range paths {
		if cands[p] == carryFavs && !normSensitive(p) {
			continue // favourites move only to another spelling, and p has none
		}
		reason, spelled, target, err := c.rekeyTarget(ctx, libraryID, p)
		if err != nil {
			rep.Failed = append(rep.Failed, RekeyPath{LibraryID: libraryID, Path: p, Why: err.Error()})
			continue
		}
		if rekeySet(reason)&cands[p] == 0 {
			continue // nothing its reason moves (no target, or a track's favourites only)
		}
		rows, guard, err := c.rekeyPath(ctx, libraryID, p, spelled, target, reason, reach)
		at := RekeyPath{LibraryID: libraryID, Path: p, Target: target}
		switch {
		case err != nil:
			at.Why = err.Error()
			rep.Failed = append(rep.Failed, at)
		case guard != "":
			at.Why = guard
			rep.Skipped = append(rep.Skipped, at)
		case rows > 0:
			rep.moved(reason, 1)
			rep.Rows += rows
		}
	}
	return rep, nil
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
	var plain []Ref
	for p := range cands {
		if !normSensitive(p) {
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
// spelled is p in the index's spelling (Spelling).
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

// rekeyPath moves p's rows (spelled: p in the index's spelling) in the tables
// reason moves (rekeySet) to the book at target in one transaction, all or
// nothing: every row is copied
// into path_rekeys first, then carried as a move carries it (carryState:
// progress by mergeNewest, a list entry or favourite once, a rating or
// reader's place the newer). It checks again, inside the transaction, that
// from is still no book and to still one (a scan may have run since the
// candidates were read), and returns the rows moved, or the guard that left
// them (nothing written). reach caches each listener's access for ownersReach.
func (c *Catalog) rekeyPath(ctx context.Context, libraryID int64, p, spelled, target, reason string, reach map[int64][]Scope) (int, string, error) {
	set := rekeySet(reason)
	moved, guard := 0, ""
	err := c.db.WithTx(ctx, "RekeyPathState", func(tx *sql.Tx) error {
		if _, err := bookIDByPath(ctx, tx, libraryID, p); err == nil {
			return nil // indexed since: its rows are its own
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		var total float64
		if err := tx.QueryRowContext(ctx, `SELECT duration FROM books WHERE library_id = ? AND rel_path = ?`,
			libraryID, target).Scan(&total); errors.Is(err, sql.ErrNoRows) {
			return nil // removed since
		} else if err != nil {
			return err
		}
		var rows []rekeyRow
		for _, t := range rekeyTables {
			if t.set&set == 0 {
				continue
			}
			got, err := readRekeyRows(ctx, tx, t, libraryID, p)
			if err != nil {
				return err
			}
			rows = append(rows, got...)
		}
		if len(rows) == 0 {
			return nil
		}
		// A guard returns before anything is written: the commit is a rollback's.
		if ok, err := c.ownersReach(ctx, tx, rows, Ref{LibraryID: libraryID, Path: target}, reach); err != nil {
			return err
		} else if !ok {
			guard = RekeyGuardScope
			return nil
		}
		if reason == rekeyFile {
			if tripped, err := fileRelative(ctx, tx, libraryID, spelled, target, rows); err != nil {
				return err
			} else if tripped {
				guard = RekeyGuardTimeline
				return nil
			}
		}
		at := c.stamp()
		for _, r := range rows {
			js, err := json.Marshal(r.cols)
			if err != nil {
				return fmt.Errorf("log a %s row: %w", r.table, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO path_rekeys(at, reason, user_id, library_id, table_name, from_path, to_path, row_json)
				 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
				at, reason, r.owner, libraryID, r.table, p, target, string(js)); err != nil {
				return err
			}
		}
		if err := carryState(ctx, tx, libraryID, JoinPart{Path: p, Last: true}, target, total, mergeNewest, set); err != nil {
			return err
		}
		moved = len(rows)
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	return moved, guard, nil
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

// ownersReach reports whether every listener with a row in rows can reach ref
// with their current access (admins always can), as the book path rule asks of
// a write (api.timelinePath): a listener granted only a track would otherwise
// lose sight of their own row. reach holds the access already read, by
// listener (one run's: their scopes, read once).
func (c *Catalog) ownersReach(ctx context.Context, tx *sql.Tx, rows []rekeyRow, ref Ref, reach map[int64][]Scope) (bool, error) {
	for _, r := range rows {
		if !r.owner.Valid {
			continue
		}
		scopes, ok := reach[r.owner.Int64]
		if !ok {
			var admin bool
			if err := tx.QueryRowContext(ctx, `SELECT role = 'admin' FROM users WHERE id = ?`, r.owner.Int64).
				Scan(&admin); err != nil {
				return false, err
			}
			var err error
			if scopes, err = c.UserScopes(ctx, r.owner.Int64, admin); err != nil {
				return false, err
			}
			reach[r.owner.Int64] = scopes
		}
		if !ScopesAllow(scopes, ref) {
			return false, nil
		}
	}
	return true, nil
}

// fileRelative is the timeline guard: whether a progress row in rows, on a
// track of book (file: its spelling in the index), reads as a place in that track rather
// than the book: its length is the track's (within timelineSlack) and not the
// book's, on a track other than the book's first. A player always saved the
// whole book's place, so this should never hold; it stays as a tripwire.
func fileRelative(ctx context.Context, tx *sql.Tx, libraryID int64, file, book string, rows []rekeyRow) (bool, error) {
	var (
		fileLen, bookLen float64
		first            bool
	)
	err := tx.QueryRowContext(ctx,
		`SELECT f.duration, b.duration,
		        f.id = (SELECT id FROM book_files WHERE book_id = b.id ORDER BY seq, id LIMIT 1)
		   FROM books b JOIN book_files f ON f.book_id = b.id
		  WHERE b.library_id = ? AND b.rel_path = ? AND f.rel_path = ?`,
		libraryID, book, file).Scan(&fileLen, &bookLen, &first)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || first || fileLen <= 0 {
		return false, err
	}
	for _, r := range rows {
		if r.table != "progress" {
			continue
		}
		d, ok := r.cols["duration"].(float64) // REAL NOT NULL: always a float
		if ok && math.Abs(d-fileLen) <= timelineSlack && math.Abs(d-bookLen) > timelineSlack {
			return true, nil
		}
	}
	return false, nil
}

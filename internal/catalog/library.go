package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// ErrNotFound is returned when a library or book does not exist.
var ErrNotFound = errors.New("not found")

// ErrInvalidCursor marks a malformed pagination cursor so the transport layer
// can map it to 400 (client error) and distinguish it from an internal failure.
var ErrInvalidCursor = errors.New("invalid cursor")

// ErrNameTaken marks a create or rename that collides with an existing unique
// name (a library or share). The transport layer maps it to 409 Conflict so a
// recoverable client mistake isn't reported as an internal server error.
var ErrNameTaken = errors.New("name already taken")

// CreateLibrary inserts a library, last in the display order, and returns it
// with its assigned ID and sort order.
func (c *Catalog) CreateLibrary(ctx context.Context, lib Library) (*Library, error) {
	if lib.DefaultView == "" {
		lib.DefaultView = ViewHybrid
	}
	if err := checkMetadataSource(&lib); err != nil {
		return nil, err
	}
	if lib.Kind == "" {
		lib.Kind = KindBooks
	}
	if !KnownKind(lib.Kind) {
		return nil, ErrInvalidKind
	}
	resolved, err := c.resolveRoots(ctx, lib)
	if err != nil {
		return nil, err
	}
	var id int64
	err = c.db.WithTx(ctx, "CreateLibrary", func(tx *sql.Tx) error {
		if _, err := checkLibraryRoot(ctx, tx, lib, resolved); err != nil {
			return err
		}
		// A new library lands after every other in the display order (ADMIN-12),
		// where an order of 0 would put it among them by name.
		return tx.QueryRowContext(ctx, `INSERT INTO libraries(name,root,default_view,scan_schedule,ignore_patterns,metadata_source,created_at,kind,sort_order)
			VALUES(?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(sort_order) + 1, 0) FROM libraries)) RETURNING id, sort_order`,
			lib.Name, lib.Root, lib.DefaultView, lib.ScanSchedule, joinPatterns(lib.IgnorePatterns), lib.MetadataSource, c.ts(), lib.Kind).Scan(&id, &lib.SortOrder)
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	lib.ID = id
	return &lib, nil
}

// UpsertLibraryByName creates or updates a library keyed by name. Used to sync
// libraries declared in config into the database.
func (c *Catalog) UpsertLibraryByName(ctx context.Context, lib Library) (*Library, error) {
	existing, err := c.GetLibraryByName(ctx, lib.Name)
	if errors.Is(err, ErrNotFound) {
		return c.CreateLibrary(ctx, lib)
	}
	if err != nil {
		return nil, err
	}
	if lib.DefaultView == "" {
		lib.DefaultView = existing.DefaultView
	}
	if lib.Kind != "" && lib.Kind != existing.Kind {
		return nil, ErrKindImmutable
	}
	returnLib, _, err := c.UpdateLibrary(ctx, existing.ID, LibraryPatch{Root: lib.Root, DefaultView: lib.DefaultView})
	if err != nil {
		return nil, err
	}
	lib = *returnLib
	return &lib, nil
}

// LibraryPatch is an edit to a library: an empty string keeps a text field, a nil
// pointer keeps a setting. The scan settings are validated by the caller
// (library.ValidatePatch), the metadata source here (checkMetadataSource).
type LibraryPatch struct {
	Name, Root, DefaultView string
	Kind                    *string
	ScanSchedule            *string
	IgnorePatterns          *[]string
	MetadataSource          *string
}

// Apply writes the patch's fields onto l (a zero Library for a new one).
func (p LibraryPatch) Apply(l *Library) {
	if p.Kind != nil {
		l.Kind = *p.Kind
	}
	if p.Name != "" {
		l.Name = p.Name
	}
	if p.Root != "" {
		l.Root = p.Root
	}
	if p.DefaultView != "" {
		l.DefaultView = p.DefaultView
	}
	if p.ScanSchedule != nil {
		l.ScanSchedule = *p.ScanSchedule
	}
	if p.IgnorePatterns != nil {
		l.IgnorePatterns = *p.IgnorePatterns
	}
	if p.MetadataSource != nil {
		l.MetadataSource = *p.MetadataSource
	}
}

// LibraryChanges describes the work due after a successful library edit.
type LibraryChanges struct {
	Scan, SourceChanged, ResolvePending bool
}

// UpdateLibrary applies an edit and returns the result and the background work due. A stale
// index (a new root or new ignore rules) means the caller should
// rescan. A new metadata source is no reason to: the library's books are
// re-resolved from what the scan stored, by ResolveSources, which the edit's own
// transaction marks due (a large library takes longer than a request may,
// server#213). Whether the source changed is read inside that transaction, not
// from the earlier read: an edit that raced this one may have switched it since.
func (c *Catalog) UpdateLibrary(ctx context.Context, id int64, in LibraryPatch) (*Library, LibraryChanges, error) {
	var changes LibraryChanges
	existing, err := c.GetLibrary(ctx, id)
	if err != nil {
		return nil, LibraryChanges{}, err
	}
	if in.Kind != nil && *in.Kind != existing.Kind {
		return nil, LibraryChanges{}, ErrKindImmutable
	}
	before := *existing
	in.Apply(existing)
	if err := checkMetadataSource(existing); err != nil {
		return nil, LibraryChanges{}, err
	}
	// Only a new root is checked for overlaps: the kind never changes, so an
	// edit keeping its root (a rename, a setting, config sync's every start)
	// keeps the pairs it was stored with, and a pair stored before the check
	// knew links, case or volume roots, or made by a link since, is the warning
	// at start (RootOverlaps) rather than a refusal of every other edit. The
	// links are resolved here, before the writer, for an edit that names a new
	// root; whether the root it writes is new is read inside the transaction
	// (below), as the source is.
	var resolved map[string]string
	if existing.Root != before.Root {
		if resolved, err = c.resolveRoots(ctx, *existing); err != nil {
			return nil, LibraryChanges{}, err
		}
	}
	err = c.db.WithTx(ctx, "UpdateLibrary", func(tx *sql.Tx) error {
		var stored, storedRoot string
		if err := tx.QueryRowContext(ctx, `SELECT metadata_source, root FROM libraries WHERE id = ?`, id).
			Scan(&stored, &storedRoot); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		// Against the stored root, not the earlier read: an edit that raced
		// this one may have moved the library since, and the root this one
		// writes back (the one it read) is then a move too, checked as written.
		if existing.Root != storedRoot {
			if _, err := checkLibraryRoot(ctx, tx, *existing, resolved); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE libraries SET name = ?, root = ?, default_view = ?, scan_schedule = ?, ignore_patterns = ?,
			     metadata_source = ?
			  WHERE id = ?`,
			existing.Name, existing.Root, existing.DefaultView, existing.ScanSchedule,
			joinPatterns(existing.IgnorePatterns), existing.MetadataSource, id); err != nil {
			return err
		}
		// A podcast library's episodes come from feed.json, never from tags or
		// the path, and have no scanned snapshot: re-resolving them would blank
		// what IndexPodcastShow wrote (their descriptions among it).
		if existing.Kind == KindPodcasts {
			return nil
		}
		if existing.MetadataSource == stored {
			if in.MetadataSource != nil {
				due, err := getServerState[map[int64]int64](ctx, tx, resolveKey)
				_, changes.ResolvePending = due[id]
				return err
			}
			return nil
		}
		changes.SourceChanged = true
		return c.markResolve(ctx, tx, id)
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			return nil, LibraryChanges{}, ErrNameTaken
		}
		return nil, LibraryChanges{}, err
	}
	changes.Scan = existing.Root != before.Root || !slices.Equal(existing.IgnorePatterns, before.IgnorePatterns)
	return existing, changes, nil
}

// resolveKey is the server_state row of the libraries whose books are due a
// re-resolve (ResolveSources): each library's id and the generation of its last
// metadata source change, so a run that a later change overtook starts again.
const resolveKey = "library_resolve_due"

// resolveBatch is how many books ResolveSources re-resolves in one transaction,
// so the one writer is never held long.
const resolveBatch = 200

// markResolve marks library id's books due a re-resolve, in tx (UpdateLibrary's,
// with its new metadata source).
func (c *Catalog) markResolve(ctx context.Context, tx *sql.Tx, id int64) error {
	due, err := getServerState[map[int64]int64](ctx, tx, resolveKey)
	if err != nil {
		return err
	}
	if due == nil {
		due = map[int64]int64{}
	}
	due[id]++
	return putServerState(ctx, tx, resolveKey, due, c.stamp())
}

// ResolveSources re-resolves the effective metadata of every book in each
// library whose metadata source changed (markResolve), a batch of books a
// transaction, and returns once none is due. A library switched again while it
// runs is started again; one gone meanwhile has no books left. What is due is
// in the database, so a run a stop cut short goes on at the next start. A book
// whose stored scan doesn't decode is passed over with a read problem recorded
// (recordScannedUnreadable), so it neither holds its library due nor starts
// each retry over. A library that fails is left due and the others still run:
// the pass returns its errors rather than starting again (it would fail the
// same way).
func (c *Catalog) ResolveSources(ctx context.Context) error {
	// A library's failure is kept with the generation it failed at: one
	// switched again since starts over, and its earlier failure is no longer
	// the pass's error.
	type cursor struct {
		generation, after int64
		err               error
	}
	progress := map[int64]cursor{}
	for {
		due, err := getServerState[map[int64]int64](ctx, c.db, resolveKey)
		if err != nil {
			return err
		}
		var errs []error
		ran := false
		for lib, gen := range due {
			at := progress[lib]
			if at.generation != gen {
				at = cursor{generation: gen}
			}
			if at.err != nil {
				errs = append(errs, fmt.Errorf("library %d: %w", lib, at.err))
				continue
			}
			ran = true
			if at.after, err = c.resolveLibraryBatch(ctx, lib, gen, at.after); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				at.err = err
			}
			progress[lib] = at
		}
		if !ran {
			return errors.Join(errs...)
		}
	}
}

// resolveLibraryBatch gives this library one transaction before the others
// take their turns. A new generation starts again at the first book.
func (c *Catalog) resolveLibraryBatch(ctx context.Context, lib, gen, after int64) (int64, error) {
	var ids []int64
	done := false
	err := c.db.WithTx(ctx, "ResolveSources", func(tx *sql.Tx) error {
		due, err := getServerState[map[int64]int64](ctx, tx, resolveKey)
		if err != nil || due[lib] != gen {
			return err // a later pass picks up the new generation
		}
		ids, err = queryRows(ctx, tx, func(r *sql.Rows, id *int64) error { return r.Scan(id) },
			`SELECT id FROM books WHERE library_id = ? AND id > ? ORDER BY id LIMIT ?`, lib, after, resolveBatch)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := refreshOrSkip(ctx, tx, id); err != nil {
				return err
			}
		}
		if len(ids) < resolveBatch {
			// The last batch: the library is done, in the same transaction.
			done = true
			delete(due, lib)
			return putServerState(ctx, tx, resolveKey, due, c.stamp())
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if done {
		c.changed()
	}
	if done || len(ids) == 0 {
		return 0, nil
	}
	return ids[len(ids)-1], nil
}

// scannedUnreadableDetail is the detail of a book's read problem when its
// stored scan doesn't decode (Health: "<file> couldn't be read: <detail>").
const scannedUnreadableDetail = "what its last scan stored is damaged, so the next scan reads it again"

// recordScannedUnreadable records a read problem on book id, whose stored scan
// doesn't decode, naming its first audio file (the scanner's primaryPath): the
// next scan finds that file reads (problemCleared) and reads the whole book
// again, which writes a new snapshot and records any problem of its files
// afresh, and Health lists the book meanwhile. It goes over a problem already
// recorded (one on a later part, an empty file, data ffprobe rejects), which
// may never clear and so never bring the scan back to the book, except the
// same problem on that first file: the scan reads the book again once it opens.
func recordScannedUnreadable(ctx context.Context, tx *sql.Tx, id int64) error {
	var first string
	if err := tx.QueryRowContext(ctx,
		`SELECT CASE WHEN is_folder
		     THEN COALESCE((SELECT f.rel_path FROM book_files f WHERE f.book_id = books.id ORDER BY f.seq LIMIT 1), rel_path)
		     ELSE rel_path END
		   FROM books WHERE id = ?`, id).Scan(&first); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE books SET scan_error = ?1, scan_error_file = ?2, scan_error_detail = ?3
		  WHERE id = ?4 AND NOT (scan_error = ?1 AND scan_error_file = ?2)`,
		ProblemUnreadable, first, scannedUnreadableDetail, id)
	return err
}

// refreshOrSkip re-resolves book id (refreshEffective) or, when its stored
// scan doesn't decode, records its read problem instead
// (recordScannedUnreadable), so a batch of many books goes on past it
// (server#453). loadLayers finds that before anything of the book is written,
// so no savepoint is needed (skipUnreadable is for writes made first).
// skipped reports it.
func refreshOrSkip(ctx context.Context, tx *sql.Tx, id int64) (skipped bool, err error) {
	err = refreshEffective(ctx, tx, id)
	if !errors.Is(err, errScannedUnreadable) {
		return false, err
	}
	return true, recordScannedUnreadable(ctx, tx, id)
}

// skipSavepoint is the savepoint skipUnreadable writes one book under.
const skipSavepoint = "skip_unreadable"

// skipUnreadable runs fn, one book's writes in tx, under a savepoint: when fn
// fails because the book's stored scan doesn't decode (errScannedUnreadable,
// from loadLayers or refreshEffective), what it wrote is rolled back and the
// book's read problem recorded instead (recordScannedUnreadable, id naming the
// book), so a batch of many books goes on past it, as ResolveSources does
// (server#453): the next scan reads the book again and writes a new snapshot.
// skipped reports that. Any other error is fn's, the batch's to fail.
func skipUnreadable(ctx context.Context, tx *sql.Tx, id func() (int64, error), fn func() error) (skipped bool, err error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+skipSavepoint); err != nil {
		return false, err
	}
	ferr := fn()
	unreadable := errors.Is(ferr, errScannedUnreadable)
	if unreadable {
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO `+skipSavepoint); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE `+skipSavepoint); err != nil {
		return false, cmp.Or(ferr, err)
	}
	if !unreadable {
		return false, ferr // the caller's to handle (a batch fails on it)
	}
	bookID, err := id()
	if err != nil {
		return false, err
	}
	return true, recordScannedUnreadable(ctx, tx, bookID)
}

// joinPatterns and splitPatterns store an ignore list as one pattern per line.
func joinPatterns(p []string) string { return strings.Join(p, "\n") }

func splitPatterns(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}

// DeleteLibrary removes a library and everything indexed under it. Books cascade
// via foreign keys; their FTS rows (a standalone table, not FK-cascaded) are
// removed first via DeleteBooksNotIn with an empty keep set.
func (c *Catalog) DeleteLibrary(ctx context.Context, id int64) error {
	if _, err := c.DeleteBooksNotIn(ctx, id, nil); err != nil {
		return err
	}
	// Audible titles matched to its books lose the match (path-keyed, no FK), those
	// contested over one the contest, those whose backup into it was refused the
	// refusal (0093; the job cascades away with the library), and
	// the accounts mapped to it pause (library_id SET NULL by the FK), their open
	// polls cancelled: all with the library's own delete, so no titles call can
	// match a book of it in between. The delete takes every grant of the library
	// away (its share rules cascade), so it commits durably (DeleteShare,
	// server#260). Its scan-end re-key memo goes with it.
	defer c.forgetRekeyMemo(id)
	return c.db.WithDurableTx(ctx, "DeleteLibrary", func(tx *sql.Tx) error {
		// The library's whole-library grant shares grant nothing once it's gone
		// (their rule cascades away); drop them rather than leave empty shares.
		// One that also holds another library's rules still grants those, so it
		// stays, as an ordinary share: its whole-library rule goes with the
		// library, and a mark left on it would keep its folders closed to any
		// change (ErrLibraryGrant) while it gives no whole library.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM shares WHERE whole_library_id = ?
			    AND NOT EXISTS (SELECT 1 FROM share_paths sp
			                     WHERE sp.share_id = shares.id AND sp.library_id != ?)`, id, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE shares SET whole_library_id = NULL WHERE whole_library_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE audible_titles SET match_library_id = NULL, match_path = NULL, match_how = ''
			  WHERE match_library_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE audible_titles SET contest_library_id = NULL, contest_path = '' WHERE contest_library_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE audible_titles SET fold_refusal_reason = '', fold_refused_at = NULL
			  WHERE fold_refused_at IS NOT NULL
			    AND import_job_id IN (SELECT id FROM bridge_jobs WHERE library_id = ? AND kind = ?)`,
			id, bridge.KindAudibleImport); err != nil {
			return err
		}
		paused, err := queryRows(ctx, tx, func(rows *sql.Rows, account *string) error { return rows.Scan(account) },
			`SELECT account FROM audible_accounts WHERE library_id = ?`, id)
		if err != nil {
			return err
		}
		for _, account := range paused {
			if err := bridge.CancelAudiblePolls(ctx, tx, account, bridge.CancelAudiblePaused, c.now()); err != nil {
				return err
			}
			if err := bridge.CancelAudibleSyncs(ctx, tx, account, bridge.CancelAudiblePaused, c.now()); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM libraries WHERE id = ?`, id)
		return err
	})
}

// libraryCols is what scanLibrary reads.
const libraryCols = `id, name, root, default_view, sort_order, scan_schedule, ignore_patterns, metadata_source, kind`

func scanLibrary(row interface{ Scan(...any) error }) (*Library, error) {
	var l Library
	var patterns string
	if err := row.Scan(&l.ID, &l.Name, &l.Root, &l.DefaultView, &l.SortOrder, &l.ScanSchedule, &patterns,
		&l.MetadataSource, &l.Kind); err != nil {
		return nil, err
	}
	l.IgnorePatterns = splitPatterns(patterns)
	return &l, nil
}

// GetLibrary returns a library by ID, from the library cache (c.libs,
// librarycache.go) while no content has been written since it was read.
func (c *Catalog) GetLibrary(ctx context.Context, id int64) (*Library, error) {
	gen, cacheable := c.ContentGeneration() // before the read (store.GenMap)
	if cacheable {
		if l, ok := c.libs.Get(gen, id); ok {
			return copyLibrary(l), nil
		}
	}
	row := c.db.QueryRowContext(ctx,
		`SELECT `+libraryCols+` FROM libraries WHERE id = ?`, id)
	l, err := scanLibrary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err == nil && cacheable {
		c.libs.Put(gen, id, *copyLibrary(*l))
	}
	return l, err
}

// GetLibraryByName returns a library by name.
func (c *Catalog) GetLibraryByName(ctx context.Context, name string) (*Library, error) {
	row := c.db.QueryRowContext(ctx,
		`SELECT `+libraryCols+` FROM libraries WHERE name = ?`, name)
	l, err := scanLibrary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return l, err
}

// ListLibraries returns all libraries in display order (sort_order, then name).
func (c *Catalog) ListLibraries(ctx context.Context) ([]Library, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+libraryCols+` FROM libraries ORDER BY sort_order, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectLibraries(rows)
}

// ReorderLibraries sets each library's sort_order to its position in ids (0-based)
// in one transaction; libraries not listed keep their current order. This is the
// admin "reorder libraries" control, and the order it sets is the tiebreaker used
// when de-duplicating identical copies of a book across libraries.
func (c *Catalog) ReorderLibraries(ctx context.Context, ids []int64) error {
	return c.db.WithTx(ctx, "ReorderLibraries", func(tx *sql.Tx) error {
		for i, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`UPDATE libraries SET sort_order = ? WHERE id = ?`, i, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func collectLibraries(rows *sql.Rows) ([]Library, error) {
	var out []Library
	for rows.Next() {
		l, err := scanLibrary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

const (
	KindBooks    = "books"
	KindPodcasts = "podcasts"
)

// KnownKind reports whether kind is one this server knows: a library of
// another (a newer AudioSilo's, after a rollback) is left alone (server#182).
func KnownKind(kind string) bool { return kind == KindBooks || kind == KindPodcasts }

// inBookLibrary keeps a books row (alias b) of a books-kind library only:
// podcast episodes are out of every cross-library book aggregate and the admin
// book tools. It stays a correlated EXISTS: an IN list lets the planner lead
// with the library index and slows the Health counts (TestIssueCountsPlan).
// bookLibraryIDs is the rule for a query over books unaliased.
const (
	inBookLibrary  = `EXISTS(SELECT 1 FROM libraries pl WHERE pl.id=b.library_id AND pl.kind='` + KindBooks + `')`
	bookLibraryIDs = `library_id IN (SELECT id FROM libraries WHERE kind='` + KindBooks + `')`
)

var ErrInvalidKind = errors.New("invalid library kind")
var ErrKindImmutable = errors.New("library kind is immutable")
var ErrRootOverlaps = errors.New("podcast library roots must not overlap other libraries")

// checkLibraryRoot runs with the writer held (q a writer transaction) so
// simultaneous creates cannot admit overlapping roots; CheckLibraryRoot reads it
// without. Paths are cleaned and made absolute on both sides, and compared as
// written and, where links resolved them (resolved: resolveRoots, read before
// the writer was taken), as resolved, by the platform's rule (pathOverlaps).
// sameRoot is the name of another library at exactly this root (first in the
// console's order), which is allowed between two books libraries.
func checkLibraryRoot(ctx context.Context, q rowQuerier, lib Library, resolved map[string]string) (sameRoot string, err error) {
	root, err := filepath.Abs(lib.Root)
	if err != nil {
		return "", err
	}
	rows, err := q.QueryContext(ctx, `SELECT name,root,kind FROM libraries WHERE id != ? ORDER BY sort_order, name`, lib.ID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var name, other, kind string
		if err := rows.Scan(&name, &other, &kind); err != nil {
			return "", err
		}
		other, err = filepath.Abs(other)
		if err != nil {
			return "", err
		}
		if (lib.Kind == KindPodcasts || kind == KindPodcasts) && formsOverlap(root, other, resolved) {
			return "", fmt.Errorf("%w: the library %q at %s", ErrRootOverlaps, name, other)
		}
		if other == root && sameRoot == "" {
			sameRoot = name
		}
	}
	return sameRoot, rows.Err()
}

// ListBookLibraries is for cross-library audiobook tools; podcasts have their
// own scanner and are never candidates for book matching or enrichment.
func (c *Catalog) ListBookLibraries(ctx context.Context) ([]Library, error) {
	libs, err := c.ListLibraries(ctx)
	return slices.DeleteFunc(libs, func(l Library) bool { return l.Kind != KindBooks }), err
}

var ErrNotBookLibrary = errors.New("not a books library")

package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
)

// Upcoming releases (Track U1, migration 0059): the storage of the background
// job in internal/upcoming. Every table is derived (rebuildable from the books
// and the community metadata), path-keyed with no FK to books, and written
// through the user-state path: no content cache reads any of them.

// UpcomingBook is one owned book in one of its series, as the job reads the
// library: its handle, the series (key and spelling), its position there (a
// range's end, 0 for none) and its identifiers.
type UpcomingBook struct {
	Ref
	SeriesKey   string
	SeriesName  string
	Position    float64
	PositionEnd float64
	ASIN, ISBN  string
}

// UpcomingBooks lists every book of a books library that is in a series, once
// per series it is in (its main one and each of its more_series), in library
// sort order then path. Podcast libraries are never read.
func (c *Catalog) UpcomingBooks(ctx context.Context) ([]UpcomingBook, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT b.library_id, b.rel_path, b.series, b.series_index,
		       b.series_index_end, b.more_series, b.asin, b.isbn
		  FROM books b JOIN libraries l ON l.id = b.library_id
		 WHERE l.kind = 'books' AND (b.series <> '' OR b.more_series <> '[]')
		 ORDER BY l.sort_order, l.name, l.id, b.rel_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpcomingBook
	for rows.Next() {
		var ref Ref
		var series, more, asin, isbn string
		var idx, end float64
		if err := rows.Scan(&ref.LibraryID, &ref.Path, &series, &idx, &end, &more, &asin, &isbn); err != nil {
			return nil, err
		}
		for _, s := range SeriesList(SeriesRef{Name: series, Position: idx, PositionEnd: end}, more) {
			key := seriesKey(s.Name)
			if strings.TrimSpace(s.Name) == "" || key == "" {
				continue
			}
			out = append(out, UpcomingBook{Ref: ref, SeriesKey: key, SeriesName: s.Name,
				Position: s.Position, PositionEnd: s.PositionEnd, ASIN: asin, ISBN: isbn})
		}
	}
	return out, rows.Err()
}

// UpcomingOwned is an owned book's resolved community work: IDKey is the
// identifiers it was resolved by, WorkID "" when they have no community match.
type UpcomingOwned struct {
	Ref
	IDKey     string
	WorkID    string
	CheckedAt string
}

// UpcomingOwnedAll returns every stored resolution, by book.
func (c *Catalog) UpcomingOwnedAll(ctx context.Context) (map[Ref]UpcomingOwned, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT library_id, path, id_key, work_id, checked_at FROM upcoming_owned`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Ref]UpcomingOwned{}
	for rows.Next() {
		var o UpcomingOwned
		if err := rows.Scan(&o.LibraryID, &o.Path, &o.IDKey, &o.WorkID, &o.CheckedAt); err != nil {
			return nil, err
		}
		out[o.Ref] = o
	}
	return out, rows.Err()
}

// SaveUpcomingOwned stores resolutions (each replacing the book's) and drops
// those of books no longer indexed.
func (c *Catalog) SaveUpcomingOwned(ctx context.Context, owned []UpcomingOwned) error {
	return c.db.WithUserStateTx(ctx, "SaveUpcomingOwned", func(tx *sql.Tx) error {
		for _, o := range owned {
			if _, err := tx.ExecContext(ctx, `INSERT INTO upcoming_owned(library_id, path, id_key, work_id, checked_at)
				VALUES(?, ?, ?, ?, ?)
				ON CONFLICT(library_id, path) DO UPDATE SET id_key = excluded.id_key,
				  work_id = excluded.work_id, checked_at = excluded.checked_at`,
				o.LibraryID, o.Path, o.IDKey, o.WorkID, o.CheckedAt); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM upcoming_owned WHERE NOT EXISTS
			(SELECT 1 FROM books b WHERE b.library_id = upcoming_owned.library_id AND b.rel_path = upcoming_owned.path)`)
		return err
	})
}

// UpcomingSeries is a local series (library, folded key) resolved to one
// community series (SeriesID "" records that none was found). Name is the
// local spelling, Books the series' book count when it was resolved.
type UpcomingSeries struct {
	LibraryID  int64
	SeriesKey  string
	SeriesID   string
	Name       string
	Books      int
	ResolvedAt string
	Classified bool
}

// UpcomingSeriesAll returns every resolved local series.
func (c *Catalog) UpcomingSeriesAll(ctx context.Context) ([]UpcomingSeries, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT library_id, series_key, series_id, name, books, resolved_at,
		       classified_at IS NOT NULL
		  FROM upcoming_series ORDER BY library_id, series_key, series_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpcomingSeries
	for rows.Next() {
		var s UpcomingSeries
		if err := rows.Scan(&s.LibraryID, &s.SeriesKey, &s.SeriesID, &s.Name, &s.Books, &s.ResolvedAt, &s.Classified); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetUpcomingSeries replaces a local series' community series with ids (none:
// a row recording that none was found). A series that keeps an id keeps its
// classified mark; its items for an id it no longer has are dropped.
func (c *Catalog) SetUpcomingSeries(ctx context.Context, libraryID int64, key, name string, books int, ids []string, at string) error {
	if len(ids) == 0 {
		ids = []string{""}
	}
	return c.db.WithUserStateTx(ctx, "SetUpcomingSeries", func(tx *sql.Tx) error {
		args := append([]any{libraryID, key}, anyOf(ids)...)
		in := placeholders(len(ids))
		for _, del := range []string{
			`DELETE FROM upcoming_series WHERE library_id = ? AND series_key = ? AND series_id NOT IN (` + in + `)`,
			`DELETE FROM upcoming_items WHERE library_id = ? AND series_key = ? AND series_id NOT IN (` + in + `)`,
		} {
			if _, err := tx.ExecContext(ctx, del, args...); err != nil {
				return err
			}
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `INSERT INTO upcoming_series(library_id, series_key, series_id, name, books, resolved_at)
				VALUES(?, ?, ?, ?, ?, ?)
				ON CONFLICT(library_id, series_key, series_id) DO UPDATE SET name = excluded.name,
				  books = excluded.books, resolved_at = excluded.resolved_at`,
				libraryID, key, id, name, books, at); err != nil {
				return err
			}
		}
		return nil
	})
}

// DropUpcomingSeries forgets local series (each [library id, key]) the
// library no longer holds, with their items.
func (c *Catalog) DropUpcomingSeries(ctx context.Context, gone []UpcomingSeries) error {
	if len(gone) == 0 {
		return nil
	}
	return c.db.WithUserStateTx(ctx, "DropUpcomingSeries", func(tx *sql.Tx) error {
		for _, s := range gone {
			for _, del := range []string{
				`DELETE FROM upcoming_series WHERE library_id = ? AND series_key = ?`,
				`DELETE FROM upcoming_items WHERE library_id = ? AND series_key = ?`,
			} {
				if _, err := tx.ExecContext(ctx, del, s.LibraryID, s.SeriesKey); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// MetaSeriesPerson is an author of a community series member.
type MetaSeriesPerson struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// MetaSeriesMember is one entry of a community series as series/{id} listed it.
type MetaSeriesMember struct {
	WorkID      string             `json:"work_id"`
	Position    string             `json:"position"`
	Title       string             `json:"title"`
	Authors     []MetaSeriesPerson `json:"authors"`
	CoverURL    string             `json:"cover_url,omitempty"`
	ReleaseDate string             `json:"release_date,omitempty"`
	AddedAt     string             `json:"added_at,omitempty"`
}

// MetaSeriesRecord is a community series as last fetched. Gone: the service
// answered 404 since GoneAt; its members are kept for a grace period.
type MetaSeriesRecord struct {
	ID        string
	Name      string
	Members   []MetaSeriesMember
	FetchedAt string
	Gone      bool
	GoneAt    string
}

// MetaSeriesAll returns every stored community series, by id.
func (c *Catalog) MetaSeriesAll(ctx context.Context) (map[string]MetaSeriesRecord, error) {
	out := map[string]MetaSeriesRecord{}
	if err := c.readMetaSeries(ctx, out, "", nil); err != nil {
		return nil, err
	}
	return out, nil
}

// metaSeriesOf returns the stored community series of ids, by id.
func (c *Catalog) metaSeriesOf(ctx context.Context, ids []string) (map[string]MetaSeriesRecord, error) {
	out := map[string]MetaSeriesRecord{}
	for chunk := range slices.Chunk(ids, 500) {
		if err := c.readMetaSeries(ctx, out, ` WHERE series_id IN (`+placeholders(len(chunk))+`)`, anyOf(chunk)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readMetaSeries adds the meta_series rows where (a WHERE clause, "" for all)
// selects to out.
func (c *Catalog) readMetaSeries(ctx context.Context, out map[string]MetaSeriesRecord, where string, args []any) error {
	rows, err := c.db.QueryContext(ctx, `SELECT series_id, name, members, fetched_at, state = 'gone', COALESCE(gone_at, '')
		  FROM meta_series`+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r MetaSeriesRecord
		var members string
		if err := rows.Scan(&r.ID, &r.Name, &members, &r.FetchedAt, &r.Gone, &r.GoneAt); err != nil {
			return err
		}
		if json.Unmarshal([]byte(members), &r.Members) != nil {
			r.Members = nil // unreadable: refetched as a series with no members
		}
		out[r.ID] = r
	}
	return rows.Err()
}

// SaveMetaSeries stores a series as fetched (ok again if it was gone).
func (c *Catalog) SaveMetaSeries(ctx context.Context, r MetaSeriesRecord) error {
	members, err := json.Marshal(r.Members)
	if err != nil {
		return err
	}
	if r.Members == nil {
		members = []byte("[]")
	}
	_, err = c.db.ExecUserState(ctx, `INSERT INTO meta_series(series_id, name, members, fetched_at, state, gone_at)
		VALUES(?, ?, ?, ?, 'ok', NULL)
		ON CONFLICT(series_id) DO UPDATE SET name = excluded.name, members = excluded.members,
		  fetched_at = excluded.fetched_at, state = 'ok', gone_at = NULL`,
		r.ID, r.Name, string(members), r.FetchedAt)
	return err
}

// MarkMetaSeriesGone records a 404 for id (kept from the first one: gone_at
// doesn't move), stored without members when it was never fetched.
func (c *Catalog) MarkMetaSeriesGone(ctx context.Context, id, at string) error {
	_, err := c.db.ExecUserState(ctx, `INSERT INTO meta_series(series_id, fetched_at, state, gone_at)
		VALUES(?, ?, 'gone', ?)
		ON CONFLICT(series_id) DO UPDATE SET fetched_at = excluded.fetched_at, state = 'gone',
		  gone_at = COALESCE(meta_series.gone_at, excluded.gone_at)`, id, at, at)
	return err
}

// DropMetaSeries forgets community series, the local series resolved to them
// and their items: a series gone for its grace period, or no longer anyone's.
func (c *Catalog) DropMetaSeries(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return c.db.WithUserStateTx(ctx, "DropMetaSeries", func(tx *sql.Tx) error {
		in := placeholders(len(ids))
		for _, del := range []string{
			`DELETE FROM meta_series WHERE series_id IN (` + in + `)`,
			`DELETE FROM upcoming_series WHERE series_id IN (` + in + `)`,
			`DELETE FROM upcoming_items WHERE series_id IN (` + in + `)`,
		} {
			if _, err := tx.ExecContext(ctx, del, anyOf(ids)...); err != nil {
				return err
			}
		}
		return nil
	})
}

// The upcoming item kinds.
const (
	UpcomingPreorder = "preorder"
	UpcomingReleased = "released"
	UpcomingMissing  = "missing"
)

// UpcomingItem is one work a local series lacks, as of the last run.
type UpcomingItem struct {
	LibraryID   int64
	SeriesKey   string
	WorkID      string
	SeriesID    string
	Kind        string
	ReleaseDate string
	Position    string
	FirstSeenAt string
}

// ReplaceUpcomingItems makes items a local series' items: a work it had keeps
// its first_seen_at and announced_at, a new one is first seen at at, and the
// series' other items go. With silent (the series' first classification, a
// baseline) every released item is marked announced without being announced.
// The series is marked classified.
func (c *Catalog) ReplaceUpcomingItems(ctx context.Context, libraryID int64, key string, items []UpcomingItem, silent bool, at string) error {
	return c.db.WithUserStateTx(ctx, "ReplaceUpcomingItems", func(tx *sql.Tx) error {
		works := make([]string, 0, len(items))
		for _, it := range items {
			works = append(works, it.WorkID)
			var announced any
			if silent && it.Kind == UpcomingReleased {
				announced = at
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO upcoming_items(library_id, series_key, work_id, series_id,
				  kind, release_date, position, first_seen_at, announced_at)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(library_id, series_key, work_id) DO UPDATE SET series_id = excluded.series_id,
				  kind = excluded.kind, release_date = excluded.release_date, position = excluded.position,
				  announced_at = COALESCE(upcoming_items.announced_at, excluded.announced_at)`,
				libraryID, key, it.WorkID, it.SeriesID, it.Kind, it.ReleaseDate, it.Position, at, announced); err != nil {
				return err
			}
		}
		// The items no longer found (every item, when none is); the ids go in
		// as one JSON array.
		found, err := json.Marshal(works)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM upcoming_items WHERE library_id = ? AND series_key = ?
			AND work_id NOT IN (SELECT value FROM json_each(?))`, libraryID, key, string(found)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE upcoming_series SET classified_at = COALESCE(classified_at, ?)
			WHERE library_id = ? AND series_key = ?`, at, libraryID, key)
		return err
	})
}

// UnannouncedReleases are the released items book_released hasn't told the
// admin about, in library sort order then series and position. A work some
// other row of has been announced is included with Announced set (marked
// without a second announcement).
func (c *Catalog) UnannouncedReleases(ctx context.Context) ([]UpcomingItem, map[string]bool, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT i.library_id, i.series_key, i.work_id, i.series_id, i.kind,
		       i.release_date, i.position, i.first_seen_at,
		       EXISTS (SELECT 1 FROM upcoming_items o WHERE o.work_id = i.work_id AND o.announced_at IS NOT NULL)
		  FROM upcoming_items i JOIN libraries l ON l.id = i.library_id
		 WHERE i.kind = 'released' AND i.announced_at IS NULL
		 ORDER BY l.sort_order, l.name, l.id, i.series_key, i.position, i.work_id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []UpcomingItem
	already := map[string]bool{}
	for rows.Next() {
		var it UpcomingItem
		var done bool
		if err := rows.Scan(&it.LibraryID, &it.SeriesKey, &it.WorkID, &it.SeriesID, &it.Kind,
			&it.ReleaseDate, &it.Position, &it.FirstSeenAt, &done); err != nil {
			return nil, nil, err
		}
		if done {
			already[it.WorkID] = true
		}
		out = append(out, it)
	}
	return out, already, rows.Err()
}

// MarkUpcomingAnnounced marks every released item of these works announced.
func (c *Catalog) MarkUpcomingAnnounced(ctx context.Context, workIDs []string, at string) error {
	if len(workIDs) == 0 {
		return nil
	}
	_, err := c.db.ExecUserState(ctx, `UPDATE upcoming_items SET announced_at = ?
		WHERE announced_at IS NULL AND kind = 'released' AND work_id IN (`+placeholders(len(workIDs))+`)`,
		append([]any{at}, anyOf(workIDs)...)...)
	return err
}

// UpcomingOwnedWorks reports which of workIDs an indexed book of a books
// library is: within scopes, or anywhere on the server with all.
func (c *Catalog) UpcomingOwnedWorks(ctx context.Context, scopes []Scope, all bool, workIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(workIDs) == 0 || (!all && len(scopes) == 0) {
		return out, nil
	}
	for chunk := range slices.Chunk(workIDs, 500) {
		q := `SELECT DISTINCT o.work_id FROM upcoming_owned o
			JOIN books b ON b.library_id = o.library_id AND b.rel_path = o.path
			JOIN libraries l ON l.id = o.library_id AND l.kind = 'books'
		   WHERE o.work_id IN (` + placeholders(len(chunk)) + `)`
		args := anyOf(chunk)
		if !all {
			frag, fargs := scopesFilterSQL("o.library_id", "o.path", scopes)
			q += " AND " + frag
			args = append(args, fargs...)
		}
		if err := c.collectWorkIDs(ctx, out, q, args); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// collectWorkIDs adds the work ids query q returns to out.
func (c *Catalog) collectWorkIDs(ctx context.Context, out map[string]bool, q string, args []any) error {
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		out[id] = true
	}
	return rows.Err()
}

// UpcomingEntry is an item as GET /me/upcoming reads it: the item, its local
// series' spelling, and the community series it came from.
type UpcomingEntry struct {
	UpcomingItem
	LocalName string
	Series    MetaSeriesRecord
}

// UpcomingFor lists the items of the local series the caller holds a book of
// within scopes (a series counts only through a book they can see, main series
// or more_series), in library sort order. Items whose community series was
// never fetched are left out.
func (c *Catalog) UpcomingFor(ctx context.Context, scopes []Scope) ([]UpcomingEntry, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	frag, fargs := scopesFilterSQL("b.library_id", "b.rel_path", scopes)
	args := slices.Concat(fargs, fargs)
	rows, err := c.db.QueryContext(ctx, `SELECT i.library_id, i.series_key, i.work_id, i.series_id, i.kind,
		       i.release_date, i.position, i.first_seen_at, s.name
		  FROM upcoming_items i
		  JOIN libraries l ON l.id = i.library_id AND l.kind = 'books'
		  JOIN upcoming_series s ON s.library_id = i.library_id AND s.series_key = i.series_key
		   AND s.series_id = i.series_id
		 WHERE EXISTS (SELECT 1 FROM books b WHERE b.library_id = i.library_id AND b.series_key = i.series_key
		                 AND `+frag+`)
		    OR EXISTS (SELECT 1 FROM book_more_series m JOIN books b ON b.id = m.book_id
		                WHERE m.library_id = i.library_id AND m.series_key = i.series_key AND `+frag+`)
		 ORDER BY l.sort_order, l.name, l.id, i.series_key, i.work_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpcomingEntry
	var ids []string
	seen := map[string]bool{}
	for rows.Next() {
		var e UpcomingEntry
		if err := rows.Scan(&e.LibraryID, &e.SeriesKey, &e.WorkID, &e.SeriesID, &e.Kind, &e.ReleaseDate,
			&e.Position, &e.FirstSeenAt, &e.LocalName); err != nil {
			return nil, err
		}
		if !seen[e.SeriesID] {
			seen[e.SeriesID] = true
			ids = append(ids, e.SeriesID)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	series, err := c.metaSeriesOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, e := range out {
		s, ok := series[e.SeriesID]
		if !ok {
			continue
		}
		e.Series = s
		kept = append(kept, e)
	}
	return kept, nil
}

// upcomingRunKey is the job's run status row in server_state.
const upcomingRunKey = "upcoming"

// UpcomingRun is the upcoming job's run status (server_state 'upcoming'). Times
// are fixed-width UTC milliseconds, "" for never.
type UpcomingRun struct {
	LastRunAt     string `json:"last_run_at,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	// Failures counts the runs that failed since the last success.
	Failures  int    `json:"failures,omitempty"`
	NextRunAt string `json:"next_run_at,omitempty"`
	Series    int    `json:"series"`
	Items     int    `json:"items"`
}

// UpcomingRunState reads the job's run status (the zero value before its first run).
func (c *Catalog) UpcomingRunState(ctx context.Context) (UpcomingRun, error) {
	return getServerState[UpcomingRun](ctx, c.db, upcomingRunKey)
}

// SaveUpcomingRunState records the job's run status.
func (c *Catalog) SaveUpcomingRunState(ctx context.Context, r UpcomingRun) error {
	return putServerState(ctx, c.db, upcomingRunKey, r, c.stamp())
}

// CountUpcomingItems is how many items every local series holds in all.
func (c *Catalog) CountUpcomingItems(ctx context.Context) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM upcoming_items`).Scan(&n)
	return n, err
}

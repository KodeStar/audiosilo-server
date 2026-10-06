package catalog

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The community metadata cache's persistent rows (table meta_cache, migration
// 0024): internal/meta's Store, behind its in-memory cache. The catalog only
// keeps the rows; what goes in them, when one is fresh and which are served is
// internal/meta's business (the api package adapts these methods to meta.Store,
// so neither package imports the other). Derived, rebuildable data, not user
// state: keyed by an identifier, never by a book or a user.

// MetaCacheRows is how many rows the launcher's daily retention keeps (the
// newest by write time). Each is one book's enrichment or one work, a few KiB of
// JSON, so the table stays in the tens of MiB however large the library, while
// still covering far more books than the in-memory cache's 2048 entries.
const MetaCacheRows = 20_000

// MetaCacheEntry is one persisted meta cache row.
type MetaCacheEntry struct {
	Key     string
	Version int
	Source  string
	// Payload is the cached answer's JSON; empty for a cached "no match".
	Payload []byte
	Expires time.Time
}

// GetMetaCache returns key's row, nil when there is none.
func (c *Catalog) GetMetaCache(ctx context.Context, key string) (*MetaCacheEntry, error) {
	var (
		e       MetaCacheEntry
		payload string
		expires int64
	)
	err := c.db.QueryRowContext(ctx,
		`SELECT key, version, source, payload, expires_at FROM meta_cache WHERE key = ?`, key).
		Scan(&e.Key, &e.Version, &e.Source, &payload, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.Payload = []byte(payload)
	e.Expires = time.UnixMilli(expires)
	return &e, nil
}

// PutMetaCache inserts or replaces key's row, stamped now for retention.
func (c *Catalog) PutMetaCache(ctx context.Context, e MetaCacheEntry) error {
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO meta_cache(key, version, source, payload, expires_at, stored_at) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(key) DO UPDATE SET version = excluded.version, source = excluded.source,
		     payload = excluded.payload, expires_at = excluded.expires_at, stored_at = excluded.stored_at`,
		e.Key, e.Version, e.Source, string(e.Payload), e.Expires.UnixMilli(), c.now().UnixMilli())
	return err
}

// PruneMetaCache keeps the newest keep rows (by write time) and deletes the
// rest, returning how many went. Run daily by the launcher's retention.
func (c *Catalog) PruneMetaCache(ctx context.Context, keep int) (int64, error) {
	res, err := c.db.ExecContext(ctx,
		`DELETE FROM meta_cache WHERE rowid NOT IN
		    (SELECT rowid FROM meta_cache ORDER BY stored_at DESC, rowid DESC LIMIT ?)`, max(keep, 0))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

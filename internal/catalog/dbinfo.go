package catalog

import (
	"context"
	"database/sql"
	"errors"
)

// DatabaseInfo is what Health > System shows about the database.
type DatabaseInfo struct {
	Bytes  int64  `json:"bytes"`  // pages in use x page size (the WAL is not counted)
	Schema string `json:"schema"` // the newest applied migration, e.g. "0018_sessions.sql"
}

// DatabaseInfo reports the database's size and schema version.
func (c *Catalog) DatabaseInfo(ctx context.Context) (DatabaseInfo, error) {
	var info DatabaseInfo
	err := c.db.QueryRowContext(ctx,
		`SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()`).Scan(&info.Bytes)
	if err != nil {
		return info, err
	}
	err = c.db.QueryRowContext(ctx, `SELECT name FROM schema_migrations ORDER BY name DESC LIMIT 1`).Scan(&info.Schema)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return info, err
	}
	return info, nil
}

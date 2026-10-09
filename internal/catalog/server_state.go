package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// server_state (migration 0040) holds small server-wide facts the server keeps
// for itself, one JSON value per key. These accessors take a querier/execer so a
// caller can read and write one key inside a single transaction.

// execer is what both *store.DB and *sql.Tx offer for writes.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// getServerState reads key's value as a T: the zero T when the key has no row,
// or when its value can't be read as a T (an unreadable value counts as none,
// never as the fields it got through before failing).
func getServerState[T any](ctx context.Context, q querier, key string) (T, error) {
	var v T
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM server_state WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if json.Unmarshal([]byte(raw), &v) != nil {
		var none T
		return none, nil
	}
	return v, nil
}

// putServerState stores v as key's value, stamped at (formatSessionTime).
func putServerState(ctx context.Context, x execer, key string, v any, at string) error {
	value, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = x.ExecContext(ctx,
		`INSERT INTO server_state(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(value), at)
	return err
}

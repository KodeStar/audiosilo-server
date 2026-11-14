package catalog

import (
	"context"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// server_state (migration 0040) holds small server-wide facts the server keeps
// for itself, one JSON value per key. The accessors live in store (GetState,
// PutState: internal/auth keeps a row there too); these are the catalog's names
// for them. They take a querier/execer so a caller can read and write one key
// inside a single transaction.

// execer is what both *store.DB and *sql.Tx offer for writes.
type execer = store.StateWriter

// getServerState reads key's value as a T: the zero T when the key has no row,
// or when its value can't be read as a T (an unreadable value counts as none,
// never as the fields it got through before failing).
func getServerState[T any](ctx context.Context, q querier, key string) (T, error) {
	return store.GetState[T](ctx, q, key)
}

// putServerState stores v as key's value, stamped at (formatSessionTime).
func putServerState(ctx context.Context, x execer, key string, v any, at string) error {
	return store.PutState(ctx, x, key, v, at)
}

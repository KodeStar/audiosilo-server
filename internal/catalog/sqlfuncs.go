package catalog

import (
	"database/sql/driver"
	"sync"
	"sync/atomic"

	"modernc.org/sqlite"

	"github.com/kodestar/audiosilo-server/internal/names"
)

// SQL functions the catalogue's queries call, registered once on the driver so
// every connection has them. They order and filter results only: never use one
// in a migration, view, index or generated column, or a database could no longer
// be opened by a plain sqlite3 shell (or an older AudioSilo build).
func init() {
	// name_sort(credit): the surname-first sort key of an Author/Narrator credit,
	// so a query orders by exactly what a keyset cursor holds (keySurname).
	sqlite.MustRegisterDeterministicScalarFunction("name_sort", 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			return nameSortKey(sqlText(args[0])), nil
		})
}

// sortKeys memoizes names.SortKey: an ORDER BY name_sort(...) calls it for every
// matching row of every page, and a library holds far fewer distinct credits
// than books (an edit adds at most one). It lives as long as the process, so
// past maxSortKeys entries (credits edited or rescanned away pile up) it starts
// over rather than growing without bound.
var (
	sortKeys    sync.Map
	sortKeysLen atomic.Int64
)

const maxSortKeys = 100_000

// nameSortKey is names.SortKey, memoized.
func nameSortKey(credit string) string {
	if k, ok := sortKeys.Load(credit); ok {
		return k.(string)
	}
	k := names.SortKey(credit)
	if _, loaded := sortKeys.LoadOrStore(credit, k); !loaded && sortKeysLen.Add(1) > maxSortKeys {
		sortKeys.Clear()
		sortKeysLen.Store(0)
	}
	return k
}

// sqlText reads a TEXT argument (NULL as "").
func sqlText(v driver.Value) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	}
	return ""
}

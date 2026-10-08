package catalog

import (
	"database/sql/driver"
	"slices"
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
	// credit_has(credit, name): whether name is the whole credit or exactly one
	// of the people it names (creditNames), so "Kate Reading" finds "Michael
	// Kramer, Kate Reading" (creditFilter). Exact: a spelling differing in case is
	// another name, which a merge fixes.
	sqlite.MustRegisterDeterministicScalarFunction("credit_has", 2,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			credit, name := sqlText(args[0]), sqlText(args[1])
			if credit == name || slices.Contains(creditNames(credit), name) {
				return int64(1), nil
			}
			return int64(0), nil
		})
}

// memo caches a function of a credit. A query calls these for every matching
// row, and a library holds far fewer distinct credits than books (an edit adds
// at most one). It lives as long as the process, so past maxMemo entries
// (credits edited or rescanned away pile up) it starts over rather than growing
// without bound.
type memo[V any] struct {
	m sync.Map
	n atomic.Int64
	f func(string) V
}

const maxMemo = 100_000

func (c *memo[V]) get(credit string) V {
	if v, ok := c.m.Load(credit); ok {
		return v.(V)
	}
	v := c.f(credit)
	if _, loaded := c.m.LoadOrStore(credit, v); !loaded && c.n.Add(1) > maxMemo {
		c.m.Clear()
		c.n.Store(0)
	}
	return v
}

var (
	sortKeys    = &memo[string]{f: names.SortKey}
	creditSplit = &memo[[]string]{f: splitOnce}
)

// splitOnce is names.Split with each name once: a credit naming someone twice
// ("Kate Reading & Kate Reading", a tag repeated on joining) is one book of
// theirs, not two.
func splitOnce(credit string) []string {
	var out []string
	for _, name := range names.Split(credit) {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// nameSortKey is names.SortKey, memoized.
func nameSortKey(credit string) string { return sortKeys.get(credit) }

// creditNames is the people a credit names (splitOnce), memoized (callers must
// not modify the slice).
func creditNames(credit string) []string { return creditSplit.get(credit) }

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

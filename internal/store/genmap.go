package store

import "sync"

// GenMap is a map whose entries all belong to one write generation
// (DB.Generation): a value computed from reads begun after the generation was g
// is current while it is still g, so Get answers only at the generation the
// entries were put at, Put drops a value read at an older generation than the
// map's, and a newer one empties the map first. The caller takes the
// generation before its read, never after. The zero value is ready to use.
type GenMap[K comparable, V any] struct {
	// Max is how many entries the map holds before it starts again empty (0:
	// no bound).
	Max int

	mu   sync.Mutex
	gen  uint64
	rows map[K]V
}

// Get is k's value put at gen; false when there is none or the map holds
// another generation.
func (m *GenMap[K, V]) Get(gen uint64, k K) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen != m.gen {
		var zero V
		return zero, false
	}
	v, ok := m.rows[k]
	return v, ok
}

// Put keeps v for k, read after the generation was gen.
func (m *GenMap[K, V]) Put(gen uint64, k K, v V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen < m.gen {
		return
	}
	if gen > m.gen || m.rows == nil || (m.Max > 0 && len(m.rows) >= m.Max) {
		m.gen, m.rows = gen, map[K]V{}
	}
	m.rows[k] = v
}

// Len is how many entries the map holds.
func (m *GenMap[K, V]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

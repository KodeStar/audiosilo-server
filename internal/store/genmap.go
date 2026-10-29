package store

import "sync"

// GenMap is a map whose entries all belong to one content generation
// (DB.ContentGeneration): a value computed from reads begun after the generation was g
// is current while it is still g, so Get answers only at the generation the
// entries were put at, Put drops a value read at an older generation than the
// map's, and a newer one empties the map first. The caller takes the
// generation before its read, never after. The zero value is ready to use.
//
// A caller that Deletes (a value it made stale with a write that leaves the
// generation alone) also takes Mark before its read and puts with PutMarked: a
// read that began before a Delete may hold what the Delete removed, and is
// dropped.
type GenMap[K comparable, V any] struct {
	// Max is how many entries the map holds before it starts again empty (0:
	// no bound).
	Max int

	mu   sync.Mutex
	gen  uint64
	dels uint64 // Deletes so far (Mark)
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
	m.put(gen, k, v)
}

// Mark is the map's count of Deletes, taken with the generation before a read
// whose value goes to PutMarked.
func (m *GenMap[K, V]) Mark() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dels
}

// PutMarked is Put for a value read after Mark returned mark: dropped when a
// Delete came since, which may have removed what that read saw.
func (m *GenMap[K, V]) PutMarked(gen, mark uint64, k K, v V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mark == m.dels {
		m.put(gen, k, v)
	}
}

// put is Put with mu held.
func (m *GenMap[K, V]) put(gen uint64, k K, v V) {
	if gen < m.gen {
		return
	}
	if gen > m.gen || m.rows == nil || (m.Max > 0 && len(m.rows) >= m.Max) {
		m.gen, m.rows = gen, map[K]V{}
	}
	m.rows[k] = v
}

// Delete drops k's value, whatever generation it was put at: for a value the
// caller made stale with a write that leaves the generation alone (a user-state
// write). A value read before it and put after it with PutMarked is dropped too.
func (m *GenMap[K, V]) Delete(k K) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dels++
	delete(m.rows, k)
}

// Len is how many entries the map holds.
func (m *GenMap[K, V]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

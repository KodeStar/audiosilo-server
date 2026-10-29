package store

import "testing"

// A GenMap keeps only the newest generation's values, never one read before a
// write, and starts again when full.
func TestGenMapGenerations(t *testing.T) {
	t.Parallel()
	m := GenMap[string, int]{Max: 4}
	m.Put(2, "a", 1)
	if _, ok := m.Get(1, "a"); ok {
		t.Fatal("a value answered at another generation")
	}
	m.Put(1, "b", 2)
	if _, ok := m.Get(2, "b"); ok {
		t.Fatal("an older generation's value was kept")
	}
	if v, ok := m.Get(2, "a"); !ok || v != 1 {
		t.Fatalf("Get(2, a) = %d %v", v, ok)
	}
	m.Put(3, "c", 3)
	if _, ok := m.Get(3, "a"); ok {
		t.Fatal("a newer generation kept the older values")
	}
	for i := range 10 {
		m.Put(3, string(rune('d'+i)), i)
	}
	if m.Len() > 4 {
		t.Fatalf("map holds %d values, bound 4", m.Len())
	}
}

// Delete drops one key at whatever generation, leaving the others.
func TestGenMapDelete(t *testing.T) {
	t.Parallel()
	var m GenMap[string, int]
	m.Put(4, "a", 1)
	m.Put(4, "b", 2)
	m.Delete("a")
	m.Delete("missing")
	if _, ok := m.Get(4, "a"); ok {
		t.Fatal("a deleted key still answers")
	}
	if v, ok := m.Get(4, "b"); !ok || v != 2 {
		t.Fatalf("the other key = %d, %v", v, ok)
	}
}

// A value read before a Delete and put after it is dropped (denied), at the same
// generation; one read after the Delete is kept (allowed).
func TestGenMapPutMarked(t *testing.T) {
	t.Parallel()
	var m GenMap[string, int]
	before := m.Mark() // a read begins
	m.Delete("a")      // a user-state write changed what it is reading
	m.PutMarked(5, before, "a", 1)
	if _, ok := m.Get(5, "a"); ok {
		t.Fatal("a value read before a Delete was kept")
	}
	m.PutMarked(5, m.Mark(), "a", 2)
	if v, ok := m.Get(5, "a"); !ok || v != 2 {
		t.Fatalf("a value read after the Delete = %d, %v", v, ok)
	}
}

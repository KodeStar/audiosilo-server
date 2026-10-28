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

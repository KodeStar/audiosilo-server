package ttlset

import (
	"strconv"
	"testing"
	"time"
)

// A key is remembered for the ttl from its last Add, then forgotten.
func TestLeft(t *testing.T) {
	var none *Set[string]
	if none.Left("a", time.Unix(1000, 0)) != 0 || none.Len() != 0 {
		t.Fatal("a nil set holds something")
	}
	s := New[string](time.Minute, 4)
	now := time.Unix(1000, 0)
	if left := s.Left("a", now); left != 0 {
		t.Fatalf("unknown key left = %v", left)
	}
	s.Add("a", now)
	if left := s.Left("a", now.Add(20*time.Second)); left != 40*time.Second {
		t.Fatalf("left = %v, want 40s", left)
	}
	s.Add("a", now.Add(30*time.Second)) // again: from then
	if left := s.Left("a", now.Add(time.Minute)); left != 30*time.Second {
		t.Fatalf("left after a second Add = %v, want 30s", left)
	}
	if left := s.Left("a", now.Add(90*time.Second)); left != 0 || s.Len() != 0 {
		t.Fatalf("expired: left %v, %d held; want 0 and forgotten", left, s.Len())
	}
}

// A full set forgets its expired keys before a live one, else an arbitrary
// one, and always keeps the newest; adding a key it holds evicts nothing.
func TestBound(t *testing.T) {
	const max = 8
	now := time.Unix(1000, 0)
	s := New[string](time.Minute, max)
	for i := range max - 1 {
		s.Add("old"+strconv.Itoa(i), now.Add(-2*time.Minute)) // expired by now
	}
	s.Add("live", now)
	s.Add("new", now)
	if s.Len() != 2 || s.Left("live", now) <= 0 || s.Left("new", now) <= 0 {
		t.Fatalf("after the sweep: %d held, want live and new", s.Len())
	}

	full := New[int](time.Minute, max)
	for i := range max {
		full.Add(i, now)
	}
	full.Add(3, now) // held already
	if full.Len() != max {
		t.Fatalf("re-adding a held key: %d held, want %d", full.Len(), max)
	}
	full.Add(max, now) // all live: one goes
	if full.Len() != max || full.Left(max, now) <= 0 {
		t.Fatalf("full of live keys: %d held, newest left %v; want %d and kept", full.Len(), full.Left(max, now), max)
	}
}

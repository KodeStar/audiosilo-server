// Package ttlset is a bounded set of keys each remembered for a while: the
// failures a cache or a fetcher holds back from trying again (media.FileCache's
// refused files, the community covers' failed URLs). A Set is not safe for
// concurrent use: its owner's lock guards it, so a read can be made under the
// same lock as the owner's other state (FileCache.StatusOf reads a refusal
// with the runs in flight).
package ttlset

import "time"

// Set remembers keys for ttl each, at most max at once. New makes one; a nil
// *Set holds nothing (Left and Len read it as empty), so an owner may make its
// set at its first Add.
type Set[K comparable] struct {
	ttl   time.Duration
	max   int
	until map[K]time.Time
}

// New is an empty Set that remembers a key for ttl, and at most max keys.
func New[K comparable](ttl time.Duration, max int) *Set[K] {
	return &Set[K]{ttl: ttl, max: max, until: map[K]time.Time{}}
}

// Add remembers k until ttl from now (again from now when it was already
// remembered). A full set forgets its expired keys first, else an arbitrary
// one: it stays bounded, and the newest key is always kept.
func (s *Set[K]) Add(k K, now time.Time) {
	if _, ok := s.until[k]; !ok && len(s.until) >= s.max {
		for key, until := range s.until {
			if !now.Before(until) {
				delete(s.until, key)
			}
		}
		if len(s.until) >= s.max {
			for key := range s.until {
				delete(s.until, key)
				break
			}
		}
	}
	s.until[k] = now.Add(s.ttl)
}

// Left is how long k is remembered for yet, 0 when it isn't (an expired key is
// forgotten).
func (s *Set[K]) Left(k K, now time.Time) time.Duration {
	if s == nil {
		return 0
	}
	until, ok := s.until[k]
	if !ok {
		return 0
	}
	left := until.Sub(now)
	if left <= 0 {
		delete(s.until, k)
		return 0
	}
	return left
}

// Len is how many keys are held, expired ones not yet forgotten included.
func (s *Set[K]) Len() int {
	if s == nil {
		return 0
	}
	return len(s.until)
}

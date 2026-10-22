package discovery

import (
	"container/list"
	"time"
)

// lru is a bounded least-recently-used map with an optional time to live: at
// most max entries and maxBytes of their sizes (0: no byte bound), an entry
// older than ttl (0: no expiry) reading as absent. Not safe for concurrent use:
// the Service's mutex guards every lru it holds.
type lru[V any] struct {
	max, maxBytes, used int
	ttl                 time.Duration
	now                 func() time.Time
	order               *list.List // front = most recently used
	items               map[string]*list.Element
}

type lruEntry[V any] struct {
	key     string
	val     V
	size    int
	expires time.Time
}

func newLRU[V any](maxEntries, maxBytes int, ttl time.Duration, now func() time.Time) *lru[V] {
	return &lru[V]{max: maxEntries, maxBytes: maxBytes, ttl: ttl, now: now, order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru[V]) get(key string) (V, bool) {
	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	e := el.Value.(*lruEntry[V])
	if c.ttl > 0 && !c.now().Before(e.expires) {
		c.remove(el)
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.val, true
}

// put stores val, replacing key's entry (its expiry starts again), and evicts
// the least recently used entries past the bounds. An entry larger than the
// byte bound is not stored.
func (c *lru[V]) put(key string, val V, size int) {
	if c.maxBytes > 0 && size > c.maxBytes {
		return
	}
	if el, ok := c.items[key]; ok {
		c.remove(el)
	}
	c.items[key] = c.order.PushFront(&lruEntry[V]{key: key, val: val, size: size, expires: c.now().Add(c.ttl)})
	c.used += size
	for len(c.items) > c.max || c.maxBytes > 0 && c.used > c.maxBytes {
		c.remove(c.order.Back())
	}
}

func (c *lru[V]) remove(el *list.Element) {
	e := el.Value.(*lruEntry[V])
	c.order.Remove(el)
	delete(c.items, e.key)
	c.used -= e.size
}

func (c *lru[V]) len() int { return len(c.items) }

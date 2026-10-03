package media

import (
	"container/list"
	"sync"
)

// ThumbCache is a small in-memory LRU of cover thumbnails, held in the form they
// are sent (a data: URL, so a hit costs no encoding) and bounded by the bytes it
// holds. Keys carry a version of the source art (a custom cover's timestamp, a
// file's size and modification time), so a replaced cover is a new key and the
// stale entry simply ages out. An empty value records "this source has no usable
// art" so a book without a cover isn't re-read on every page.
type ThumbCache struct {
	mu    sync.Mutex
	max   int
	used  int
	order *list.List // front = most recently used
	items map[string]*list.Element
}

type thumbEntry struct {
	key, value string
}

// entryOverhead approximates an entry's bookkeeping cost, so a cache full of
// negative (empty) entries is bounded too.
const entryOverhead = 64

// NewThumbCache returns a cache holding at most maxBytes of thumbnails.
func NewThumbCache(maxBytes int) *ThumbCache {
	return &ThumbCache{max: maxBytes, order: list.New(), items: map[string]*list.Element{}}
}

func entryCost(key, value string) int { return len(key) + len(value) + entryOverhead }

// Get returns the cached thumbnail for key ("" for "no art") and whether key was
// cached at all.
func (c *ThumbCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*thumbEntry).value, true
}

// Put stores value under key ("" = no art), evicting the least recently used
// entries to stay within the byte bound. An entry larger than the whole cache is
// not stored.
func (c *ThumbCache) Put(key, value string) {
	cost := entryCost(key, value)
	if cost > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		old := el.Value.(*thumbEntry)
		c.used += cost - entryCost(old.key, old.value)
		old.value = value
		c.order.MoveToFront(el)
	} else {
		c.items[key] = c.order.PushFront(&thumbEntry{key: key, value: value})
		c.used += cost
	}
	for c.used > c.max {
		el := c.order.Back()
		e := el.Value.(*thumbEntry)
		c.order.Remove(el)
		delete(c.items, e.key)
		c.used -= entryCost(e.key, e.value)
	}
}

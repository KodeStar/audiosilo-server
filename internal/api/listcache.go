package api

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The list cache (server#243) keeps the encoded answers of the player's book
// lists: a library's /books pages, the recent shelf and search. A grid that
// loads every page, a home screen opened again, a search typed twice: each is
// the same answer while nothing was written, and building it is most of the
// request (the books' rows, their file counts, FTS ranking, JSON).
//
// It is exact because it is keyed on the database's write generation
// (store.DB.Generation, which moves after every write has finished) as well as
// on everything the answer is computed from: the route, the raw query string
// and the caller's scopes (their access, as the catalog computed it for this
// request). The answers are pure functions of those and the database: no
// listener-specific field rides on a list book, and the scopes carry the
// client gate and private shows. The generation is read before anything else
// the request reads, so an answer is stored under a generation no newer than
// the data it was built from; a write that commits later moves the generation
// on and the entry is never served again. Every write moves it (a progress
// save, a token's last-seen touch), so the cache only ever saves work between
// writes; it never decides what is current.
const (
	listCacheBytes    = 8 << 20 // the whole cache
	listCacheMaxEntry = 1 << 20 // one answer (a 200-book page is ~80 KB)
)

type listCache struct {
	mu      sync.Mutex
	gen     uint64 // the generation the entries belong to
	size    int    // bytes held (bodies)
	order   *list.List
	entries map[[sha256.Size]byte]*list.Element
}

type listEntry struct {
	key  [sha256.Size]byte
	body []byte
}

func newListCache() *listCache {
	return &listCache{order: list.New(), entries: map[[sha256.Size]byte]*list.Element{}}
}

// at moves the cache forward to gen (dropping every entry of an older one) and
// reports whether gen is now its generation: false for a request that read an
// older generation than one already seen, which neither reads nor stores.
// Caller holds mu.
func (c *listCache) at(gen uint64) bool {
	if gen > c.gen {
		c.gen, c.size = gen, 0
		c.order.Init()
		clear(c.entries)
	}
	return gen == c.gen
}

func (c *listCache) get(gen uint64, key [sha256.Size]byte) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at(gen) {
		return nil, false
	}
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*listEntry).body, true
}

func (c *listCache) put(gen uint64, key [sha256.Size]byte, body []byte) {
	if len(body) > listCacheMaxEntry {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at(gen) {
		return
	}
	if el, ok := c.entries[key]; ok { // a concurrent miss stored it first
		c.order.MoveToFront(el)
		return
	}
	for c.size+len(body) > listCacheBytes && c.order.Len() > 0 {
		old := c.order.Remove(c.order.Back()).(*listEntry)
		delete(c.entries, old.key)
		c.size -= len(old.body)
	}
	c.entries[key] = c.order.PushFront(&listEntry{key: key, body: body})
	c.size += len(body)
}

// listCacheKey is the key of an answer to r computed for scopes: the route
// pattern, the raw query string and the scopes, hashed (the scopes of a member
// with many paths would otherwise make a long key).
func listCacheKey(r *http.Request, scopes []catalog.Scope) ([sha256.Size]byte, error) {
	b, err := json.Marshal(struct {
		Route  string
		Lib    string
		Query  string
		Scopes []catalog.Scope
	}{r.Pattern, r.PathValue("id"), r.URL.RawQuery, scopes})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// cachedList is the list cache's lookup for one of the handlers it serves: gen
// is the generation read before the request's first read (a.listGeneration).
// It answers from the cache and returns true, or returns a store func that
// encodes v exactly as writeJSON does, keeps it, and writes it.
func (a *API) cachedList(w http.ResponseWriter, r *http.Request, gen uint64, ok bool, scopes []catalog.Scope) (served bool, store func(v any)) {
	write := func(v any) { writeJSON(w, http.StatusOK, v) }
	if !ok {
		return false, write
	}
	key, err := listCacheKey(r, scopes)
	if err != nil {
		return false, write
	}
	if body, hit := a.lists.get(gen, key); hit {
		writeJSONBytes(w, body)
		return true, nil
	}
	return false, func(v any) {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(v); err != nil {
			write(v) // writeJSON's own answer to a value that won't encode
			return
		}
		a.lists.put(gen, key, buf.Bytes())
		writeJSONBytes(w, buf.Bytes())
	}
}

// writeJSONBytes writes body, an encoded 200 answer, as writeJSON would have.
func writeJSONBytes(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

package api

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The list cache (server#243) keeps the encoded answers of the player's book
// lists: a library's /books pages, the recent shelf and search. A grid that
// loads every page, a home screen opened again, a search typed twice: each is
// the same answer while nothing was written, and building it is most of the
// request (the books' rows, their file counts, FTS ranking, JSON). The admin
// console's book pages and Health summary are kept the same way (server#245).
//
// It is exact because it is keyed on the database's write generation
// (store.DB.Generation, which moves after every write has finished) as well as
// on everything the answer is computed from: the path, the raw query string
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
	// listEntryCost is what an entry holds beside its body (its list element,
	// listEntry and map slot, about 200 bytes), counted so a flood of tiny
	// answers (a search per keystroke finding nothing) stays inside the bound.
	listEntryCost = 256
)

type listCache struct {
	mu      sync.Mutex
	gen     uint64 // the generation the entries belong to
	size    int    // bytes held (entryCost of each entry)
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
		c.entries = map[[sha256.Size]byte]*list.Element{} // clear would keep the old tables
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
	cost := entryCost(body)
	for c.size+cost > listCacheBytes && c.order.Len() > 0 {
		old := c.order.Remove(c.order.Back()).(*listEntry)
		delete(c.entries, old.key)
		c.size -= entryCost(old.body)
	}
	c.entries[key] = c.order.PushFront(&listEntry{key: key, body: body})
	c.size += cost
}

// entryCost is what keeping body costs the cache: its capacity (the encoder's
// buffer, kept as it grew, may be up to twice its length) and the entry around it.
func entryCost(body []byte) int { return cap(body) + listEntryCost }

// listCacheKey is the key of an answer to r computed for scopes: the path, the
// raw query string and the scopes, hashed (the scopes of a member with many paths
// would otherwise make a long key). Written with %q and %#v, which spell out
// every byte (JSON would turn bytes that aren't UTF-8 into U+FFFD, so two scopes
// naming such paths could share a key) and every field, so a field added to
// catalog.Scope joins the key by itself.
func listCacheKey(r *http.Request, scopes []catalog.Scope) [sha256.Size]byte {
	return sha256.Sum256(fmt.Appendf(nil, "%q %q %#v", r.URL.Path, r.URL.RawQuery, scopes))
}

// cachedList is the list cache's lookup for one of the player's list handlers:
// gen is the generation read before the request's first read (a.cat.Generation).
// It answers from the cache and returns nil, or returns a write func that
// encodes v exactly as writeJSON does, keeps it when cacheable, and writes it.
func (a *API) cachedList(w http.ResponseWriter, r *http.Request, gen uint64, cacheable bool, scopes []catalog.Scope) (write func(v any)) {
	if !cacheable {
		return func(v any) { writeJSON(w, http.StatusOK, v) }
	}
	return a.cachedAnswer(w, gen, listCacheKey(r, scopes))
}

// cachedAnswer is cachedList for an answer under any key: one that names
// everything the answer is computed from besides the database (the admin
// console's Health summary keys on its kinds and the offline libraries,
// handleIssues). Every answer the cache holds is either a list's or one of
// these, keyed on its route's path, so the two never share a key.
func (a *API) cachedAnswer(w http.ResponseWriter, gen uint64, key [sha256.Size]byte) (write func(v any)) {
	if body, hit := a.lists.get(gen, key); hit {
		writeJSONBytes(w, body)
		return nil
	}
	return func(v any) {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(v); err == nil {
			a.lists.put(gen, key, buf.Bytes())
		}
		// A value that won't encode writes nothing: the empty 200 writeJSON answers.
		writeJSONBytes(w, buf.Bytes())
	}
}

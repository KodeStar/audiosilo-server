package api

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The list cache (server#243) keeps the encoded answers of the player's book
// lists: a library's /books pages, the recent shelf, search and the browse
// lists (authors, narrators, series). A grid that
// loads every page, a home screen opened again, a search typed twice: each is
// the same answer while no content was written, and building it is most of the
// request (the books' rows, their file counts, FTS ranking, JSON). The admin
// console's book pages and Health summary are kept the same way (server#245).
//
// It is exact because it is keyed on the database's content generation
// (store.DB.ContentGeneration, which moves after every write has finished but
// a user-state one: listening, a token's last-seen touch and the rest of
// store's userStateTables) as well as
// on everything the answer is computed from: the path, the raw query string
// and the caller's scopes (their access, as the catalog computed it for this
// request). The answers are pure functions of those and the database: no
// listener-specific field rides on a list book, and the scopes carry the
// client gate and private shows. The generation is read before anything else
// the request reads, so an answer is stored under a generation no newer than
// the data it was built from; a write that commits later moves the generation
// on and the entry is never served again. A user-state write leaves it, and
// none of these answers reads what such a write changes (store's
// userStateTables, checked as each one runs; CONVENTIONS.md, Content
// generation), so listening keeps them warm. The cache only ever saves work
// between content writes; it never decides what is current.
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
	gz   []byte // body gzipped (gzipBytes), nil below gzipMinSize
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

// get returns the answer stored under key and its gzip (nil for a small one).
func (c *listCache) get(gen uint64, key [sha256.Size]byte) (body, gz []byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at(gen) {
		return nil, nil, false
	}
	el, ok := c.entries[key]
	if !ok {
		return nil, nil, false
	}
	c.order.MoveToFront(el)
	e := el.Value.(*listEntry)
	return e.body, e.gz, true
}

// put keeps body and gz, its gzip, under key; both are dropped together.
func (c *listCache) put(gen uint64, key [sha256.Size]byte, body, gz []byte) {
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
	e := &listEntry{key: key, body: body, gz: gz}
	cost := e.cost()
	for c.size+cost > listCacheBytes && c.order.Len() > 0 {
		old := c.order.Remove(c.order.Back()).(*listEntry)
		delete(c.entries, old.key)
		c.size -= old.cost()
	}
	c.entries[key] = c.order.PushFront(e)
	c.size += cost
}

// cost is what keeping e costs the cache: its buffers' capacity (the encoder's
// buffer, kept as it grew, may be up to twice its length) and the entry around them.
func (e *listEntry) cost() int { return cap(e.body) + cap(e.gz) + listEntryCost }

// listCacheKey is the key of an answer to r computed for scopes and extra (what
// else the answer is computed from besides the database, such as the Health
// summary's kinds and offline libraries): the path, the raw query string, the
// scopes and extra, hashed (the scopes of a member with many paths would
// otherwise make a long key). Written with %q and %#v, which spell out every
// byte (JSON would turn bytes that aren't UTF-8 into U+FFFD, so two scopes
// naming such paths could share a key) and every field, so a field added to
// catalog.Scope joins the key by itself.
func listCacheKey(r *http.Request, scopes []catalog.Scope, extra ...any) [sha256.Size]byte {
	return sha256.Sum256(fmt.Appendf(nil, "%q %q %#v %#v", r.URL.Path, r.URL.RawQuery, scopes, extra))
}

// cachedList is the list cache's lookup for one of the handlers it serves: gen
// is the generation read before the request's first read (a.cat.ContentGeneration),
// and extra is what the answer is computed from besides the database and r
// (listCacheKey). It answers from the cache and returns nil, or returns a
// write func that encodes v exactly as writeJSON does, keeps it (and its gzip,
// compress.go) when cacheable, and writes it. A client that takes gzip
// (gzipAccepted) is sent the stored gzip, so a hit compresses nothing.
func (a *API) cachedList(w http.ResponseWriter, r *http.Request, gen uint64, cacheable bool, scopes []catalog.Scope, extra ...any) (write func(v any)) {
	if !cacheable {
		return func(v any) { writeJSON(w, http.StatusOK, v) }
	}
	key := listCacheKey(r, scopes, extra...)
	if body, gz, hit := a.lists.get(gen, key); hit {
		writeCachedJSON(w, r, body, gz)
		return nil
	}
	return func(v any) {
		var buf bytes.Buffer
		var gz []byte
		if err := json.NewEncoder(&buf).Encode(v); err == nil {
			if buf.Len() >= gzipMinSize {
				gz = gzipBytes(buf.Bytes())
			}
			a.lists.put(gen, key, buf.Bytes(), gz)
		}
		// A value that won't encode writes nothing: the empty 200 writeJSON answers.
		writeCachedJSON(w, r, buf.Bytes(), gz)
	}
}

// writeCachedJSON writes a cached 200 answer: gz, its gzip, when there is one
// and the client takes it (the compress middleware then passes it through as
// already encoded), else body as writeJSON would have.
func writeCachedJSON(w http.ResponseWriter, r *http.Request, body, gz []byte) {
	if gz == nil || !gzipAccepted(r) {
		writeJSONBytes(w, body)
		return
	}
	h := w.Header()
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Length", strconv.Itoa(len(gz)))
	writeJSONBytes(w, gz) // the middleware adds Vary, as for every JSON answer
}

package auth

import (
	"database/sql"
	"sync"
)

// maxCachedTokens bounds the token cache: past it the cache starts again empty.
const maxCachedTokens = 4096

// tokenCache holds what lookupToken read for a token, by its hash, for as long as
// the database's write generation (store.DB.Generation) stays where it was when
// the row was read. Every change that could make a token invalid (a sign-out or
// revoke, a disabled or deleted account, a changed role or membership, a token's
// own touch) is a write, which moves the generation, so the next request reads
// the row again: a revoked token is refused on the very next request. Expiry,
// which needs no write, is checked on every hit. Only a valid session, API key or
// bridge token is held: a cast token's grant and parent and a pairing token's
// one use are read each time, and nothing is held for a refusal.
type tokenCache struct {
	mu   sync.Mutex
	gen  uint64
	rows map[string]cachedToken
}

// cachedToken is a valid token's lookup: the user (its pointers its own), the
// row, and the expiry to check on each use.
type cachedToken struct {
	user    User
	row     tokenRow
	expires sql.NullString
}

// get returns the lookup cached for hash, read at gen; false when there is none
// or it was read at another generation.
func (c *tokenCache) get(gen uint64, hash string) (cachedToken, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return cachedToken{}, false
	}
	t, ok := c.rows[hash]
	return t, ok
}

// put caches a lookup read at gen (the generation taken before the read): an
// older one than the cache's is dropped, a newer one starts the cache again.
func (c *tokenCache) put(gen uint64, hash string, t cachedToken) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen < c.gen {
		return
	}
	if gen > c.gen || c.rows == nil || len(c.rows) >= maxCachedTokens {
		c.gen, c.rows = gen, map[string]cachedToken{}
	}
	c.rows[hash] = t
}

// cloneUser is u with its own copies of what its pointers point at, so a caller
// changing the user it was handed never changes the cache's.
func cloneUser(u User) User {
	if u.ManagedBy != nil {
		r := *u.ManagedBy
		u.ManagedBy = &r
	}
	if u.MemberKind != nil {
		k := *u.MemberKind
		u.MemberKind = &k
	}
	if u.MemberAccess != nil {
		a := *u.MemberAccess
		u.MemberAccess = &a
	}
	if u.MemberRule != nil {
		m := *u.MemberRule
		u.MemberRule = &m
	}
	return u
}

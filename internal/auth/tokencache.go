package auth

import (
	"database/sql"
	"time"
)

// maxCachedTokens bounds the token cache: past it the cache starts again empty.
const maxCachedTokens = 4096

// maxCachedTokenAge is how long a cached row is answered from before the token
// is read again. The content generation moves for every write through this
// process's handle, but not for another process's on the same file: reset-admin
// beside a running server (pkg/launcher ResetAdmin) ends an admin's sessions
// with a write this server's generation never sees. Reading a row this old
// again bounds how long such a write goes unseen, at one more read a minute for
// a token in use (the minute its touch already keeps, touchInterval).
const maxCachedTokenAge = time.Minute

// The token cache (Service.tokens, a store.GenMap) holds what lookupToken read
// for a token, by its hash, for as long as the database's content generation
// (store.DB.ContentGeneration) stays where it was when the row was read. Every
// change that could make a token invalid (a sign-out or revoke, a disabled or
// deleted account, a changed role or membership, a password set) is an ordinary
// write, which moves the generation, so the next request reads the row again: a
// revoked token is refused on the very next request. A token's own touch
// (last_seen, address, app) is the one user-state write to the table: it leaves
// the generation, so listening never empties the cache, and drops that token's
// row (resolve), the only one it changed; a lookup that read the row before the
// touch and puts it after the drop is dropped too (GenMap.PutMarked, with the
// mark lookupToken takes before its read). Expiry, which needs no write, is checked
// on every hit, and so is the row's age (maxCachedTokenAge: the one write the
// generation can't see is another process's). Only a valid session, API key,
// bridge or feed token is held: a cast token's grant and parent and a pairing
// token's one use are read each time, and nothing is held for a refusal.

// cachedToken is a valid token's lookup: the user (its pointers its own), the
// row, the expiry to check on each use, and when the row was read.
type cachedToken struct {
	user    User
	row     tokenRow
	expires sql.NullString
	read    time.Time
}

// fresh reports whether c's row was read under maxCachedTokenAge before now
// (one read ahead of a clock that has moved back since is not).
func (c cachedToken) fresh(now time.Time) bool {
	age := now.Sub(c.read)
	return age >= 0 && age < maxCachedTokenAge
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

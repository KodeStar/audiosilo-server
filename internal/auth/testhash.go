package auth

import "testing"

// UseCheapHashingForTests makes HashPassword use the smallest argon2id cost, for
// test binaries that create many password users: under the race detector one
// hash at the real cost takes about a quarter of a second. Call it from TestMain
// before the tests start (it is not synchronized). It panics outside a test
// binary, so it can never weaken a server's hashes: that guard is why this
// production package imports testing (no flags; it only links the package).
func UseCheapHashingForTests() {
	if !testing.Testing() {
		panic("auth.UseCheapHashingForTests called outside a test binary")
	}
	hashCost = cheapCost
}

// cheapCost is argon2id's smallest cost (memory is at least 8 KiB per thread).
var cheapCost = argonCost{time: 1, memory: 8, threads: 1}

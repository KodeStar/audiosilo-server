package auth

import (
	"fmt"
	"strings"
	"testing"
)

// TestDummyHashMatchesParams guards the login timing-equalization. Authenticate
// verifies a presented password against dummyHash for unknown and password-less
// accounts so that path does the same argon2 work as a real verify. If dummyHash's
// embedded cost params drift from the live constants (e.g. someone bumps argonTime
// without regenerating the string), the dummy verify would do less work and leak
// account existence by timing — so this test fails loudly on any mismatch.
func TestDummyHashMatchesParams(t *testing.T) {
	parts := strings.Split(dummyHash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		t.Fatalf("dummyHash is malformed: %q", dummyHash)
	}
	var mem, tm uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &tm, &threads); err != nil {
		t.Fatalf("parse dummyHash params %q: %v", parts[3], err)
	}
	if mem != argonMemory || tm != argonTime || threads != argonThreads {
		t.Fatalf("dummyHash params m=%d,t=%d,p=%d must match the argon constants m=%d,t=%d,p=%d — regenerate dummyHash",
			mem, tm, threads, argonMemory, argonTime, argonThreads)
	}

	// It must also be a well-formed hash VerifyPassword runs to completion (a clean
	// false), so the unknown-account path actually performs the argon2 work.
	ok, err := VerifyPassword("whatever-no-one-knows", dummyHash)
	if err != nil {
		t.Fatalf("dummyHash is not verifiable, so timing equalization would be skipped: %v", err)
	}
	if ok {
		t.Fatal("dummyHash must not match any known password")
	}
}

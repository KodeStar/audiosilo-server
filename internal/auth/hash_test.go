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
// account existence by timing - so this test fails loudly on any mismatch.
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
		t.Fatalf("dummyHash params m=%d,t=%d,p=%d must match the argon constants m=%d,t=%d,p=%d - regenerate dummyHash",
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

// A server hashes at the real cost; the test-only cheap cost (this package's
// TestMain sets it) still makes hashes that verify (VerifyPassword reads the cost
// from the hash).
func TestHashCost(t *testing.T) {
	saved := hashCost
	t.Cleanup(func() { hashCost = saved })
	hashCost = realCost
	full, err := HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("$m=%d,t=%d,p=%d$", argonMemory, argonTime, argonThreads); !strings.Contains(full, want) {
		t.Fatalf("hash %q lacks the real cost %s", full, want)
	}

	UseCheapHashingForTests()
	cheap, err := HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("$m=%d,t=%d,p=%d$", cheapCost.memory, cheapCost.time, cheapCost.threads); !strings.Contains(cheap, want) {
		t.Fatalf("cheap hash %q lacks the cheap cost %s", cheap, want)
	}
	for _, h := range []string{full, cheap} {
		if ok, err := VerifyPassword("pw", h); err != nil || !ok {
			t.Fatalf("VerifyPassword(%q) = %v, %v", h, ok, err)
		}
		if ok, _ := VerifyPassword("other", h); ok {
			t.Fatalf("a wrong password verified against %q", h)
		}
	}
}

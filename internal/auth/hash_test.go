package auth

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDummyHashMatchesParams guards the login timing-equalization. Authenticate
// verifies a presented password against dummyHash for unknown and password-less
// accounts so that path does the same argon2 work as a real verify. If dummyHash's
// embedded cost params drift from the live constants (e.g. someone bumps argonTime
// without regenerating the string), the dummy verify would do less work and leak
// account existence by timing - so this test fails loudly on any mismatch.
func TestDummyHashMatchesParams(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	if hashCost != cheapCost {
		t.Fatalf("hashCost = %+v, want the cheap cost TestMain sets", hashCost)
	}
	full, err := hashWithCost("pw", realCost)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("$m=%d,t=%d,p=%d$", argonMemory, argonTime, argonThreads); !strings.Contains(full, want) {
		t.Fatalf("hash %q lacks the real cost %s", full, want)
	}

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

// countingReleaser is a memoryReleaser whose release counts on a channel.
func countingReleaser(delay time.Duration) (*memoryReleaser, chan struct{}) {
	freed := make(chan struct{}, 16)
	return &memoryReleaser{delay: delay, free: func() { freed <- struct{}{} }}, freed
}

// A burst of hashes is released once, after it goes quiet; a later hash again.
func TestMemoryReleaserCoalescesBurst(t *testing.T) {
	r, freed := countingReleaser(50 * time.Millisecond)
	for range 5 {
		r.release()
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-freed:
	case <-time.After(5 * time.Second):
		t.Fatal("memory never released after the burst")
	}
	select {
	case <-freed:
		t.Fatal("burst released more than once")
	case <-time.After(150 * time.Millisecond):
	}
	r.release()
	select {
	case <-freed:
	case <-time.After(5 * time.Second):
		t.Fatal("a hash after the release was not released again")
	}
}

// Nothing is released while hashes keep coming inside the delay.
func TestMemoryReleaserWaitsForQuiet(t *testing.T) {
	r, freed := countingReleaser(200 * time.Millisecond)
	for range 6 {
		r.release()
		time.Sleep(50 * time.Millisecond)
		select {
		case <-freed:
			t.Fatal("released while hashes were still coming")
		default:
		}
	}
	select {
	case <-freed:
	case <-time.After(5 * time.Second):
		t.Fatal("memory never released")
	}
}

// Every hash and verify schedules the release: HashPassword, VerifyPassword with
// the right and the wrong password.
func TestHashAndVerifyReleaseMemory(t *testing.T) {
	r, freed := countingReleaser(time.Millisecond)
	prev := hashMemory
	hashMemory = r
	t.Cleanup(func() { hashMemory = prev })
	wait := func(what string) {
		t.Helper()
		select {
		case <-freed:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not release its memory", what)
		}
	}
	h, err := HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	wait("HashPassword")
	if ok, _ := VerifyPassword("pw", h); !ok {
		t.Fatal("verify failed")
	}
	wait("VerifyPassword")
	if ok, _ := VerifyPassword("nope", h); ok {
		t.Fatal("wrong password verified")
	}
	wait("a failed VerifyPassword")
}

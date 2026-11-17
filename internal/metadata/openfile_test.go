package metadata

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitUncounted waits for s's abandoned reads to return, failing the test
// when they don't.
func waitUncounted(t *testing.T, s *stuckReads) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.n.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("abandoned reads = %d after they returned, want 0", s.n.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

// An open that doesn't answer (a share that stopped answering) is given up on
// at the read's deadline and counted on its mount like a read: the file's tags
// unread and its length out of time, as a read cut short there is, no
// fingerprint.
// Once the mount's count is at the cap, further opens there are refused at
// once; as the held opens return, the count goes back to none. Not parallel:
// HoldOpensForTests swaps the mount lookup.
func TestHeldOpenIsAbandonedAndCounted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "book.mp3")
	if err := os.WriteFile(path, make([]byte, 1<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	release := HoldOpensForTests(dir)
	t.Cleanup(release)
	hung := stuckByMount.For(path)

	start := time.Now()
	m, err := ExtractTimeout(context.Background(), path, "", 20*time.Millisecond)
	if err != nil || m.OpenErr != nil || !m.TagsUnread() || m.ReadRefused() || !m.ProbeTimedOut() {
		t.Fatalf("held open = %+v (err %v), want it out of time, not refused", m, err)
	}
	if fp, err := FingerprintErr(context.Background(), path, 20*time.Millisecond); fp != "" || !GaveUp(err) {
		t.Errorf("fingerprint of a held open = %q, %v; want none, given up on", fp, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("held opens took %v, want them given up at their deadline", d)
	}
	if n := hung.n.Load(); n != 2 {
		t.Errorf("abandoned reads = %d, want the two opens counted", n)
	}
	for hung.n.Load() < maxAbandonedReads {
		Fingerprint(context.Background(), path, time.Millisecond)
	}
	if m := ExtractTags(t.Context(), path, ProbeTimeout); !m.ReadRefused() {
		t.Errorf("an open at the cap = %+v, want it refused", m)
	}
	if _, err := FingerprintErr(t.Context(), path, ProbeTimeout); !GaveUp(err) {
		t.Errorf("a fingerprint at the cap = %v, want it given up on", err)
	}
	release()
	waitUncounted(t, hung)
	if fp := Fingerprint(context.Background(), path, time.Minute); fp == "" {
		t.Error("no fingerprint once the opens answer")
	}
}

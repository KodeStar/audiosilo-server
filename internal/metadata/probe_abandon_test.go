package metadata

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// An ffprobe killed at its time-out that doesn't end (here its child holds
// its output open; on macOS one stuck in an NFS read) is left after
// ffprobeKillGrace, answering as timed out and counted on the file's mount
// until it ends. Not parallel: it swaps ffprobeKillGrace and the mount lookup.
func TestFFprobeThatWontEndIsLeft(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the ffprobe stand-in is a shell script")
	}
	dir := t.TempDir()
	probe := filepath.Join(dir, "ffprobe")
	syscall.ForkLock.RLock()
	err := os.WriteFile(probe, []byte("#!/bin/sh\nsleep 3 &\nwait\n"), 0o755) //nolint:gosec // G306: the stand-in must be executable
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	wasGrace, wasOf := ffprobeKillGrace, stuckByMount.Mount
	ffprobeKillGrace = 100 * time.Millisecond
	key := t.Name()
	stuckByMount.Mount = func(string) (string, string) { return key, key }
	t.Cleanup(func() { ffprobeKillGrace, stuckByMount.Mount = wasGrace, wasOf })
	stuck := stuckByMount.For(dir)

	start := time.Now()
	_, err = runFFprobe(context.Background(), filepath.Join(dir, "book.mp3"), probe, 500*time.Millisecond)
	if !errors.Is(err, ErrProbeTimeout) {
		t.Errorf("err = %v, want the time-out", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("runFFprobe took %v, want it left after its grace", d)
	}
	if n := stuck.n.Load(); n != 1 {
		t.Errorf("abandoned reads = %d, want the run counted", n)
	}
	waitUncounted(t, stuck)
}

// With its mount's count of hung reads full, ffprobe is not run on a file
// there (it would hang too, one more process left behind): it answers as timed
// out at once. Not parallel: HangMountForTests swaps the mount lookup.
func TestFFprobeNotRunOnSaturatedMount(t *testing.T) {
	dir := t.TempDir()
	release := HangMountForTests(dir)
	t.Cleanup(release)
	_, err := runFFprobe(context.Background(), filepath.Join(dir, "book.mp3"), "/no/such/ffprobe", time.Minute)
	if !errors.Is(err, ErrProbeTimeout) || !errors.Is(err, errReadRefused) {
		t.Errorf("err = %v, want it refused as a time-out, ffprobe not run", err)
	}
}

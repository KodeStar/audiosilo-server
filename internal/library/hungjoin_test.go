package library

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// SafeJoin's Lstats of the names below the root on a share that stopped
// answering (SRV-15c: one stream sat 71 s in them before its open's bound
// began) are given up on at the bound, as a look given up on
// (metadata.GaveUp), and answer as SafeJoin's once the share does. A path out
// of the root is refused without a look, and so is a hidden one by
// ContentJoinBounded. Not parallel: metadata.HoldOpensForTests swaps the mount
// lookup.
func TestSafeJoinBoundedGivesUpOnAHeldShare(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Book/01.mp3", []byte("audio"))
	release := metadata.HoldOpensForTests(filepath.Join(root, "Book"))
	t.Cleanup(release)

	start := time.Now()
	if _, err := SafeJoinBounded(t.Context(), root, "Book/01.mp3", 100*time.Millisecond); !metadata.GaveUp(err) {
		t.Errorf("SafeJoinBounded on a held share = %v, want a look given up on", err)
	}
	if _, err := ContentJoinBounded(t.Context(), root, "Book/01.mp3", 100*time.Millisecond); !metadata.GaveUp(err) {
		t.Errorf("ContentJoinBounded on a held share = %v, want a look given up on", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the joins took %v, want each given up on at its bound", took)
	}
	if _, err := SafeJoinBounded(t.Context(), root, "../outside", time.Second); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("a path out of the root = %v, want ErrOutsideRoot", err)
	}
	if _, err := ContentJoinBounded(t.Context(), root, "Book/.audiosilo/x.mp3", time.Second); !errors.Is(err, ErrHidden) {
		t.Errorf("a hidden path = %v, want ErrHidden", err)
	}

	release()
	got, err := SafeJoinBounded(t.Context(), root, "Book/01.mp3", time.Second)
	if want := filepath.Join(root, "Book", "01.mp3"); err != nil || got != want {
		t.Errorf("SafeJoinBounded once the share answers = %q, %v; want %q", got, err, want)
	}
}

//go:build windows

package library

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// mklinkJ makes a junction at link to target (cmd's mklink /J), skipping the
// test where none can be made.
func mklinkJ(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("no junction made: %v %s", err, out)
	}
}

// TestSafeJoinRootThroughAJunction (security, Windows): a library root reached
// through a junction, which filepath.EvalSymlinks fails (ENOTDIR), is resolved
// as Below resolves it (metadata.ResolveLinks), so the links below it are
// still checked: a junction inside the library to a folder out of it is
// ErrOutsideRoot, never joined as a plain path, for SafeJoin and
// SafeJoinListed alike; a file inside the library joins.
func TestSafeJoinRootThroughAJunction(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	reached := filepath.Join(base, "Real")
	outside := filepath.Join(base, "Outside")
	for _, file := range []string{filepath.Join(reached, "Lib", "Book", "01.mp3"), filepath.Join(outside, "01.mp3")} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mklinkJ(t, filepath.Join(base, "Via"), reached)
	mklinkJ(t, filepath.Join(reached, "Lib", "Away"), outside)
	root := filepath.Join(base, "Via", "Lib")
	if _, err := SafeJoin(root, "Book/01.mp3"); err != nil {
		t.Fatalf("a file inside a root reached through a junction: %v", err)
	}
	if _, err := SafeJoin(root, "Away/01.mp3"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("a junction out of a root reached through a junction = %v, want ErrOutsideRoot", err)
	}
	ctx := t.Context()
	if _, there, err := SafeJoinListed(ctx, root, "Book/01.mp3", time.Minute, metadata.ReadDirNamesBounded); err != nil || !there {
		t.Fatalf("the stream's join of a file inside that root = %v, %v; want it there", there, err)
	}
	if _, _, err := SafeJoinListed(ctx, root, "Away/01.mp3", time.Minute, metadata.ReadDirNamesBounded); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("the stream's join through a junction out of that root = %v, want ErrOutsideRoot", err)
	}
}

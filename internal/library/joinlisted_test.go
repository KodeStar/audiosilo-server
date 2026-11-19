package library

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// listedRoot is a resolved temp root holding "Émile Zola/Germinal/01.mp3",
// with a case variant of each folder the disk answers to as one that ignores
// case would (when the temp dir doesn't itself: a link at "émile zola" and at
// "Émile Zola/germinal"), kept out of the listings names reads, as such a disk
// lists only the stored spelling.
func listedRoot(t *testing.T) (root string, names func(ctx context.Context, dir string, timeout time.Duration) ([]string, error)) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Émile Zola/Germinal/01.mp3", []byte("x"))
	hidden := map[string]bool{}
	for at, to := range map[string]string{"émile zola": "Émile Zola", "Émile Zola/germinal": "Germinal"} {
		p := filepath.Join(root, filepath.FromSlash(at))
		if _, err := os.Lstat(p); err == nil {
			continue // the disk ignores case itself
		}
		if err := os.Symlink(to, p); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
		hidden[p] = true
	}
	return root, func(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
		ns, err := metadata.ReadDirNamesBounded(ctx, dir, timeout)
		return slices.DeleteFunc(ns, func(n string) bool { return hidden[filepath.Join(dir, n)] }), err
	}
}

// TestSafeJoinListedVariantCostsWhatMissingDoes (security): a case variant of
// a path the disk has and a path with nothing there take the same steps (the
// same listings and looks, in the same order, each name looked for in its
// folder's listing before it is looked at), so how long the join takes never
// tells a share rule spelled in another case that the folder is there; both
// are not there. Allowed: the path as the disk spells it is there.
func TestSafeJoinListedVariantCostsWhatMissingDoes(t *testing.T) {
	t.Parallel()
	root, names := listedRoot(t)
	ctx := t.Context()
	steps := func(rel string) ([]string, bool) {
		t.Helper()
		var (
			mu  sync.Mutex
			got []string
		)
		note := func(op, p string) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, op+" "+strings.TrimPrefix(p, root))
		}
		lk := boundedLooks(ctx, func() time.Duration { return time.Second }, names)
		lstat, listed := lk.lstat, lk.listed
		lk.lstat = func(p string) (fs.FileInfo, error) { note("lstat", p); return lstat(p) }
		lk.listed = func(dir, name string) (bool, error) { note("list", dir); return listed(dir, name) }
		_, there, err := resolveBelow(root, filepath.Join(root, filepath.FromSlash(rel)), lk)
		if err != nil {
			t.Fatalf("%+q: %v", rel, err)
		}
		return got, there
	}
	for _, k := range []struct{ variant, missing string }{
		{"émile zola/Germinal/01.mp3", "émile zola/Nana/01.mp3"},
		{"Émile Zola/germinal/01.mp3", "Émile Zola/Nana/01.mp3"},
		{"émile zola", "Nowhere"},
	} {
		v, vThere := steps(k.variant)
		m, mThere := steps(k.missing)
		if vThere || mThere || len(v) == 0 || !slices.Equal(v, m) {
			t.Errorf("the variant %+q took %q (there %v), the missing %+q %q (there %v); want the same steps, neither there",
				k.variant, v, vThere, k.missing, m, mThere)
		}
	}
	// SafeJoin's plain looks, with no listing, would look further at the variant.
	if _, there, _ := resolveBelow(root, filepath.Join(root, "Émile Zola", "germinal", "01.mp3"), plainLooks); !there {
		t.Fatal("the fixture's disk does not answer to the variant")
	}

	for _, rel := range []string{"Émile Zola/Germinal/01.mp3", "Émile Zola/Germinal", "Émile Zola"} {
		abs, there, err := SafeJoinListed(ctx, root, rel, time.Second, names)
		if err != nil || !there || abs != filepath.Join(root, filepath.FromSlash(rel)) {
			t.Errorf("SafeJoinListed(%+q) = %q %v %v, want it there", rel, abs, there, err)
		}
	}
	for _, rel := range []string{"émile zola/Germinal/01.mp3", "Émile Zola/germinal/01.mp3", "Émile Zola/Nana"} {
		if _, there, err := SafeJoinListed(ctx, root, rel, time.Second, names); err != nil || there {
			t.Errorf("SafeJoinListed(%+q) = there %v %v, want not there", rel, there, err)
		}
	}
}

// TestSafeJoinListedLinkOutOfRoot (security): a path through a link out of
// the root is ErrOutsideRoot whatever the folder it points to holds, and
// nothing below the link is listed or looked at, a path that comes back into
// the root through a link to the folder holding it ("Up/lib/...") too: a link
// out of the root further down it would go unlooked at. Allowed: a link to a
// folder inside the root is followed, as SafeJoin follows it; a dangling one
// is a path with nothing there.
func TestSafeJoinListedLinkOutOfRoot(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "lib")
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, outside, "present/01.mp3", []byte("x"))
	writeFile(t, root, "Book/01.mp3", []byte("x"))
	for link, to := range map[string]string{"Out": outside, "In": filepath.Join(root, "Book"), "Gone": filepath.Join(root, "nope"), "Up": base} {
		if err := os.Symlink(to, filepath.Join(root, link)); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
	}
	var (
		mu    sync.Mutex
		dirs  []string
		names = func(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
			mu.Lock()
			dirs = append(dirs, dir)
			mu.Unlock()
			return metadata.ReadDirNamesBounded(ctx, dir, timeout)
		}
	)
	ctx := t.Context()
	for _, rel := range []string{"Out", "Out/present", "Out/present/01.mp3", "Out/absent", "Out/absent/01.mp3",
		"Up", "Up/lib/Book/01.mp3", "Up/lib/Out/present/01.mp3"} {
		if _, _, err := SafeJoinListed(ctx, root, rel, time.Second, names); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("SafeJoinListed(%+q) err = %v, want ErrOutsideRoot", rel, err)
		}
	}
	for _, d := range dirs {
		if !withinRoot(root, d) {
			t.Errorf("listed %q, a folder out of the root", d)
		}
	}
	for rel, want := range map[string]bool{"In": true, "In/01.mp3": true, "In/02.mp3": false, "Gone": false, "Gone/x": false} {
		if _, there, err := SafeJoinListed(ctx, root, rel, time.Second, names); err != nil || there != want {
			t.Errorf("SafeJoinListed(%+q) = there %v %v, want there %v", rel, there, err, want)
		}
	}
	if _, _, err := SafeJoinListed(ctx, root, "../x", time.Second, names); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("a path climbing out of the root: err = %v, want ErrOutsideRoot", err)
	}
}

// TestSafeJoinListedGivesUp: a listing given up on is the join's error
// (metadata.GaveUp), never a path with nothing there.
func TestSafeJoinListedGivesUp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "Book/01.mp3", []byte("x"))
	_, _, err := SafeJoinListed(t.Context(), root, "Book/01.mp3", time.Second,
		func(context.Context, string, time.Duration) ([]string, error) { return nil, metadata.ErrNotAnswered })
	if !metadata.GaveUp(err) {
		t.Errorf("err = %v, want one given up on", err)
	}
}

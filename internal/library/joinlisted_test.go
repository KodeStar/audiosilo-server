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
	"syscall"
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
		lk := boundedLooks(ctx, time.Now().Add(time.Second), names, metadataLooks)
		lstat, listed := lk.lstat, lk.listed
		lk.lstat = func(p string) (fs.FileInfo, error) { note("lstat", p); return lstat(p) }
		lk.listed = func(dir, name string) (bool, error) { note("list", dir); return listed(dir, name) }
		_, there, err := resolveBelow(root, root, filepath.Join(root, filepath.FromSlash(rel)), lk)
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
	if _, there, _ := resolveBelow(root, root, filepath.Join(root, "Émile Zola", "germinal", "01.mp3"), plainLooks); !there {
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

// TestSafeJoinListedLinkFaultOutOfRoot (security): a link out of the root is
// refused (ErrOutsideRoot) whatever its target's mount does, hanging, failing
// as a share does that a retry may get past (EIO, ESTALE) or answering, and
// whether the target is there or not: the link is read, never followed, and
// no look is made at any name out of the root, so the answer tells nothing
// about what the link points to or how its mount fails. Denied the refusal: a
// link inside the root whose own read or whose target's look faults is a look
// given up on, as any look there is, and one whose target is there is
// followed.
func TestSafeJoinListedLinkFaultOutOfRoot(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, outside, "Book/01.mp3", []byte("x"))
	writeFile(t, root, "Real/01.mp3", []byte("x"))
	for link, to := range map[string]string{
		"Out": filepath.Join(outside, "Book"), "OutGone": filepath.Join(outside, "nope"),
		"OutRel": "../" + filepath.Base(outside) + "/Book", "In": filepath.Join(root, "Real"), "InRel": "Real",
	} {
		if err := os.Symlink(to, filepath.Join(root, link)); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
	}
	names := metadata.ReadDirNamesBounded
	for _, c := range []struct {
		name, rel string
		at        string // the look that fails there
		fault     error
		want      string // "out", "gaveUp", "there"
	}{
		{"out, its target hanging", "Out/01.mp3", outside, metadata.ErrNotAnswered, "out"},
		{"out, its target's mount EIO", "Out/01.mp3", outside, syscall.EIO, "out"},
		{"out, its target ESTALE", "Out", filepath.Join(outside, "Book"), syscall.ESTALE, "out"},
		{"out, its target not there", "OutGone/01.mp3", "", nil, "out"},
		{"out by a relative link, its target hanging", "OutRel/01.mp3", outside, metadata.ErrNotAnswered, "out"},
		{"in, its target's lstat EIO", "In/01.mp3", filepath.Join(root, "Real"), syscall.EIO, "gaveUp"},
		{"in, the link's own readlink ESTALE", "In", filepath.Join(root, "In"), syscall.ESTALE, "gaveUp"},
		{"in, a relative link's target hanging", "InRel/01.mp3", filepath.Join(root, "Real"), metadata.ErrNotAnswered, "gaveUp"},
		{"in, answering", "In/01.mp3", "", nil, "there"},
		{"in by a relative link, answering", "InRel/01.mp3", "", nil, "there"},
	} {
		var (
			mu     sync.Mutex
			looked []string
		)
		fail := func(p string) error {
			mu.Lock()
			looked = append(looked, p)
			mu.Unlock()
			if c.at != "" && (p == c.at || strings.HasPrefix(p, c.at+string(filepath.Separator))) {
				return &fs.PathError{Op: "lstat", Path: p, Err: c.fault}
			}
			return nil
		}
		raw := rawLooks{
			lstat: func(ctx context.Context, p string, timeout time.Duration) (fs.FileInfo, error) {
				if err := fail(p); err != nil {
					return nil, err
				}
				return metadata.LstatBounded(ctx, p, timeout)
			},
			readlink: func(ctx context.Context, p string, timeout time.Duration) (string, error) {
				if err := fail(p); err != nil {
					return "", err
				}
				return metadata.ReadlinkBounded(ctx, p, timeout)
			},
		}
		lk := boundedLooks(t.Context(), time.Now().Add(time.Second), names, raw)
		target, there, err := resolveBelow(root, root, filepath.Join(root, filepath.FromSlash(c.rel)), lk)
		switch c.want {
		case "out":
			if err != nil || withinRoot(root, target) {
				t.Errorf("%s: target %q, there %v, err %v; want a path out of the root, no error", c.name, target, there, err)
			}
		case "gaveUp":
			if !metadata.GaveUp(err) {
				t.Errorf("%s: there %v, err %v; want a look given up on", c.name, there, err)
			}
		case "there":
			if err != nil || !there || !withinRoot(root, target) {
				t.Errorf("%s: target %q, there %v, err %v; want it there in the root", c.name, target, there, err)
			}
		}
		for _, p := range looked {
			if !withinRoot(root, p) {
				t.Errorf("%s: looked at %q, out of the root", c.name, p)
			}
		}
	}
	// Through SafeJoinListed: every link out of the root refused, one in it followed.
	for _, rel := range []string{"Out/01.mp3", "OutGone/01.mp3", "OutRel/01.mp3", "Out"} {
		if _, _, err := SafeJoinListed(t.Context(), root, rel, time.Second, names); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("SafeJoinListed(%q): err %v, want ErrOutsideRoot", rel, err)
		}
	}
	if _, there, err := SafeJoinListed(t.Context(), root, "In/01.mp3", time.Second, names); err != nil || !there {
		t.Errorf("a link in the root answering: there %v, err %v, want there", there, err)
	}
}

// TestResolveBelowLinkLoop: a link loop inside the root resolves to nothing
// (a name not there, as filepath.EvalSymlinks' error made it), never a walk
// that does not end.
func TestResolveBelowLinkLoop(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for link, to := range map[string]string{"A": "B", "B": "A", "Self": "Self/x"} {
		if err := os.Symlink(to, filepath.Join(root, link)); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
	}
	for _, rel := range []string{"A", "A/x", "Self"} {
		target, there, err := resolveBelow(root, root, filepath.Join(root, rel), plainLooks)
		if err != nil || there || !withinRoot(root, target) {
			t.Errorf("resolveBelow(%q) = %q, there %v, err %v; want a path in the root with nothing there", rel, target, there, err)
		}
	}
}

// TestJoinLinkThroughTheRootAsConfigured (security): with a root configured
// through a link (alias, to the folder real), a link inside it whose absolute
// target names a file through the configured path resolves inside the root,
// as the OS opens it (SafeJoin and SafeJoinListed both). Denied: an absolute
// target through any other link above the root, or through the configured
// path and back out of it ("alias/../outside"), is out of the root.
func TestJoinLinkThroughTheRootAsConfigured(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real, alias, other := filepath.Join(base, "real"), filepath.Join(base, "alias"), filepath.Join(base, "other")
	writeFile(t, real, "Real/01.mp3", []byte("x"))
	writeFile(t, base, "outside/01.mp3", []byte("x"))
	for link, to := range map[string]string{
		alias: real, other: real,
		filepath.Join(real, "ViaAlias"):   filepath.Join(alias, "Real"),
		filepath.Join(real, "ViaOther"):   filepath.Join(other, "Real"),
		filepath.Join(real, "AliasUpOut"): filepath.Join(alias, "..", "outside"),
	} {
		if err := os.Symlink(to, link); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
	}
	names := metadata.ReadDirNamesBounded
	if _, err := SafeJoin(alias, "ViaAlias/01.mp3"); err != nil {
		t.Errorf("SafeJoin through the configured root: %v, want it joined", err)
	}
	if _, there, err := SafeJoinListed(t.Context(), alias, "ViaAlias/01.mp3", time.Second, names); err != nil || !there {
		t.Errorf("SafeJoinListed through the configured root: there %v, err %v; want there", there, err)
	}
	for _, rel := range []string{"ViaOther/01.mp3", "AliasUpOut/01.mp3"} {
		if _, err := SafeJoin(alias, rel); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("SafeJoin(%q): %v, want ErrOutsideRoot", rel, err)
		}
		if _, _, err := SafeJoinListed(t.Context(), alias, rel, time.Second, names); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("SafeJoinListed(%q): %v, want ErrOutsideRoot", rel, err)
		}
	}
}

// irregularInfo is a name's Lstat as Windows reports a junction (a mount
// point) since Go 1.23: irregular, neither a link nor a folder.
type irregularInfo struct{ fs.FileInfo }

func (irregularInfo) Mode() fs.FileMode { return fs.ModeIrregular }
func (irregularInfo) IsDir() bool       { return false }

// TestResolveBelowJunction (security): a name Lstat reports as irregular and
// no folder (a Windows junction) is read as a link: one whose target is out of
// the root is refused, nothing out of the root looked at, and one into the
// root followed. Allowed: one that does not read as a link (another reparse
// point, a cloud file's placeholder) is a plain name, walked as before.
func TestResolveBelowJunction(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "J/01.mp3", []byte("x"))
	writeFile(t, root, "Real/01.mp3", []byte("x"))
	writeFile(t, outside, "01.mp3", []byte("x"))
	junction := filepath.Join(root, "J")
	for _, c := range []struct {
		name, to string
		readErr  error
		want     string // "out", "there"
	}{
		{"out of the root", outside, nil, "out"},
		{"into the root", filepath.Join(root, "Real"), nil, "there"},
		{"not a link", "", &fs.PathError{Op: "readlink", Path: junction, Err: syscall.ENOENT}, "there"},
		{"not a reparse point", "", &fs.PathError{Op: "readlink", Path: junction, Err: syscall.Errno(4390)}, "there"},
		{"that can't be read", "", &fs.PathError{Op: "readlink", Path: junction, Err: syscall.EACCES}, "refused"},
		{"whose read a share fails", "", &fs.PathError{Op: "readlink", Path: junction, Err: syscall.EIO}, "gaveUp"},
	} {
		var (
			mu     sync.Mutex
			looked []string
		)
		note := func(p string) {
			mu.Lock()
			looked = append(looked, p)
			mu.Unlock()
		}
		raw := rawLooks{
			lstat: func(ctx context.Context, p string, timeout time.Duration) (fs.FileInfo, error) {
				note(p)
				fi, err := metadata.LstatBounded(ctx, p, timeout)
				if err == nil && p == junction {
					return irregularInfo{fi}, nil
				}
				return fi, err
			},
			readlink: func(ctx context.Context, p string, timeout time.Duration) (string, error) {
				note(p)
				if p == junction {
					return c.to, c.readErr
				}
				return metadata.ReadlinkBounded(ctx, p, timeout)
			},
		}
		lk := boundedLooks(t.Context(), time.Now().Add(time.Second), metadata.ReadDirNamesBounded, raw)
		target, there, err := resolveBelow(root, root, filepath.Join(root, "J", "01.mp3"), lk)
		switch c.want {
		case "out":
			if err != nil || withinRoot(root, target) {
				t.Errorf("a junction %s: target %q, there %v, err %v; want a path out of the root", c.name, target, there, err)
			}
		case "there":
			if err != nil || !there || !withinRoot(root, target) {
				t.Errorf("a junction %s: target %q, there %v, err %v; want it there in the root", c.name, target, there, err)
			}
		case "refused":
			// Never walked through as a plain folder: its target is not known.
			if !errors.Is(err, ErrOutsideRoot) || there {
				t.Errorf("a junction %s: target %q, there %v, err %v; want ErrOutsideRoot", c.name, target, there, err)
			}
		case "gaveUp":
			if !metadata.GaveUp(err) || there {
				t.Errorf("a junction %s: target %q, there %v, err %v; want a look given up on", c.name, target, there, err)
			}
		}
		for _, p := range looked {
			if !withinRoot(root, p) {
				t.Errorf("a junction %s: looked at %q, out of the root", c.name, p)
			}
		}
	}
}

// TestSafeJoinRefusesALinkItCannotCheck (security): SafeJoin never accepts a
// path through a link it could not read. A link there whose read fails for
// good (a permission) is ErrOutsideRoot, as a link out of the root is; a read
// or a name's lstat failing as a share does that a retry may get past (EIO)
// is a look given up on, never a name not there, which an open made once the
// share answers again would follow wherever the link points. Allowed: a link
// into the root that reads is followed, and a name whose lstat is refused for
// good is a name not there, as it was (no open gets through it either).
func TestSafeJoinRefusesALinkItCannotCheck(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Real/01.mp3", []byte("x"))
	writeFile(t, outside, "01.mp3", []byte("x"))
	// Out points out of the root: with its read failing, only the refusal
	// keeps a path through it from being accepted.
	for link, to := range map[string]string{"In": filepath.Join(root, "Real"), "Out": outside} {
		if err := os.Symlink(to, filepath.Join(root, link)); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
	}
	for _, c := range []struct {
		name, rel string
		op, at    string // the look that fails, and where
		fault     syscall.Errno
		want      string // "refused", "gaveUp", "joined"
	}{
		{"a link out of the root whose read is refused", "Out/01.mp3", "readlink", "Out", syscall.EACCES, "refused"},
		{"a link whose read fails for another lasting reason", "Out/01.mp3", "readlink", "Out", syscall.ENXIO, "refused"},
		{"a link into the root whose read is refused", "In/01.mp3", "readlink", "In", syscall.EACCES, "refused"},
		{"a link whose read a share fails", "Out/01.mp3", "readlink", "Out", syscall.EIO, "gaveUp"},
		{"a name whose lstat a share fails", "Out/01.mp3", "lstat", "Out", syscall.ESTALE, "gaveUp"},
		{"a name under a link whose lstat a share fails", "In/01.mp3", "lstat", "Real/01.mp3", syscall.EIO, "gaveUp"},
		{"a name whose lstat is refused for good", "Real/01.mp3", "lstat", "Real", syscall.EACCES, "joined"},
		{"every look answering", "In/01.mp3", "", "", 0, "joined"},
	} {
		fail := func(op, p string) error {
			if op == c.op && p == filepath.Join(root, filepath.FromSlash(c.at)) {
				return &fs.PathError{Op: op, Path: p, Err: c.fault}
			}
			return nil
		}
		lk := plainLooksOver(
			func(p string) (fs.FileInfo, error) {
				if err := fail("lstat", p); err != nil {
					return nil, err
				}
				return os.Lstat(p)
			},
			func(p string) (string, error) {
				if err := fail("readlink", p); err != nil {
					return "", err
				}
				return os.Readlink(p)
			})
		abs, err := safeJoin(root, c.rel, lk)
		switch c.want {
		case "refused":
			if !errors.Is(err, ErrOutsideRoot) {
				t.Errorf("%s: %q, err %v; want ErrOutsideRoot", c.name, abs, err)
			}
		case "gaveUp":
			if !metadata.GaveUp(err) || errors.Is(err, ErrOutsideRoot) {
				t.Errorf("%s: %q, err %v; want a look given up on", c.name, abs, err)
			}
		case "joined":
			if err != nil || abs != filepath.Join(root, filepath.FromSlash(c.rel)) {
				t.Errorf("%s: %q, err %v; want the path joined", c.name, abs, err)
			}
		}
	}
	// The same through SafeJoinListed's looks: an unreadable link is refused.
	raw := rawLooks{
		lstat: metadata.LstatBounded,
		readlink: func(ctx context.Context, p string, timeout time.Duration) (string, error) {
			if p == filepath.Join(root, "Out") {
				return "", &fs.PathError{Op: "readlink", Path: p, Err: syscall.EACCES}
			}
			return metadata.ReadlinkBounded(ctx, p, timeout)
		},
	}
	lk := boundedLooks(t.Context(), time.Now().Add(time.Second), metadata.ReadDirNamesBounded, raw)
	if _, there, err := resolveBelow(root, root, filepath.Join(root, "Out", "01.mp3"), lk); !errors.Is(err, ErrOutsideRoot) || there {
		t.Errorf("SafeJoinListed's looks at an unreadable link: there %v, err %v; want ErrOutsideRoot", there, err)
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

// TestSafeJoinListedReadsAShareFault: a listing or look failing as a share
// does that a retry may get past (EIO, ESTALE, ENOTCONN: metadata.TransientFault) is
// one given up on (metadata.GaveUp, the API's 503 share_not_answering), as the
// scanner reads it, never a name not there (a 404 for a book on a share that
// stopped answering). Denied the 503: a permission (EACCES, EPERM), which
// stays until an admin changes it, the device gone (ENXIO), and the name gone
// stay a name not there.
func TestSafeJoinListedReadsAShareFault(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Author/Book/01.mp3", []byte("x"))
	author := filepath.Join(root, "Author")
	for _, c := range []struct {
		errno  syscall.Errno
		gaveUp bool
	}{
		{syscall.EIO, true}, {syscall.ESTALE, true}, {syscall.ENOTCONN, true},
		{syscall.EACCES, false}, {syscall.EPERM, false}, {syscall.ENOENT, false},
		{syscall.ENXIO, false}, // the device gone (a USB disk unplugged): lasting, as a permission is
	} {
		names := func(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
			if dir == author {
				return nil, &fs.PathError{Op: "open", Path: dir, Err: c.errno}
			}
			return metadata.ReadDirNamesBounded(ctx, dir, timeout)
		}
		_, there, err := SafeJoinListed(t.Context(), root, "Author/Book", time.Second, names)
		switch {
		case c.gaveUp && (!metadata.GaveUp(err) || !errors.Is(err, c.errno)):
			t.Errorf("the book's folder's listing %v: there %v, err %v; want a look given up on, keeping the errno", c.errno, there, err)
		case !c.gaveUp && (err != nil || there):
			t.Errorf("the book's folder's listing %v: there %v, err %v; want a name not there", c.errno, there, err)
		}
	}
}

// TestUnansweredLookReadsAShareFault: the book folder's own stat in
// SafeJoinListed's walk is read by the same rule as a listing: EIO or ESTALE
// a look given up on, a permission, the device gone (ENXIO) or the name gone
// none (a name not there).
func TestUnansweredLookReadsAShareFault(t *testing.T) {
	t.Parallel()
	stat := func(errno syscall.Errno) error {
		return &fs.PathError{Op: "lstat", Path: "/lib/Author/Book", Err: errno}
	}
	for _, errno := range []syscall.Errno{syscall.EIO, syscall.ESTALE, syscall.ETIMEDOUT} {
		if err := unansweredLook(t.Context(), stat(errno)); !metadata.GaveUp(err) {
			t.Errorf("a stat failing %v: %v, want a look given up on", errno, err)
		}
	}
	for _, errno := range []syscall.Errno{syscall.EACCES, syscall.EPERM, syscall.ENOENT, syscall.ENOTDIR, syscall.ENXIO} {
		if err := unansweredLook(t.Context(), stat(errno)); err != nil {
			t.Errorf("a stat failing %v: %v, want a name not there", errno, err)
		}
	}
	if err := unansweredLook(t.Context(), metadata.ErrNotAnswered); !metadata.GaveUp(err) {
		t.Errorf("a stat not answered in time: %v, want a look given up on", err)
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if err := unansweredLook(ended, stat(syscall.ENOENT)); err == nil {
		t.Error("a look whose context ended: no error, want it unanswered")
	}
}

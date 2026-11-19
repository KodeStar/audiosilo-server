package metadata

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Below runs open on path, a file or folder a request named under root that
// its caller joined and checked there (library.SafeJoin, SafeJoinListed),
// through an os.Root at root, as rel under it: the open itself can't leave
// root. A check of the path and then a plain open of it are two looks, and a
// name along it swapped for a link out of root between them (by whoever can
// write to the library, or a share that serves one) would be followed by the
// open; through the os.Root, the open refuses such a link wherever it then
// is. os.Root follows a link that stays under root by a relative target, but
// refuses one it could follow safely all the same: an absolute target that
// names a place under root, a relative one that climbs above root and comes
// back. A join allows those (library.followLink), so when the first open
// fails for anything but the name gone, path is resolved
// (filepath.EvalSymlinks) and, when that is under root, opened there, through
// the os.Root again: no link is left in it to follow, and one swapped in
// since is refused as before. A path holding a NUL (the API's noFile: a path
// no file has) fails at once, the disk not reached, as a plain open's does.
func Below[T any](root, path string, open func(r *os.Root, rel string) (T, error)) (T, error) {
	return below(root, path, true, open)
}

// below is Below, path resolved for its second open up to its last name only
// unless followLast: an lstat's (LstatBelow) answers for the link at path
// itself, never for what it leads to.
func below[T any](root, path string, followLast bool, open func(r *os.Root, rel string) (T, error)) (T, error) {
	var zero T
	rootAbs, rel, resolved, err := underRoot(root, path)
	if err != nil {
		return zero, err
	}
	r, err := os.OpenRoot(rootAbs)
	if err != nil {
		return zero, err
	}
	defer func() { _ = r.Close() }()
	v, err := open(r, rel)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return v, err
	}
	if resolved == "" {
		if resolved, _ = filepath.EvalSymlinks(rootAbs); resolved == "" {
			return zero, err
		}
	}
	// path as underRoot read it (absolute: a relative one resolved alone
	// would never compare with the resolved root).
	abs := filepath.Join(rootAbs, rel)
	var real string
	var rerr error
	if followLast || rel == "." {
		real, rerr = filepath.EvalSymlinks(abs)
	} else if real, rerr = filepath.EvalSymlinks(filepath.Dir(abs)); rerr == nil {
		real = filepath.Join(real, filepath.Base(abs))
	}
	if rerr != nil {
		return zero, err
	}
	if at, ok := relBelow(resolved, real); ok && at != rel {
		return open(r, at)
	}
	return zero, err
}

// underRoot is path relative to root (rel), and the root it is under: root
// as configured (rootAbs, absolute) or, when path is joined on it as resolved
// (a join's), that one (rootAbs and resolved both). An error for a path that
// is under neither, or that holds a NUL (the API's noFile: a path no file
// has), the disk not reached, as a plain open's does.
func underRoot(root, path string) (rootAbs, rel, resolved string, err error) {
	if strings.IndexByte(path, 0) >= 0 {
		return "", "", "", &fs.PathError{Op: "open", Path: path, Err: syscall.EINVAL}
	}
	if rootAbs, err = filepath.Abs(root); err != nil {
		return "", "", "", err
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", "", "", err
	}
	// path is joined on the root as configured or, by a join, as resolved (the
	// same folder: resolved only when the first does not lead it).
	rel, ok := relBelow(rootAbs, path)
	if !ok {
		if resolved, err = filepath.EvalSymlinks(rootAbs); err != nil {
			return "", "", "", err
		}
		if rel, ok = relBelow(resolved, path); !ok {
			return "", "", "", &fs.PathError{Op: "open", Path: path, Err: errOutsideRoot}
		}
		rootAbs = resolved
	}
	return rootAbs, rel, resolved, nil
}

// errOutsideRoot is Below's for a path that is not under its root.
var errOutsideRoot = errors.New("path is not under the root")

// relBelow is p relative to root when p is root or a path under it.
func relBelow(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	return rel, err == nil && filepath.IsLocal(rel)
}

// OpenRegularBelow is OpenRegular of path, a file a request named under root,
// opened through root (Below): the stream's and a cover's open, which a link
// swapped in after the path's check can't lead out of the library.
func OpenRegularBelow(root, path string) (*os.File, os.FileInfo, error) {
	return openRegularWith(path, func() (*os.File, error) {
		return Below(root, path, func(r *os.Root, rel string) (*os.File, error) { return r.OpenFile(rel, openFlags, 0) })
	})
}

// OpenRegularBelowBounded is OpenRegularBelow as a Bounded call, as
// OpenRegularBounded is OpenRegular's.
func OpenRegularBelowBounded(ctx context.Context, root, path string, timeout time.Duration) (*os.File, os.FileInfo, error) {
	type opened struct {
		f    *os.File
		info os.FileInfo
	}
	o, err := Bounded(ctx, path, timeout, func() (opened, error) {
		f, info, err := OpenRegularBelow(root, path)
		return opened{f, info}, err
	}, func(o opened) { _ = o.f.Close() })
	return o.f, o.info, err
}

// DirBelow is a folder a request named under a root, opened through the root
// (Below) and listed from that one open: Entries sorted by name, as
// os.ReadDir's are. An entry's info is read through the open folder too
// (Info), never by the path again: fs.DirEntry.Info on Unix is an lstat of
// the folder's path and the name, which a folder swapped for a link since the
// listing would lead out of the root.
type DirBelow struct {
	Entries []fs.DirEntry
	dir     *os.Root
}

// ReadDirBelow opens and lists the folder at path under root. The caller
// closes it.
func ReadDirBelow(root, path string) (*DirBelow, error) {
	dir, err := Below(root, path, func(r *os.Root, rel string) (*os.Root, error) { return r.OpenRoot(rel) })
	if err != nil {
		return nil, err
	}
	f, err := dir.Open(".")
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return &DirBelow{Entries: entries, dir: dir}, nil
}

// ReadDirBelowBounded is ReadDirBelow as a Bounded look (as ReadDirBounded
// is os.ReadDir's): a folder opened after a call given up on is closed when
// its listing returns.
func ReadDirBelowBounded(ctx context.Context, root, path string, timeout time.Duration) (*DirBelow, error) {
	return bounded(ctx, path, timeout, true, func() (*DirBelow, error) { return ReadDirBelow(root, path) },
		func(d *DirBelow) { _ = d.Close() })
}

// Info is the info of e, one of Entries (an lstat: the entry's own, a link
// not followed), read through the open folder; where the listing holds each
// entry's info already (Windows), that one, at no further look.
func (d *DirBelow) Info(e fs.DirEntry) (fs.FileInfo, error) {
	if listingHoldsInfo {
		return e.Info()
	}
	return d.dir.Lstat(e.Name())
}

// listingHoldsInfo is whether a folder's listing carries each entry's info
// (Windows lists it with the name), so fs.DirEntry.Info looks at nothing.
const listingHoldsInfo = runtime.GOOS == "windows"

// Close closes the folder.
func (d *DirBelow) Close() error { return d.dir.Close() }

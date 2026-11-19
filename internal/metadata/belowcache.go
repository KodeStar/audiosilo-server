package metadata

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A scan's reads (a book's audio, its fingerprint, the files beside it) open
// below the library's root as a request's do (Below), but a folder walked
// from the root for every file would cost a scan an openat for each name
// along every path. So the folder a read opens a file in is kept open a
// moment (belowFolders), opened through the root once, and each file in it
// is one openat through it (BelowFolder): what a plain open of the path cost.
// A kept folder is looked at again on each use (folderCache.current: a stat
// of it and one of its path), so a folder renamed away and another made at
// its path is never read through the old handle.

// folderIdle is how long a folder opened for reads below a library's root is
// kept open unused: a scan reads a book's files, and the next book's beside
// them, within it.
const folderIdle = 2 * time.Second

// folderMaxAge is how long a folder is kept open however busy it is: past
// it the next read opens it again, the handle never older than that.
const folderMaxAge = 30 * time.Second

// maxFolders bounds how many folders are kept open at once, across every
// library: past it the one idle longest is closed, or, with all of them in
// use, a new one is opened for its read alone.
const maxFolders = 64

// folderKey names a kept folder: the root it was opened through (absolute,
// as configured, or as resolved for a path joined on that) and its path
// under it ("." for the root itself).
type folderKey struct{ root, rel string }

// keptFolder is a folder opened through its library's root, held by refs
// reads (closed only once none holds it).
type keptFolder struct {
	key    folderKey
	r      *os.Root
	refs   int
	opened time.Time
	used   time.Time
	// gone: no longer kept (dropped or never kept): closed by its last read.
	gone bool
}

// folderCache is the folders kept open (belowFolders).
type folderCache struct {
	mu      sync.Mutex
	m       map[folderKey]*keptFolder
	sweeper *time.Timer
	// openOnly: no folder is kept past its read. On Windows an open folder
	// keeps the folders above it from being renamed (a user's, or the
	// Bridge's, rename on the share would fail), so none is kept there.
	openOnly bool
	// opened, in a test, is called by get once it has opened a folder,
	// before it looks whether another read kept one meanwhile.
	opened func()
}

// belowFolders are the folders the scan's reads keep open (BelowFolder).
var belowFolders = &folderCache{m: map[folderKey]*keptFolder{}, openOnly: runtime.GOOS == "windows"}

// get is the folder at rel under root now, held for a read (put it back with
// put): one kept that is still the folder at its path (current), or opened
// now through root (the root's own kept one), and kept unless another read
// kept its own meanwhile.
func (c *folderCache) get(root, rel string) (*keptFolder, error) {
	key := folderKey{root, rel}
	c.mu.Lock()
	if f := c.m[key]; f != nil && time.Since(f.opened) < folderMaxAge {
		f.refs++
		c.mu.Unlock()
		if current(f) {
			return f, nil
		}
		// Renamed away, replaced, or remounted since it was opened.
		c.drop(f)
		c.put(f)
	} else {
		c.mu.Unlock()
	}
	var r *os.Root
	var err error
	if rel == "." {
		r, err = os.OpenRoot(root)
	} else {
		var top *keptFolder
		if top, err = c.get(root, "."); err != nil {
			return nil, err
		}
		r, err = top.r.OpenRoot(rel)
		c.put(top)
	}
	if err != nil {
		return nil, err
	}
	f := &keptFolder{key: key, r: r, refs: 1, opened: time.Now()}
	if c.opened != nil {
		c.opened()
	}
	c.mu.Lock()
	if have := c.m[key]; have != nil && time.Since(have.opened) < folderMaxAge {
		// Another read opened it meanwhile: that one stays kept, and this
		// one, opened through root just now, is its read's alone. The kept
		// one is never taken unlooked at (current): it may have been opened
		// before a rename this open came after.
		f.gone = true
		c.mu.Unlock()
		return f, nil
	}
	var closing []*keptFolder
	if old := c.m[key]; old != nil {
		// Kept past folderMaxAge: this one, opened now, takes its place.
		delete(c.m, key)
		if old.gone = true; old.refs == 0 {
			closing = append(closing, old)
		}
	}
	closing = append(closing, c.makeRoomLocked()...)
	if !c.openOnly && len(c.m) < maxFolders {
		c.m[key] = f
		c.sweepLocked()
	} else {
		f.gone = true // every kept folder in use, or none kept: this one is its read's alone
	}
	c.mu.Unlock()
	closeFolders(closing)
	return f, nil
}

// put hands back a folder get held, closing it when it is no longer kept and
// no read holds it.
func (c *folderCache) put(f *keptFolder) {
	c.mu.Lock()
	f.refs--
	f.used = time.Now()
	last := f.refs == 0 && f.gone
	c.mu.Unlock()
	if last {
		_ = f.r.Close()
	}
}

// current reports whether f, a kept folder its caller holds, is still the
// folder at its path: the folder the handle is (a stat through it) and the
// one its path names now (os.Stat) are one (os.SameFile). The path's look
// only compares: nothing is read through it, so a link along it can't lead a
// read anywhere; a folder renamed away, another made at its path, or a
// remount there, is opened again.
func current(f *keptFolder) bool {
	was, err := f.r.Stat(".")
	if err != nil {
		return false
	}
	now, err := os.Stat(filepath.Join(f.key.root, f.key.rel))
	return err == nil && os.SameFile(was, now)
}

// staleHandle reports whether err, a read's through a kept folder, says the
// folder's handle itself is dead (ESTALE: an NFS folder removed or made again
// on the server): no later read through it would answer either.
func staleHandle(err error) bool { return errors.Is(err, syscall.ESTALE) }

// drop stops keeping f (a read through it failed: it may be a folder renamed
// or replaced since it was opened), closed once no read holds it.
func (c *folderCache) drop(f *keptFolder) {
	c.mu.Lock()
	if c.m[f.key] == f {
		delete(c.m, f.key)
		f.gone = true
	}
	c.mu.Unlock()
}

// makeRoomLocked takes out of the cache the folder idle longest when it is
// full, and returns it to be closed.
func (c *folderCache) makeRoomLocked() []*keptFolder {
	if len(c.m) < maxFolders {
		return nil
	}
	var oldest *keptFolder
	for _, f := range c.m {
		if f.refs == 0 && (oldest == nil || f.used.Before(oldest.used)) {
			oldest = f
		}
	}
	if oldest == nil {
		return nil
	}
	delete(c.m, oldest.key)
	return []*keptFolder{oldest}
}

// sweepLocked starts the sweep that closes folders idle folderIdle, unless
// one is due already.
func (c *folderCache) sweepLocked() {
	if c.sweeper == nil {
		c.sweeper = time.AfterFunc(folderIdle, c.sweep)
	}
}

// sweep closes the folders idle at least folderIdle, and comes again while
// any are kept.
func (c *folderCache) sweep() {
	now := time.Now()
	var closing []*keptFolder
	c.mu.Lock()
	for k, f := range c.m {
		if f.refs == 0 && now.Sub(f.used) >= folderIdle {
			delete(c.m, k)
			closing = append(closing, f)
		}
	}
	if len(c.m) > 0 {
		c.sweeper.Reset(folderIdle)
	} else {
		c.sweeper = nil
	}
	c.mu.Unlock()
	closeFolders(closing)
}

// closeAll stops keeping every folder: one no read holds is closed now, one
// a read holds by that read's put.
func (c *folderCache) closeAll() {
	var closing []*keptFolder
	c.mu.Lock()
	for k, f := range c.m {
		delete(c.m, k)
		if f.gone = true; f.refs == 0 {
			closing = append(closing, f)
		}
	}
	c.mu.Unlock()
	closeFolders(closing)
}

// CloseFolders closes the folders kept open for reads below the libraries'
// roots (BelowFolder), every library's: called when a library is removed or
// its root changes, and when the server stops, so a folder kept for the next
// read never holds a share that is being unmounted (EBUSY) past the reads in
// progress. A read under way keeps its folder until it ends; a later read
// (a scan being stopped still has some) opens its folder again, kept
// folderIdle as ever.
func CloseFolders() { belowFolders.closeAll() }

// FoldersKeptForTests is how many folders are kept open under root (as
// given, or resolved), for a test of what lets them go. It panics outside a
// test binary.
func FoldersKeptForTests(root string) int {
	if !testing.Testing() {
		panic("metadata.FoldersKeptForTests called outside a test binary")
	}
	abs, _ := filepath.Abs(root)
	resolved, _ := filepath.EvalSymlinks(abs)
	n := 0
	belowFolders.mu.Lock()
	defer belowFolders.mu.Unlock()
	for k := range belowFolders.m {
		if k.root == abs || k.root == resolved {
			n++
		}
	}
	return n
}

func closeFolders(folders []*keptFolder) {
	for _, f := range folders {
		_ = f.r.Close()
	}
}

// BelowFolder is Below for the scan's reads: open runs on path's name
// through its folder, kept open a moment (belowFolders) and opened through
// root, so a book's files cost one openat each. The open can't leave root
// either way: a kept folder is the one opened below root and still at its
// path (folderCache.current), and a link swapped in for the name is refused
// as Below refuses it. A name not there is the answer (an episode with no
// chapters file costs one look, the folders kept), and so is its folder not
// there (a show with no .audiosilo folder: one look through the root's). A name the folder can't
// open (a link to a folder beside it, which the folder's own root refuses)
// is opened through the root's kept folder, the whole path under it, and
// failing that, the path resolved as Below resolves it (a link with an
// absolute target, a Windows junction), opened through that kept folder too,
// the root never opened again for it. A folder whose handle went stale
// (ESTALE) is kept no longer.
func BelowFolder[T any](root, path string, open func(r *os.Root, rel string) (T, error)) (T, error) {
	return belowFolder(root, path, true, open)
}

// belowFolder is BelowFolder, its last fallback (below) resolving path's
// last name too when followLast (an open's, a stat's), else only the folder
// it is in (an lstat's).
func belowFolder[T any](root, path string, followLast bool, open func(r *os.Root, rel string) (T, error)) (T, error) {
	var zero T
	rootAbs, rel, resolved, err := underRoot(root, path)
	if err != nil {
		return zero, err
	}
	// try opens name through the folder dir: answered for a file opened or a
	// name not there, else (refused, a fault) for the next way to answer.
	try := func(dir, name string) (v T, answered bool, err error) {
		f, err := belowFolders.get(rootAbs, dir)
		if err != nil {
			// No such folder under root: name is not there either, and
			// that is the answer, at one look.
			return zero, NotExist(err), err
		}
		v, err = open(f.r, name)
		if staleHandle(err) {
			belowFolders.drop(f)
		}
		belowFolders.put(f)
		return v, err == nil || NotExist(err), err
	}
	dir, name := filepath.Dir(rel), filepath.Base(rel)
	v, ok, err := try(dir, name)
	if ok {
		return v, err
	}
	if dir != "." {
		if v, ok, err = try(".", rel); ok {
			return v, err
		}
	}
	// The path resolved, opened through the root's kept folder: the open
	// through it failed just now, so it is not made again, nor the root
	// opened once more (below's way, should the root not open here, or the
	// kept handle have gone stale since its look: below's fresh root opens
	// the path itself again).
	if staleHandle(err) {
		return below(root, path, followLast, open)
	}
	top, terr := belowFolders.get(rootAbs, ".")
	if terr != nil {
		return below(root, path, followLast, open)
	}
	defer belowFolders.put(top)
	return openResolved(top.r, rootAbs, rel, resolved, followLast, err, open)
}

// openBelow opens the file at path, under root, for reading with openFlags
// (openRegular's) through its kept folder (BelowFolder).
func openBelow(root, path string) (*os.File, error) {
	return BelowFolder(root, path, func(r *os.Root, rel string) (*os.File, error) { return r.OpenFile(rel, openFlags, 0) })
}

// OpenBelow opens the file at path, under root, for reading through root
// (BelowFolder), never waiting for a FIFO's writer (openFlags, cleared once
// open): the caller checks what it opened (a regular file, the one it
// looked at).
func OpenBelow(root, path string) (*os.File, error) {
	f, err := openBelow(root, path)
	if err != nil {
		return nil, err
	}
	if err := blockingReads(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// StatBelow is os.Stat of path, a file or folder under root, looked at
// through root (BelowFolder): a link swapped in along the path after it was
// checked is never followed out of root, and the answer is for no file
// there. A link that stays in root is followed, as os.Stat follows it.
func StatBelow(root, path string) (fs.FileInfo, error) {
	return BelowFolder(root, path, func(r *os.Root, rel string) (fs.FileInfo, error) { return r.Stat(rel) })
}

// LstatBelow is StatBelow, a link at path itself not followed (os.Lstat),
// on every route: a path through a link os.Root refuses is resolved up to
// its last name only, so the answer is still the link's own.
func LstatBelow(root, path string) (fs.FileInfo, error) {
	return belowFolder(root, path, false, func(r *os.Root, rel string) (fs.FileInfo, error) { return r.Lstat(rel) })
}

// StatBelowBounded is StatBelow as a Bounded look (StatBounded's).
func StatBelowBounded(ctx context.Context, root, path string, timeout time.Duration) (fs.FileInfo, error) {
	return LookBounded(ctx, path, timeout, func() (fs.FileInfo, error) { return StatBelow(root, path) })
}

// LstatBelowBounded is LstatBelow as a Bounded look (LstatBounded's).
func LstatBelowBounded(ctx context.Context, root, path string, timeout time.Duration) (fs.FileInfo, error) {
	return LookBounded(ctx, path, timeout, func() (fs.FileInfo, error) { return LstatBelow(root, path) })
}

package metadata

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// A scan's reads (a book's audio, its fingerprint, the files beside it) open
// below the library's root as a request's do (Below), but a folder walked
// from the root for every file would cost a scan an openat for each name
// along every path. So the folder a read opens a file in is kept open a
// moment (belowFolders), opened through the root once, and each file in it
// is one openat through it (BelowFolder): what a plain open of the path cost.

// folderIdle is how long a folder opened for reads below a library's root is
// kept open unused: a scan reads a book's files, and the next book's beside
// them, within it, and a folder renamed or replaced meanwhile is opened again
// once it has been idle that long.
const folderIdle = 2 * time.Second

// folderMaxAge is how long a folder is kept open however busy it is: past
// it the next read opens it again, so a folder kept in use for a whole scan
// (a library's root) still follows a remount or a rename within that time.
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
}

// belowFolders are the folders the scan's reads keep open (BelowFolder).
var belowFolders = &folderCache{m: map[folderKey]*keptFolder{}}

// get is the folder rel under root, held for a read (put it back with put):
// one kept, or opened now through root (the root's own kept one), and kept.
// fresh reports that it was opened by this call, so it is the folder at rel
// now; a kept one may be one renamed or replaced since it was opened.
func (c *folderCache) get(root, rel string) (f *keptFolder, fresh bool, err error) {
	key := folderKey{root, rel}
	c.mu.Lock()
	if f = c.m[key]; f != nil && time.Since(f.opened) < folderMaxAge {
		f.refs++
		c.mu.Unlock()
		return f, false, nil
	}
	c.mu.Unlock()
	var r *os.Root
	if rel == "." {
		r, err = os.OpenRoot(root)
	} else {
		var top *keptFolder
		if top, _, err = c.get(root, "."); err != nil {
			return nil, false, err
		}
		r, err = top.r.OpenRoot(rel)
		c.put(top)
	}
	if err != nil {
		return nil, false, err
	}
	f = &keptFolder{key: key, r: r, refs: 1, opened: time.Now()}
	c.mu.Lock()
	if have := c.m[key]; have != nil && time.Since(have.opened) < folderMaxAge {
		// Another read opened it meanwhile: that one is kept, this one closed.
		have.refs++
		c.mu.Unlock()
		_ = r.Close()
		return have, true, nil
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
	if len(c.m) < maxFolders {
		c.m[key] = f
		c.sweepLocked()
	} else {
		f.gone = true // every kept folder in use: this one is its read's alone
	}
	c.mu.Unlock()
	closeFolders(closing)
	return f, true, nil
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

func closeFolders(folders []*keptFolder) {
	for _, f := range folders {
		_ = f.r.Close()
	}
}

// BelowFolder is Below for the scan's reads: open runs on path's name
// through its folder, kept open a moment (belowFolders) and opened through
// root, so a book's files cost one openat each. The open can't leave root
// either way: a kept folder is the one opened below root (a link swapped in
// for it since is never followed), and a link swapped in for the name is
// refused as Below refuses it. A name the folder can't open (a link to a
// folder beside it, which the folder's own root refuses) is opened through
// the root's kept folder, the whole path under it, and failing that, the
// path resolved as Below does (Below itself answers: a link with an absolute
// target). A kept folder in which the name is not there may be one renamed
// or replaced since it was opened: it is kept no longer, and the root's
// folder, then Below, answer for the path now.
func BelowFolder[T any](root, path string, open func(r *os.Root, rel string) (T, error)) (T, error) {
	var zero T
	rootAbs, rel, _, err := underRoot(root, path)
	if err != nil {
		return zero, err
	}
	dir, name := filepath.Dir(rel), filepath.Base(rel)
	if f, fresh, err := belowFolders.get(rootAbs, dir); err == nil {
		v, oerr := open(f.r, name)
		gone := NotExist(oerr)
		if gone && !fresh {
			belowFolders.drop(f)
		}
		belowFolders.put(f)
		if oerr == nil || (gone && fresh) {
			return v, oerr
		}
	}
	if dir != "." {
		if top, _, err := belowFolders.get(rootAbs, "."); err == nil {
			v, oerr := open(top.r, rel)
			if NotExist(oerr) {
				belowFolders.drop(top) // the root may have been remounted: opened again next time
			}
			belowFolders.put(top)
			if oerr == nil {
				return v, nil
			}
		}
	}
	return Below(root, path, open)
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

// LstatBelow is StatBelow, a link at path itself not followed (os.Lstat).
func LstatBelow(root, path string) (fs.FileInfo, error) {
	return BelowFolder(root, path, func(r *os.Root, rel string) (fs.FileInfo, error) { return r.Lstat(rel) })
}

// StatBelowBounded is StatBelow as a Bounded look (StatBounded's).
func StatBelowBounded(ctx context.Context, root, path string, timeout time.Duration) (fs.FileInfo, error) {
	return LookBounded(ctx, path, timeout, func() (fs.FileInfo, error) { return StatBelow(root, path) })
}

// LstatBelowBounded is LstatBelow as a Bounded look (LstatBounded's).
func LstatBelowBounded(ctx context.Context, root, path string, timeout time.Duration) (fs.FileInfo, error) {
	return LookBounded(ctx, path, timeout, func() (fs.FileInfo, error) { return LstatBelow(root, path) })
}

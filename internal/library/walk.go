package library

import (
	"context"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/pool"
)

// walkIO is how many filesystem calls a discovery walk makes at once unless
// scan.concurrency asks for fewer (Scanner.walkWidth): folders listed ahead
// (walkDir) and audio files looked at (statAudio).
const walkIO = 8

// walkDir is filepath.WalkDir(root, fn): fn is called for the same paths, in the
// same order, with the same errors, and its SkipDir and SkipAll mean the same.
// What differs is when folders are listed: the folders a folder holds are listed
// together, width at a time, before the walk goes into the first of them,
// rather than each as the walk reaches it. On a network share or a FUSE pool
// (Unraid's /mnt/user) every listing waits on a round trip, and one at a time
// they are most of a no-change scan. descend reports whether fn will go into a
// folder (nil: every one), so that none fn skips is listed ahead. A width below
// 2 lists nothing ahead, each folder read as the walk reaches it, as WalkDir
// reads it: for a walk that may stop at any folder (a capped one), or a share
// asked for one call at a time. Once ctx ends nothing more is listed ahead (fn
// sees ctx itself). The root's lstat and every listing are metadata.Bounded
// calls (ctx, at most timeout; none when 0): a folder on a share that stopped
// answering is given up on, its error (metadata.GaveUp) fn's as any listing's,
// so the walk goes on past it rather than sitting there for good.
func walkDir(ctx context.Context, root string, width int, timeout time.Duration, descend func(path string, d fs.DirEntry) bool, fn fs.WalkDirFunc) error {
	info, err := metadata.LstatBounded(ctx, root, timeout)
	if err != nil {
		err = fn(root, nil, err)
	} else {
		w := walker{ctx: ctx, width: width, timeout: timeout, descend: descend, fn: fn}
		err = w.in(root, fs.FileInfoToDirEntry(info), nil)
	}
	if err == filepath.SkipDir || err == filepath.SkipAll {
		return nil
	}
	return err
}

// listing is a folder's entries as os.ReadDir returned them.
type listing struct {
	entries []fs.DirEntry
	err     error
}

// walker is one walkDir's walk: its arguments.
type walker struct {
	ctx     context.Context
	width   int
	timeout time.Duration
	descend func(string, fs.DirEntry) bool
	fn      fs.WalkDirFunc
}

// list is os.ReadDir of path as a metadata.Bounded call (the walk's ctx and
// timeout).
func (w walker) list(path string) *listing {
	entries, err := readDirLook(w.ctx, path, w.timeout)
	return &listing{entries: entries, err: err}
}

// in is filepath's walkDir: fn on path, then into it when it is a folder fn
// doesn't skip, using listed when its entries were read ahead (nil: read now).
func (w walker) in(path string, d fs.DirEntry, listed *listing) error {
	fn := w.fn
	if err := fn(path, d, nil); err != nil || !d.IsDir() {
		if err == filepath.SkipDir && d.IsDir() {
			err = nil // skipped the folder
		}
		return err
	}
	if listed == nil {
		listed = w.list(path)
	}
	if listed.err != nil {
		// A second call, reporting the error, as WalkDir makes it.
		if err := fn(path, d, listed.err); err != nil {
			if err == filepath.SkipDir {
				err = nil
			}
			return err
		}
	}
	ahead := w.ahead(path, listed.entries)
	for i, d1 := range listed.entries {
		var next *listing
		if ahead != nil {
			next = ahead[i]
		}
		if err := w.in(filepath.Join(path, d1.Name()), d1, next); err != nil {
			if err == filepath.SkipDir {
				break
			}
			return err
		}
	}
	return nil
}

// ahead lists the folders among a folder's entries that the walk will go
// into, width at a time, each at its entry's index (nil where none was read; a
// nil slice when none was). A lone folder is left to be read when the walk
// reaches it, and so is every folder when width is below 2.
func (w walker) ahead(path string, entries []fs.DirEntry) []*listing {
	if w.width < 2 {
		return nil
	}
	var dirs []int
	for i, e := range entries {
		if e.IsDir() && (w.descend == nil || w.descend(filepath.Join(path, e.Name()), e)) {
			dirs = append(dirs, i)
		}
	}
	if len(dirs) < 2 {
		return nil
	}
	out := make([]*listing, len(entries))
	pool.Each(w.ctx, min(w.width, len(dirs)), dirs, func() bool { return true }, func(i *int) {
		out[*i] = w.list(filepath.Join(path, entries[*i].Name()))
	})
	return out
}

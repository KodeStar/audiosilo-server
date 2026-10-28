package library

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kodestar/audiosilo-server/internal/pool"
)

// walkIO is how many filesystem calls a discovery walk makes at once: folders
// listed ahead (walkDir) and audio files looked at (statAudio).
const walkIO = 8

// walkDir is filepath.WalkDir(root, fn): fn is called for the same paths, in the
// same order, with the same errors, and its SkipDir and SkipAll mean the same.
// What differs is when folders are listed: the folders a folder holds are listed
// together, walkIO at a time, before the walk goes into the first of them,
// rather than each as the walk reaches it. On a network share or a FUSE pool
// (Unraid's /mnt/user) every listing waits on a round trip, and one at a time
// they are most of a no-change scan. descend reports whether fn will go into a
// folder (nil: every one), so that none fn skips is listed ahead. Once ctx
// ends nothing more is listed ahead (fn sees ctx itself).
func walkDir(ctx context.Context, root string, descend func(path string, d fs.DirEntry) bool, fn fs.WalkDirFunc) error {
	info, err := os.Lstat(root)
	if err != nil {
		err = fn(root, nil, err)
	} else {
		err = walkIn(ctx, root, fs.FileInfoToDirEntry(info), nil, descend, fn)
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

// walkIn is filepath's walkDir: fn on path, then into it when it is a folder fn
// doesn't skip, using listed when its entries were read ahead (nil: read now).
func walkIn(ctx context.Context, path string, d fs.DirEntry, listed *listing, descend func(string, fs.DirEntry) bool, fn fs.WalkDirFunc) error {
	if err := fn(path, d, nil); err != nil || !d.IsDir() {
		if err == filepath.SkipDir && d.IsDir() {
			err = nil // skipped the folder
		}
		return err
	}
	if listed == nil {
		entries, err := os.ReadDir(path)
		listed = &listing{entries: entries, err: err}
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
	ahead := listAhead(ctx, path, listed.entries, descend)
	for i, d1 := range listed.entries {
		if err := walkIn(ctx, filepath.Join(path, d1.Name()), d1, ahead[i], descend, fn); err != nil {
			if err == filepath.SkipDir {
				break
			}
			return err
		}
	}
	return nil
}

// listAhead lists the folders among a folder's entries that the walk will go
// into, walkIO at a time, each at its entry's index (nil where none was
// read). A lone folder is left to be read when the walk reaches it.
func listAhead(ctx context.Context, path string, entries []fs.DirEntry, descend func(string, fs.DirEntry) bool) []*listing {
	var dirs []int
	for i, e := range entries {
		if e.IsDir() && (descend == nil || descend(filepath.Join(path, e.Name()), e)) {
			dirs = append(dirs, i)
		}
	}
	out := make([]*listing, len(entries))
	if len(dirs) < 2 {
		return out
	}
	pool.Each(ctx, min(walkIO, len(dirs)), dirs, func() bool { return true }, func(i *int) {
		sub, err := os.ReadDir(filepath.Join(path, entries[*i].Name()))
		out[*i] = &listing{entries: sub, err: err}
	})
	return out
}

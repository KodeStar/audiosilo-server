package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The add-library folder picker (GET /admin/fs/dirs). An admin choosing a
// library root needs to see the server's folders, not just the configured
// roots, so this lists any absolute directory. Its bounds:
//
//   - admin-only at the API (the same admin can already point a library at any
//     folder and browse it, so this reveals nothing new to that role);
//   - directory names only: never a file, size, owner or timestamp;
//   - hidden (dot) folders are skipped, like the scanner and /fs skip them;
//   - absolute paths only, cleaned, and at most `limit` entries per listing;
//   - a listing behind a deadline and a few slots (DirLister), so a mount that
//     stops answering can't hold the request, or pile up readers, for good.

// DirEntry is one folder in a DirListing.
type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"` // absolute, in the server's own path syntax
}

// DirListing is one server folder's subfolders.
type DirListing struct {
	Path string `json:"path"`
	// Parent is the folder above Path; empty at a filesystem root.
	Parent    string     `json:"parent,omitempty"`
	Dirs      []DirEntry `json:"dirs"`
	Truncated bool       `json:"truncated,omitempty"`
}

// ErrNotAbsolute rejects a relative path: the picker always names a folder from
// the filesystem root, never relative to wherever the server was started.
var ErrNotAbsolute = errors.New("path must be absolute")

// ListDirs lists the subfolders of dir (the filesystem root when dir is empty),
// sorted case-insensitively, at most limit of them. Symlinks that resolve to
// folders count as folders. An unreadable or missing dir returns the os error.
func ListDirs(dir string, limit int) (*DirListing, error) {
	if dir == "" {
		dir = filesystemRoot()
	}
	if !filepath.IsAbs(dir) {
		return nil, ErrNotAbsolute
	}
	dir = filepath.Clean(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := &DirListing{Path: dir, Dirs: []DirEntry{}}
	if parent := filepath.Dir(dir); parent != dir {
		out.Parent = parent
	}
	type dirName struct{ name, key string } // key: the lowercased sort key, computed once
	var names []dirName
	for _, e := range entries {
		if isHidden(e.Name()) {
			continue
		}
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(filepath.Join(dir, e.Name())) // follows the link
			isDir = err == nil && info.IsDir()
		}
		if isDir {
			names = append(names, dirName{e.Name(), strings.ToLower(e.Name())})
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i].key < names[j].key })
	if limit > 0 && len(names) > limit {
		names, out.Truncated = names[:limit], true
	}
	for _, n := range names {
		out.Dirs = append(out.Dirs, DirEntry{Name: n.name, Path: filepath.Join(dir, n.name)})
	}
	return out, nil
}

// ErrDirsTimedOut is a folder listing that didn't answer within its DirLister's
// time-out, or found every slot held by listings that haven't returned.
var ErrDirsTimedOut = errors.New("folder listing timed out")

// DirLister runs folder listings each on a goroutine of its own, waited for at
// most its time-out: a readdir on a network share that stops answering can't be
// interrupted (server#224). At most its slots run at once, a hung one keeping
// its slot until it returns, so hung reads can't pile up: with every slot held a
// listing waits for one only as long as its time-out.
type DirLister struct {
	list    func(dir string, limit int) (*DirListing, error)
	slots   chan struct{}
	timeout time.Duration
}

// NewDirLister is a DirLister of list (ListDirs; a test's stand-in), slots at
// once, each waited for at most timeout.
func NewDirLister(list func(dir string, limit int) (*DirListing, error), slots int, timeout time.Duration) *DirLister {
	return &DirLister{list: list, slots: make(chan struct{}, slots), timeout: timeout}
}

// List is list(dir, limit), or ErrDirsTimedOut once the time-out (or ctx) ends
// first, the listing left to finish on its own goroutine.
func (l *DirLister) List(ctx context.Context, dir string, limit int) (*DirListing, error) {
	if dir != "" && !filepath.IsAbs(dir) {
		return nil, ErrNotAbsolute // refused at once, never waiting for a slot
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ErrDirsTimedOut
	}
	type result struct {
		listing *DirListing
		err     error
	}
	done := make(chan result, 1) // buffered: an abandoned listing still returns
	go func() {
		defer func() { <-l.slots }()
		listing, err := l.list(dir, limit)
		done <- result{listing, err}
	}()
	select {
	case r := <-done:
		return r.listing, r.err
	case <-ctx.Done():
		return nil, ErrDirsTimedOut
	}
}

// filesystemRoot is "/" on Unix and the working directory's drive (C:\) on Windows.
func filesystemRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return string(filepath.Separator)
	}
	return filepath.VolumeName(wd) + string(filepath.Separator)
}

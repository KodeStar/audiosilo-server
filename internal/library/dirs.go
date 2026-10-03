package library

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The add-library folder picker (GET /admin/fs/dirs). An admin choosing a
// library root needs to see the server's folders, not just the configured
// roots, so this lists any absolute directory. Its bounds:
//
//   - admin-only at the API (the same admin can already point a library at any
//     folder and browse it, so this reveals nothing new to that role);
//   - directory names only: never a file, size, owner or timestamp;
//   - hidden (dot) folders are skipped, like the scanner and /fs skip them;
//   - absolute paths only, cleaned, and at most `limit` entries per listing.

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
	for _, e := range entries {
		if isHidden(e.Name()) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(full) // follows the link
			isDir = err == nil && info.IsDir()
		}
		if isDir {
			out.Dirs = append(out.Dirs, DirEntry{Name: e.Name(), Path: full})
		}
	}
	sort.Slice(out.Dirs, func(i, j int) bool {
		return strings.ToLower(out.Dirs[i].Name) < strings.ToLower(out.Dirs[j].Name)
	})
	if limit > 0 && len(out.Dirs) > limit {
		out.Dirs, out.Truncated = out.Dirs[:limit], true
	}
	return out, nil
}

// filesystemRoot is "/" on Unix and the working directory's drive (C:\) on Windows.
func filesystemRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return string(filepath.Separator)
	}
	return filepath.VolumeName(wd) + string(filepath.Separator)
}

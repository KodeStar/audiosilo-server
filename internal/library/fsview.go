// Package library provides the filesystem view (instant directory browsing) and
// the background scanner that builds the computed/hybrid index from file
// metadata. The filesystem view requires no prior indexing, so a freshly
// connected client can browse immediately.
package library

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// ErrOutsideRoot is returned when a requested path escapes the library root.
var ErrOutsideRoot = errors.New("path escapes library root")

// Entry is one item in a directory listing. The Book* fields are populated
// (hybrid view) when the entry corresponds to an indexed book — a file for
// flat/books_in_folder libraries, or a folder for chapters_in_folder — so a
// browsing client can act on it directly via /books/{book_id}/...
type Entry struct {
	Name    string `json:"name"`
	Path    string `json:"path"` // path relative to the library root, slash-separated
	IsDir   bool   `json:"is_dir"`
	IsAudio bool   `json:"is_audio"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`

	// Indexed-book annotations (the hybrid view). Act on a book by its Path.
	IsBook      bool    `json:"is_book,omitempty"`
	Title       string  `json:"title,omitempty"`
	Author      string  `json:"author,omitempty"`
	Series      string  `json:"series,omitempty"`
	SeriesIndex float64 `json:"series_index,omitempty"`
	Duration    float64 `json:"duration,omitempty"`

	// Override is the explicit per-folder detection override set for this
	// directory ("book" or "collection"), empty when auto-detected. Surfaced so
	// the admin console can show and toggle it.
	Override string `json:"override,omitempty"`
}

// Listing is a page of directory entries.
type Listing struct {
	Path       string  `json:"path"`
	Entries    []Entry `json:"entries"`
	Total      int     `json:"total"`
	Offset     int     `json:"offset"`
	NextOffset int     `json:"next_offset,omitempty"`
}

// SafeJoin resolves relPath against root, guaranteeing the result stays within
// root. It defends against ".." traversal, absolute-path injection, and symlinks
// inside the root that point outside it.
func SafeJoin(root, relPath string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	// Resolve symlinks in the root itself so containment is checked against the
	// real directory (the root is operator-configured and expected to exist).
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}
	full := filepath.Join(rootAbs, filepath.FromSlash(relPath))
	// Lexical containment first: reject ".." traversal. An absolute relPath is
	// treated as relative to the root by filepath.Join, so it stays contained;
	// only genuine escapes produce a ".." relative path. This also covers paths
	// that don't exist yet (which EvalSymlinks can't resolve).
	if !withinRoot(rootAbs, full) {
		return "", ErrOutsideRoot
	}
	// Symlink-aware containment: resolve symlinks in the longest existing prefix
	// of the target and re-check, so a symlink inside the root that points
	// outside it is rejected rather than followed.
	if !withinRoot(rootAbs, resolveExisting(full)) {
		return "", ErrOutsideRoot
	}
	return full, nil
}

// withinRoot reports whether p is the root itself or nested under it.
func withinRoot(rootAbs, p string) bool {
	rel, err := filepath.Rel(rootAbs, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolveExisting resolves symlinks in the longest existing prefix of p and
// re-appends the (not-yet-existing) remainder, so containment can be checked even
// for a path that has not been created yet.
func resolveExisting(p string) string {
	rest := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // nothing along the path exists to resolve
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// BrowseFS lists a directory within a library root with offset pagination.
// relPath "" (or "/") lists the root. Directories sort before files, both
// alphabetically, giving a stable order for paging. If allow is non-nil, only
// entries for which it returns true are included (applied before pagination so
// pages stay full) — used to scope browsing to a share's path rules.
func BrowseFS(root, relPath string, offset, limit int, allow func(relPath string) bool) (*Listing, error) {
	full, err := SafeJoin(root, relPath)
	if err != nil {
		return nil, err
	}
	dirEntries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	entries := make([]Entry, 0, len(dirEntries))
	cleanRel := strings.Trim(filepath.ToSlash(filepath.Clean("/"+relPath)), "/")
	for _, de := range dirEntries {
		name := de.Name()
		if strings.HasPrefix(name, ".") {
			continue // hide dotfiles
		}
		childRel := name
		if cleanRel != "" {
			childRel = cleanRel + "/" + name
		}
		if allow != nil && !allow(childRel) {
			continue // outside the caller's share scope
		}
		if !de.IsDir() && !metadata.IsAudio(name) {
			continue // hide non-audio files; clicking one can't open a book
		}
		isDir := de.IsDir()
		// os.ReadDir already provides name + type for free; only files need a
		// per-entry stat (for Size, used to compute bitrate). Skipping it for
		// directories avoids one network round-trip per entry — the difference
		// between a snappy and a multi-second author listing on a network mount.
		var size, modTime int64
		if !isDir {
			if info, err := de.Info(); err == nil {
				size = info.Size()
				modTime = info.ModTime().Unix()
			}
		}
		entries = append(entries, Entry{
			Name:    name,
			Path:    childRel,
			IsDir:   isDir,
			IsAudio: !isDir && metadata.IsAudio(name),
			Size:    size,
			ModTime: modTime,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir // dirs first
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	total := len(entries)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	out := &Listing{Path: cleanRel, Entries: entries[offset:end], Total: total, Offset: offset}
	if end < total {
		out.NextOffset = end
	}
	return out, nil
}

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

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// ErrOutsideRoot is returned when a requested path escapes the library root.
var ErrOutsideRoot = errors.New("path escapes library root")

// Entry is one item in a directory listing. The Book* fields are populated by
// the API layer (annotateWithBooks) when the entry resolves to an indexed book
// under the current per-folder detection (there is no library-wide layout). A
// browsing client acts on the entry by its Path (?path=), never a book id.
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

	// SplitDiscs marks a folder whose audio is only in disc folders directly in it
	// (CD1, CD2...), each indexed as its own book: a `book` override joins them into
	// one (see discSets). Surfaced so the admin console offers that only here; set
	// for an admin's request only (annotateWithBooks), never on a player's listing.
	SplitDiscs bool `json:"split_discs,omitempty"`
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

// BrowseFS lists a directory within a library root with offset pagination: a
// page of ListDir, its files with their Size and ModTime. relPath "" (or "/")
// lists the root.
func BrowseFS(root, relPath string, offset, limit int, allow func(relPath string) bool, ignore *Ignore) (*Listing, error) {
	full, entries, err := listDir(root, relPath, allow, ignore)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	total := len(entries)
	offset = min(max(offset, 0), total)
	end := min(offset+limit, total)
	page := entries[offset:end]
	// Only files need a stat (for Size, used to compute bitrate), and only the
	// page's: one round-trip per entry is the difference between a snappy and a
	// multi-second listing on a network mount.
	for i := range page {
		if page[i].IsDir {
			continue
		}
		if info, err := os.Lstat(filepath.Join(full, page[i].Name)); err == nil {
			page[i].Size = info.Size()
			page[i].ModTime = info.ModTime().Unix()
		}
	}
	out := &Listing{Path: catalog.CleanRelPath(relPath), Entries: page, Total: total, Offset: offset}
	if end < total {
		out.NextOffset = end
	}
	return out, nil
}

// ListDir is a directory within a library root as the player browses it, whole
// and without Size or ModTime: hidden and non-audio files left out, and so is
// whatever allow refuses (a share's path rules; nil allows everything) or the
// library's ignore rules skip, as the scanner skips it. Directories sort before
// files, both by name, case folded, a stable order for paging.
func ListDir(root, relPath string, allow func(relPath string) bool, ignore *Ignore) ([]Entry, error) {
	_, entries, err := listDir(root, relPath, allow, ignore)
	return entries, err
}

// listDir is ListDir, with the directory's absolute path.
func listDir(root, relPath string, allow func(relPath string) bool, ignore *Ignore) (string, []Entry, error) {
	full, err := SafeJoin(root, relPath)
	if err != nil {
		return "", nil, err
	}
	dirEntries, err := os.ReadDir(full)
	if err != nil {
		return "", nil, err
	}
	entries := make([]Entry, 0, len(dirEntries))
	// The canonical rel path prefixes each entry's Path; scope checks and
	// persisted path keys rely on this same form (see catalog.CleanRelPath).
	cleanRel := catalog.CleanRelPath(relPath)
	for _, de := range dirEntries {
		name := de.Name()
		if isHidden(name) {
			continue // hidden here AND skipped by the scanner (see isHidden)
		}
		childRel := name
		if cleanRel != "" {
			childRel = cleanRel + "/" + name
		}
		if allow != nil && !allow(childRel) {
			continue // outside the caller's share scope
		}
		isDir := de.IsDir()
		if !isDir && !metadata.IsAudio(name) {
			continue // hide non-audio files; clicking one can't open a book
		}
		if ignore.Covers(childRel, isDir) {
			continue // skipped by the library's ignore rules, here and by the scanner
		}
		entries = append(entries, Entry{Name: name, Path: childRel, IsDir: isDir, IsAudio: !isDir})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir // dirs first
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return full, entries, nil
}

// MarkBooks fills in the entries the index holds as books (books by path, as
// catalog.BooksByPaths returns them): IsBook and the book's metadata.
func MarkBooks(entries []Entry, books map[string]catalog.Book) {
	for i := range entries {
		e := &entries[i]
		if b, ok := books[e.Path]; ok {
			e.IsBook = true
			e.Title = b.Title
			e.Author = b.Author
			e.Series = b.Series
			e.SeriesIndex = b.SeriesIndex
			e.Duration = b.Duration
		}
	}
}

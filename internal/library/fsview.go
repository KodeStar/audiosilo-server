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
// root. It defends against ".." traversal and absolute-path injection.
func SafeJoin(root, relPath string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(rootAbs, filepath.FromSlash(relPath))
	// Reject any path that resolves outside the root (".." traversal). An
	// absolute relPath is treated as relative to the root by filepath.Join, so
	// it stays contained; only genuine escapes produce a ".." relative path.
	rel, err := filepath.Rel(rootAbs, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrOutsideRoot
	}
	return full, nil
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
		info, err := de.Info()
		if err != nil {
			continue
		}
		childRel := name
		if cleanRel != "" {
			childRel = cleanRel + "/" + name
		}
		if allow != nil && !allow(childRel) {
			continue // outside the caller's share scope
		}
		entries = append(entries, Entry{
			Name:    name,
			Path:    childRel,
			IsDir:   de.IsDir(),
			IsAudio: !de.IsDir() && metadata.IsAudio(name),
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
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

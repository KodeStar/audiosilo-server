// Package relpath holds the rules for library-relative paths shared by catalog
// (a share's path rules, every persisted path key) and auth (a cast token's
// book): how a path is cleaned, and whether one lies within another. It is its
// own package so that auth, which depends only on store, need not import
// catalog's whole graph (media, metadata, match) for a few lines of string logic.
package relpath

import (
	"path"
	"path/filepath"
	"strings"
)

// Clean canonicalizes a library-relative path: separators become slashes, "."
// and ".." segments collapse (a leading ".." clamps at the root instead of
// escaping it), and enclosing slashes are trimmed - "" addresses the library
// root. Security-relevant: Within is a literal prefix match, so a path must be
// cleaned before it is judged ("Author A/../Author B/x.m4b" would otherwise pass
// an "Author A" rule). catalog.CleanRelPath is this.
func Clean(p string) string {
	return strings.Trim(path.Clean("/"+filepath.ToSlash(p)), "/")
}

// Within reports whether p is root itself or lies below it on a segment
// boundary ("Author/Book" holds "Author/Book/01.mp3", never "Author/Book 2"), and
// "" (the library root) holds everything. Both paths must already be clean
// (Clean): this is a literal prefix match, so an uncleaned
// "Author/Book/../Other" would pass. Security-critical: share scopes and cast
// tokens both stop here.
func Within(root, p string) bool {
	return root == "" || p == root || strings.HasPrefix(p, root+"/")
}

package library

import (
	"os"
	"path"
	"strings"
)

// BookEbook finds the ebook that sits beside a book's audio, the one the
// Bridge's sidecars.generate reads in place of the audio ("auto" source): a
// regular .epub file (never a link, never hidden) directly in the book's folder.
// A folder book's folder is the book itself (relPath); a single-file book's is
// the folder holding the file, and there only an epub named like the audio file
// (the same stem: "Book.m4b" and "Book.epub") counts, since that folder may hold
// other books. It returns the epub's library-relative path, the first by name,
// or "" when there is none or the folder can't be read (through SafeJoin, so
// never outside root).
func BookEbook(root, relPath string, folder bool) string {
	dir, stem := relPath, ""
	if !folder {
		dir = path.Dir(relPath)
		if dir == "." {
			dir = ""
		}
		base := path.Base(relPath)
		stem = strings.TrimSuffix(base, path.Ext(base))
	}
	full, err := SafeJoin(root, dir)
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return ""
	}
	for _, e := range entries { // sorted by name
		name := e.Name()
		ext := path.Ext(name)
		if strings.HasPrefix(name, ".") || !strings.EqualFold(ext, ".epub") || !e.Type().IsRegular() {
			continue
		}
		if !folder && strings.TrimSuffix(name, ext) != stem {
			continue
		}
		return path.Join(dir, name)
	}
	return ""
}

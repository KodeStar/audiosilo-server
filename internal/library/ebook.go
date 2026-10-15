package library

import (
	"os"
	"path"
	"strings"
)

// BookEbook finds the ebook that sits beside a book's audio, the one the
// Bridge's sidecars.generate reads in place of the audio ("auto" source), by the
// Bridge's own rule (its generate.epubBeside), so the estimate quotes the run the
// Bridge will make:
//   - a folder book (its folder is relPath): the one regular .epub directly in
//     it (never a link, never hidden; the extension in any case). Several is
//     none: the Bridge won't pick one and reads the audio instead.
//   - a single-file book: exactly "<stem>.epub" beside it (the audio file's name
//     without its extension: "Book.m4b" and "Book.epub"), since that folder may
//     hold other books; a link to a file inside the library counts, as the
//     Bridge follows one.
//
// It returns the epub's library-relative path, or "" when there is none or it
// can't be looked at (through ContentJoin, so never outside root or in a hidden
// folder).
func BookEbook(root, relPath string, folder bool) string {
	if !folder {
		rel := path.Join(dirOf(relPath), stemOf(relPath)+".epub")
		full, err := ContentJoin(root, rel)
		if err != nil {
			return ""
		}
		if info, err := os.Stat(full); err != nil || !info.Mode().IsRegular() {
			return ""
		}
		return rel
	}
	full, err := ContentJoin(root, relPath)
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return ""
	}
	found := ""
	for _, e := range entries {
		name := e.Name()
		if isHidden(name) || !strings.EqualFold(path.Ext(name), ".epub") || !e.Type().IsRegular() {
			continue
		}
		if found != "" {
			return "" // several: the Bridge reads the audio
		}
		found = path.Join(relPath, name)
	}
	return found
}

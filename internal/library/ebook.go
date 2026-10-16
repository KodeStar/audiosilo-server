package library

import (
	"archive/zip"
	"os"
	"path"
	"path/filepath"
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
// The one it finds must also be readable (readableEpub), as the Bridge falls
// back to the audio for an epub it can't read (a DRM-protected or corrupt one).
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
		if info, err := os.Stat(full); err != nil || !info.Mode().IsRegular() || !readableEpub(full) {
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
	// Several are counted before this, readable or not: the Bridge counts them
	// the same way.
	if found != "" && !readableEpub(filepath.Join(full, path.Base(found))) {
		return ""
	}
	return found
}

// readableEpub is the cheap stand-in for the Bridge's check (generate.readEpub,
// audiosilo-meta's extract.ReadMetadata, which opens the zip and parses the OPF
// that META-INF/container.xml names): the file opens as a zip whose directory
// lists META-INF/container.xml. Like the Bridge it doesn't judge
// META-INF/encryption.xml: publishers obfuscate fonts with it in DRM-free books,
// and a really encrypted book parks ebook_unreadable on the Bridge, saying so.
// Only the zip's central directory is read, never an entry, so a large epub
// costs a few reads.
func readableEpub(full string) bool {
	f, err := os.Open(full) //nolint:gosec // full is ContentJoin'd under the library root
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	// Any error (not a zip, a truncated one, an insecure entry name) is one the
	// Bridge's zip.OpenReader would refuse too.
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return false
	}
	for _, e := range zr.File {
		if e.Name == "META-INF/container.xml" {
			return true
		}
	}
	return false
}

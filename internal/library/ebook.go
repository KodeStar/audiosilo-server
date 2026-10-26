package library

import (
	"archive/zip"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/catalog"
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
//
// The scan records the same rule (books.ebook_path, what the reader opens) from
// the folder listings its walk already reads (ebookCandidate over the walk's
// .epub entries, then checkEbook), so the reader and the Bridge always agree.
func BookEbook(root, relPath string, folder bool) string {
	rel, entry := ebookOnDisk(root, relPath, folder)
	if rel == "" {
		return ""
	}
	r, _ := checkEbook(root, catalog.EbookReading{Path: rel, Sig: ebookSig(root, rel, entry)})
	return r.Path
}

// ebookOnDisk is ebookCandidate over the folder holding relPath's ebook, listed
// now ("" when there is none or the folder can't be listed): for a book no walk
// found (resolveBook) and for BookEbook.
func ebookOnDisk(root, relPath string, folder bool) (string, fs.DirEntry) {
	full, err := ContentJoin(root, ebookDir(relPath, folder))
	if err != nil {
		return "", nil
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return "", nil
	}
	var epubs []fs.DirEntry
	for _, e := range entries {
		if isEpubEntry(e) {
			epubs = append(epubs, e)
		}
	}
	return ebookCandidate(relPath, folder, epubs)
}

// ebookDir is the library-relative folder holding a book's ebook: a folder book's
// own ("" for a `book` override on the library root), a single-file book's.
func ebookDir(relPath string, folder bool) string {
	if folder {
		if relPath == "." {
			return ""
		}
		return relPath
	}
	return dirOf(relPath)
}

// isEpubEntry reports whether a directory entry is one BookEbook's rule looks at:
// not a folder, not hidden, named .epub in any case. Links are kept (a
// single-file book's may be one); ebookCandidate decides.
func isEpubEntry(de fs.DirEntry) bool {
	return !de.IsDir() && !isHidden(de.Name()) && strings.EqualFold(path.Ext(de.Name()), ".epub")
}

// ebookCandidate is BookEbook's rule before the readability check, over epubs,
// the .epub entries (isEpubEntry) of the folder holding the book's ebook
// (ebookDir): its library-relative path and entry, "" for none.
//   - a folder book: the one regular file (a link is never one); several is none.
//   - a single-file book: the entry named exactly "<stem>.epub", whatever it is
//     (a link counts when it leads to a regular file inside the library:
//     checkEbook).
func ebookCandidate(relPath string, folder bool, epubs []fs.DirEntry) (string, fs.DirEntry) {
	dir := ebookDir(relPath, folder)
	if !folder {
		want := stemOf(relPath) + ".epub"
		for _, e := range epubs {
			if e.Name() == want {
				return path.Join(dir, want), e
			}
		}
		return "", nil
	}
	var found fs.DirEntry
	for _, e := range epubs {
		if !e.Type().IsRegular() {
			continue
		}
		if found != nil {
			return "", nil // several: the Bridge reads the audio
		}
		found = e
	}
	if found == nil {
		return "", nil
	}
	return path.Join(dir, found.Name()), found
}

// ebookSig is the signature (catalog.EbookSig) of the candidate rel, entry its
// listing entry: the file's own for a regular file (no further read), the
// target's for a link inside the library, else the link's own; "" when it can't
// be looked at now (gone since it was listed), so the next look tries again.
func ebookSig(root, rel string, entry fs.DirEntry) string {
	info, err := entry.Info()
	if err != nil {
		return ""
	}
	if !info.Mode().IsRegular() {
		if full, jerr := ContentJoin(root, rel); jerr == nil {
			if target, serr := os.Stat(full); serr == nil {
				info = target
			}
		}
	}
	return catalog.EbookSig(path.Base(rel), info)
}

// checkEbook finishes a candidate (r: its path and signature; Path "" for none)
// into what is recorded: Path kept only when it is a readable epub (a regular
// file inside the library, readableEpub), so an unreadable one is recorded by its
// signature alone and not opened again until it changes. ok is false when it
// couldn't be read right now (it won't open, a flaky mount): the caller keeps
// what is stored, or records none with no signature, so the next scan looks again.
func checkEbook(root string, r catalog.EbookReading) (_ catalog.EbookReading, ok bool) {
	switch {
	case r.Path == "":
		return catalog.EbookReading{}, true // none
	case r.Sig == "":
		return catalog.EbookReading{}, false // gone since it was listed
	}
	full, err := ContentJoin(root, r.Path)
	if err != nil {
		return catalog.EbookReading{Sig: r.Sig}, true // a link leading out of the library
	}
	info, err := os.Stat(full)
	if err != nil {
		return catalog.EbookReading{}, false
	}
	if !info.Mode().IsRegular() {
		return catalog.EbookReading{Sig: r.Sig}, true
	}
	readable, err := readableEpub(full)
	if err != nil {
		return catalog.EbookReading{}, false
	}
	if !readable {
		r.Path = ""
	}
	return r, true
}

// readableEpub is the cheap stand-in for the Bridge's check (generate.readEpub,
// audiosilo-meta's extract.ReadMetadata, which opens the zip and parses the OPF
// that META-INF/container.xml names): the file opens as a zip whose directory
// lists META-INF/container.xml. Like the Bridge it doesn't judge
// META-INF/encryption.xml: publishers obfuscate fonts with it in DRM-free books,
// and a really encrypted book parks ebook_unreadable on the Bridge, saying so.
// Only the zip's central directory is read, never an entry, so a large epub
// costs a few reads. A file that isn't a readable epub is (false, nil); one that
// can't be opened or stat'ed right now is an error, which the scan looks at again
// rather than recording.
func readableEpub(full string) (bool, error) {
	f, err := os.Open(full) //nolint:gosec // full is ContentJoin'd under the library root
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	// Any error (not a zip, a truncated one, an insecure entry name) is one the
	// Bridge's zip.OpenReader would refuse too.
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return false, nil
	}
	for _, e := range zr.File {
		if e.Name == "META-INF/container.xml" {
			return true, nil
		}
	}
	return false, nil
}

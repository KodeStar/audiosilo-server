package library

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// BookEbook finds the ebook that sits beside a book's audio, the one the
// Bridge's sidecars.generate reads in place of the audio ("auto" source), by the
// Bridge's own rule (its generate.epubBeside), so the estimate quotes the run the
// Bridge will make:
//   - a folder book (its folder is relPath): the one regular .epub directly in
//     it (never a link, never hidden; the extension in any case). Several is
//     none: the Bridge won't pick one and reads the audio instead.
//   - a single-file book: "<stem>.epub" beside it (the audio file's name
//     without its extension: "Book.m4b" and "Book.epub"), since that folder may
//     hold other books: what the disk opens by that name, as the Bridge's
//     os.Stat of it does (exactly that name on a case-sensitive disk, any case
//     of it on a case-insensitive one); a link to a file inside the library
//     counts, as the Bridge follows one.
//
// The one it finds must also be readable (readableEpub), as the Bridge falls
// back to the audio for an epub it can't read (not a zip, a corrupt one, or one
// naming no META-INF/container.xml).
//
// It returns the epub's library-relative path, or "" when there is none or it
// can't be looked at (through ContentJoin, so never outside root or in a hidden
// folder).
//
// The scan records the same rule (books.ebook_path, what the reader opens) from
// the folder listings its walk already reads (ebookCandidate over the walk's
// .epub entries, then checkEbook), so the reader and the Bridge always agree.
func BookEbook(root, relPath string, folder bool) string {
	r, _ := checkEbook(root, ebookOnDisk(root, relPath, folder))
	return r.Path
}

// ebookOnDisk is BookEbook's rule over what is on disk now, before the
// readability check, for a book no walk found (an on-demand index) and for
// BookEbook: a folder book's folder listed for ebookCandidate, a single-file
// book's "<stem>.epub" looked up by that name (ebookNamed: one Lstat, not a
// listing of a folder other books share, unless a name with non-ASCII letters
// isn't there by that spelling: ebookCandidate over the folder's epubs then
// finds it in another Unicode form). Unread when it can't be looked at right
// now (the folder won't list), so what is stored for it stays.
func ebookOnDisk(root, relPath string, folder bool) catalog.EbookReading {
	dir := sidecarDir(relPath, folder)
	if !folder {
		name := ebookName(relPath)
		r := ebookNamed(root, dir, name)
		if r.Sig != "" || r.Unread || isASCII(name) {
			return r
		}
		// Not there by that name, and a name with accents may be spelled in
		// another Unicode form on disk (an NFD copy from macOS beside NFC audio
		// on a disk that tells them apart): look through the folder's epubs.
		return ebookListed(root, relPath, folder)
	}
	return ebookListed(root, relPath, folder)
}

// ebookListed is ebookCandidate over the .epub entries of the folder holding the
// book's ebook, listed now: none when the folder is gone, Unread when it won't
// list right now.
func ebookListed(root, relPath string, folder bool) catalog.EbookReading {
	full, err := ContentJoin(root, sidecarDir(relPath, folder))
	if err != nil {
		return catalog.EbookReading{}
	}
	entries, err := os.ReadDir(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return catalog.EbookReading{}
	case err != nil:
		return catalog.EbookReading{Unread: true}
	}
	var epubs []fs.DirEntry
	for _, e := range entries {
		if isEpubEntry(e) {
			epubs = append(epubs, e)
		}
	}
	return ebookCandidate(root, relPath, folder, epubs)
}

// ebookName is the name of a single-file book's ebook: its audio file's stem
// with ".epub" ("A/Book.m4b" -> "Book.epub").
func ebookName(relPath string) string { return stemOf(relPath) + ".epub" }

// ebookNameKey is the key a listed name matches a single-file book's ebookName
// by: its NFC form, lower-cased, so the walk hands over entries named in another
// case or Unicode form of it (ebookCandidate decides which counts).
func ebookNameKey(name string) string { return strings.ToLower(norm.NFC.String(name)) }

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// isEpubEntry reports whether a directory entry is one BookEbook's rule looks at:
// not a folder, not hidden, named .epub in any case. Links are kept (a
// single-file book's may be one); ebookCandidate decides.
func isEpubEntry(de fs.DirEntry) bool {
	return !de.IsDir() && !isHidden(de.Name()) && strings.EqualFold(path.Ext(de.Name()), ".epub")
}

// ebookCandidate is BookEbook's rule before the readability check, over epubs,
// the .epub entries (isEpubEntry) of the folder holding the book's ebook
// (sidecarDir): its library-relative path and signature, the zero reading for
// none.
//   - a folder book: the one regular file (a link is never one); several is none.
//   - a single-file book: "<stem>.epub" (ebookName), whatever it is (a link
//     counts when it leads to a regular file inside the library: checkEbook):
//     the entry of exactly that name, else the one named in another Unicode
//     form of it (NFC/NFD), else, when one is named in another case of
//     it, what the disk opens by that name (ebookNamed: a case-insensitive
//     disk's "Book.EPUB", as the Bridge's os.Stat finds it; nothing on a
//     case-sensitive one), recorded under that name. Only entries named in some
//     case of it matter, so the walk hands over just those.
func ebookCandidate(root, relPath string, folder bool, epubs []fs.DirEntry) catalog.EbookReading {
	dir := sidecarDir(relPath, folder)
	if !folder {
		name := ebookName(relPath)
		for _, e := range epubs {
			if e.Name() == name {
				return ebookEntry(root, path.Join(dir, name), e)
			}
		}
		// The same name in another Unicode form (NFD "Cafe\u0301.epub" beside
		// NFC "Café.m4b"): that entry, recorded under its own name.
		nfc := norm.NFC.String(name)
		for _, e := range epubs {
			if norm.NFC.String(e.Name()) == nfc {
				return ebookEntry(root, path.Join(dir, e.Name()), e)
			}
		}
		for _, e := range epubs {
			if strings.EqualFold(e.Name(), name) {
				return ebookNamed(root, dir, name)
			}
		}
		return catalog.EbookReading{}
	}
	var one fs.DirEntry
	for _, e := range epubs {
		if !e.Type().IsRegular() {
			continue
		}
		if one != nil {
			return catalog.EbookReading{} // several: the Bridge reads the audio
		}
		one = e
	}
	if one == nil {
		return catalog.EbookReading{}
	}
	return ebookEntry(root, path.Join(dir, one.Name()), one)
}

// ebookNamed is the candidate the disk opens by name in the library-relative
// folder dir (os.Lstat: the entry itself, a link not followed), as the Bridge
// finds a single-file book's "<stem>.epub", recorded under that name: none when
// nothing is there, Unread when it can't be looked up right now.
func ebookNamed(root, dir, name string) catalog.EbookReading {
	full, err := ContentJoin(root, dir)
	if err != nil {
		return catalog.EbookReading{}
	}
	info, err := os.Lstat(filepath.Join(full, name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return catalog.EbookReading{}
	case err != nil:
		return catalog.EbookReading{Unread: true}
	case info.IsDir():
		return catalog.EbookReading{}
	}
	return ebookFound(root, path.Join(dir, name), info)
}

// ebookEntry is ebookFound for a listed entry: Sig "" when its info can't be read
// now (gone since it was listed), so the next look tries again (checkEbook).
func ebookEntry(root, rel string, entry fs.DirEntry) catalog.EbookReading {
	info, err := entry.Info()
	if err != nil {
		return catalog.EbookReading{Path: rel}
	}
	return ebookFound(root, rel, info)
}

// ebookFound is the candidate at library-relative rel, info its entry's own
// (not followed): its signature (catalog.EbookSig) is the file's for a regular
// file (no further read), the target's for a link inside the library, else the
// link's own.
func ebookFound(root, rel string, info fs.FileInfo) catalog.EbookReading {
	if !info.Mode().IsRegular() {
		if full, err := ContentJoin(root, rel); err == nil {
			if target, err := os.Stat(full); err == nil {
				info = target
			}
		}
	}
	return catalog.EbookReading{Path: rel, Sig: catalog.EbookSig(path.Base(rel), info)}
}

// checkEbook finishes a candidate (r: its path and signature; Path "" for none)
// into what is recorded: Path kept only when it is a readable epub (a regular
// file inside the library, readableEpub). One that isn't (unreadable, a link out
// of the library or to nothing, anything but a regular file) is recorded by its
// signature alone, so a scan that finds its book unchanged doesn't open it again
// until it changes. ok is false when it couldn't be read right now (Unread, gone
// since it was listed, it won't open, a read fails on a flaky mount): the caller
// keeps what is stored, so the next scan looks again.
func checkEbook(root string, r catalog.EbookReading) (_ catalog.EbookReading, ok bool) {
	switch {
	case r.Unread:
		return catalog.EbookReading{}, false
	case r.Path == "":
		return catalog.EbookReading{}, true // none
	case r.Sig == "":
		return catalog.EbookReading{}, false // gone since it was listed
	}
	f, info, err := OpenEbook(root, r.Path, false)
	switch {
	case errors.Is(err, ErrNoEbook):
		return catalog.EbookReading{Sig: r.Sig}, true
	case err != nil:
		return catalog.EbookReading{}, false
	}
	defer func() { _ = f.Close() }()
	readable, err := readableEpub(f, info.Size())
	if err != nil {
		return catalog.EbookReading{}, false
	}
	if !readable {
		r.Path = ""
	}
	return r, true
}

// ErrNoEbook is OpenEbook's answer for a name that holds no ebook to open: one
// leading out of the library or through a hidden folder, a link where none may
// be or one leading nowhere, anything but a regular file (a folder, a FIFO), or
// a file swapped for another while it was opened.
var ErrNoEbook = errors.New("library: no regular file inside the library there")

// OpenEbook opens the ebook at library-relative rel for reading, with its info:
// a regular file inside the library, checked as one before it is opened (a
// FIFO's open would wait for a writer) and, once open, still the very file rel
// leads to inside the library (os.SameFile), so a link swapped in or pointed
// elsewhere meanwhile is never followed out. noLink refuses a link at rel itself
// (a folder book's ebook is never one); otherwise a link counts while it leads to
// a regular file inside the library. ErrNoEbook when no such file is there,
// fs.ErrNotExist when nothing is; any other error is one reading it right now (a
// permission, a flaky mount).
func OpenEbook(root, rel string, noLink bool) (*os.File, fs.FileInfo, error) {
	full, _, err := lookEbook(root, rel, noLink)
	if err != nil {
		return nil, nil, err
	}
	f, err := openEbookFile(full)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	if err == nil {
		// Looked at again now it is open, the name must still lead, inside the
		// library, to this very file: nothing was swapped in meanwhile.
		var again fs.FileInfo
		if _, again, err = lookEbook(root, rel, noLink); err == nil && !os.SameFile(again, opened) {
			err = fmt.Errorf("%w: it changed as it was opened", ErrNoEbook)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, opened, nil
}

// openEbookFile opens OpenEbook's file; a test swaps the file as it is opened.
var openEbookFile = func(full string) (*os.File, error) {
	return os.Open(full) //nolint:gosec // ContentJoin'd under the library root, a regular file when looked at
}

// lookEbook is the regular file rel names inside the library by OpenEbook's
// rules: its joined path and its info (a link's target's, unless noLink).
func lookEbook(root, rel string, noLink bool) (string, fs.FileInfo, error) {
	full, err := ContentJoin(root, rel)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrNoEbook, err)
	}
	stat := os.Stat
	if noLink {
		stat = os.Lstat
	}
	info, err := stat(full)
	switch {
	case err == nil && info.Mode().IsRegular():
		return full, info, nil
	case err == nil:
		return "", nil, ErrNoEbook
	case errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ELOOP):
		return "", nil, fmt.Errorf("%w: %w", ErrNoEbook, err)
	case !noLink && errors.Is(err, fs.ErrNotExist):
		if _, lerr := os.Lstat(full); lerr == nil {
			return "", nil, fmt.Errorf("%w: a link leading nowhere", ErrNoEbook)
		}
	}
	return "", nil, err
}

// readableEpub is the cheap stand-in for the Bridge's check (generate.readEpub,
// audiosilo-meta's extract.ReadMetadata, which opens the zip and parses the OPF
// that META-INF/container.xml names): r (size bytes) is a zip whose directory
// lists META-INF/container.xml. Like the Bridge it judges neither DRM nor
// META-INF/encryption.xml: publishers obfuscate fonts with it in DRM-free books,
// and a really encrypted book parks ebook_unreadable on the Bridge, saying so.
// Only the zip's central directory is read, never an entry, so a large epub
// costs a few reads. A file that isn't a readable epub is (false, nil); a read
// the disk fails (an *fs.PathError, os.File's: a flaky mount) is an error, which
// the scan looks at again rather than recording.
func readableEpub(r io.ReaderAt, size int64) (bool, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		var diskErr *fs.PathError
		if errors.As(err, &diskErr) {
			return false, err
		}
		// Any other error (not a zip, a truncated or corrupt one, an insecure
		// entry name) is the archive's own, one the Bridge's zip.OpenReader
		// would refuse too.
		return false, nil
	}
	for _, e := range zr.File {
		if e.Name == "META-INF/container.xml" {
			return true, nil
		}
	}
	return false, nil
}

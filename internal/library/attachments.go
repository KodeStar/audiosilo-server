package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// A book's .audiosilo/ attachments (metadata.AttachmentsDir; catalog/attachments.go)
// are found the way an Audiobookshelf metadata.json is: the discovery walk lists
// each .audiosilo folder it meets once (never deeper) and records the known
// files' sizes and mtimes; a book's signature is the part of that listing that is
// its own. A book whose audio is unchanged but whose signature differs has its
// attachments read again, with no re-index (the scan's attachments backfill).

// attachmentFiles is one .audiosilo folder's known files, by file name.
type attachmentFiles map[string]fs.FileInfo

// attachmentsPlace is where a book's attachments live: the library-relative
// folder holding its .audiosilo folder, and the prefix of each name in it. A
// folder book's (a joined disc set's included) are in its own folder with no
// prefix; a single-file book's are beside it, named after its stem
// ("Book.m4b" -> "Book.characters.json").
func attachmentsPlace(b *catalog.Book) (dir, prefix string) {
	if b.IsFolder {
		if b.RelPath == "." { // a `book` override on the library root
			return "", ""
		}
		return b.RelPath, ""
	}
	name := path.Base(b.RelPath)
	return dirOf(b.RelPath), strings.TrimSuffix(name, path.Ext(name)) + "."
}

// knownAttachment reports whether a file name in a .audiosilo folder is one the
// scan reads for some book: a known name, alone or after a stem.
func knownAttachment(name string) bool {
	for _, known := range metadata.AttachmentNames {
		if name == known || strings.HasSuffix(name, "."+known) {
			return true
		}
	}
	return false
}

// listAttachments lists the .audiosilo folder at absDir once: its known regular
// files (a link or anything else is never read). Nil when it can't be listed.
func listAttachments(absDir string) attachmentFiles {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil
	}
	var out attachmentFiles
	for _, e := range entries {
		if !e.Type().IsRegular() || !knownAttachment(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if out == nil {
			out = attachmentFiles{}
		}
		out[e.Name()] = info
	}
	return out
}

// attachmentsSig is the signature of the attachments in files named with prefix
// (metadata.AttachmentSignature of each, in metadata.AttachmentNames order,
// joined), "" for none.
func attachmentsSig(files attachmentFiles, prefix string) string {
	var parts []string
	for _, known := range metadata.AttachmentNames {
		if info, ok := files[prefix+known]; ok {
			parts = append(parts, metadata.AttachmentSignature(known, info))
		}
	}
	return strings.Join(parts, ",")
}

// sigNames are the known names a signature (attachmentsSig) lists.
func sigNames(sig string) []string {
	var out []string
	for part := range strings.SplitSeq(sig, ",") {
		if name, _, ok := strings.Cut(part, ":"); ok {
			out = append(out, name)
		}
	}
	return out
}

// isAttachmentsDir reports whether the folder entry at library-relative rel is a
// .audiosilo folder the scan lists: a real folder (the walk never follows a link)
// of exactly that name, not ignored.
func isAttachmentsDir(de fs.DirEntry, rel string, ignore *Ignore) bool {
	return de.IsDir() && de.Name() == metadata.AttachmentsDir && (ignore.Empty() || !ignore.Match(rel, true))
}

// attachmentsSigAt is the signature of book b's attachments, read from disk: for
// a book indexed with no walk (IndexPath) or refreshed alone (RefreshAttachments).
func attachmentsSigAt(lib catalog.Library, b *catalog.Book, ignore *Ignore) string {
	dir, prefix := attachmentsPlace(b)
	rel := path.Join(dir, metadata.AttachmentsDir)
	abs := absOf(lib, rel)
	info, err := os.Lstat(abs)
	if err != nil || !isAttachmentsDir(fs.FileInfoToDirEntry(info), rel, ignore) {
		return ""
	}
	return attachmentsSig(listAttachments(abs), prefix)
}

// readAttachments reads book b's attachments whose signature is sig ("" for
// none: nothing to read). A file that can't be used (metadata.ErrAttachmentInvalid:
// not the format, too large, a link; logged) is left out, and that reading is
// recorded with its signature, so it isn't read again until it changes. read is
// false when a file couldn't be read right now (gone since the walk, a flaky
// mount; logged): the caller records nothing for it, or records no signature, so
// the next scan reads them again.
func (s *Scanner) readAttachments(lib catalog.Library, b *catalog.Book, sig string) (files metadata.AttachmentFiles, read bool) {
	if sig == "" {
		return files, true
	}
	dir, prefix := attachmentsPlace(b)
	read = true
	for _, name := range sigNames(sig) {
		rel := path.Join(dir, metadata.AttachmentsDir, prefix+name)
		p := absOf(lib, rel)
		var err error
		switch name {
		case metadata.AttachmentCharacters:
			files.Characters, err = metadata.ReadCharacters(p)
		case metadata.AttachmentRecaps:
			files.Recaps, err = metadata.ReadRecaps(p)
		case metadata.AttachmentBridge:
			files.Provenance, err = metadata.ReadProvenance(p)
		}
		if err != nil {
			s.log.Warn("unusable .audiosilo attachment", "library", lib.Name, "path", rel, "err", err)
			read = read && errors.Is(err, metadata.ErrAttachmentInvalid)
		}
	}
	return files, read
}

// AttachmentsRefresh is what RefreshAttachments indexed for a book: its path and
// which attachments it now has.
type AttachmentsRefresh struct {
	Path       string
	Characters bool
	Recaps     bool
	Bridge     bool
}

// RefreshAttachments reads the .audiosilo/ attachments of the indexed book at
// relPath again and records them, never reading its audio: the Bridge's call
// once it has written them. relPath is the book's path, or a path inside it (a
// part, a disc folder). ErrNotIndexable when no indexed book is there; an error
// (nothing recorded) when a file couldn't be read right now.
func (s *Scanner) RefreshAttachments(ctx context.Context, lib catalog.Library, relPath string) (*AttachmentsRefresh, error) {
	if _, err := SafeJoin(lib.Root, relPath); err != nil {
		return nil, err
	}
	if HasHiddenPart(relPath) {
		return nil, fmt.Errorf("%w: %q is in a hidden folder", ErrNotIndexable, relPath)
	}
	book, err := s.cat.GetBookByPath(ctx, lib.ID, relPath)
	if errors.Is(err, catalog.ErrNotFound) {
		book, err = s.cat.GetBookHolding(ctx, lib.ID, relPath)
	}
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, fmt.Errorf("%w: no indexed book at %q", ErrNotIndexable, relPath)
	}
	if err != nil {
		return nil, err
	}
	sig := attachmentsSigAt(lib, book, ParseIgnore(lib.IgnorePatterns))
	files, read := s.readAttachments(lib, book, sig)
	if !read {
		return nil, fmt.Errorf("the attachments of %q couldn't be read", book.RelPath)
	}
	if err := s.cat.SetAttachments(ctx, lib.ID, map[string]catalog.AttachmentsReading{
		book.RelPath: {Sig: sig, Files: files},
	}); err != nil {
		return nil, err
	}
	return &AttachmentsRefresh{Path: book.RelPath, Characters: files.Characters != nil,
		Recaps: files.Recaps != nil, Bridge: files.Provenance != nil}, nil
}

// attachmentsOfWalk sets each book's attachments signature from the .audiosilo
// folders the discovery walk listed (audioDirs), by the folder holding them.
func attachmentsOfWalk(books []*catalog.Book, found map[string]attachmentFiles) {
	if len(found) == 0 {
		return
	}
	for _, b := range books {
		dir, prefix := attachmentsPlace(b)
		b.Attachments.Sig = attachmentsSig(found[dir], prefix)
	}
}

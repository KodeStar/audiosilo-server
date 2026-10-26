package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

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
// folder holding its .audiosilo folder (sidecarDir), and the prefix of each name
// in it. A folder book's (a joined disc set's included) are in its own folder
// with no prefix; a single-file book's are beside it, named after its stem
// ("Book.m4b" -> "Book.characters.json").
func attachmentsPlace(b *catalog.Book) (dir, prefix string) {
	if b.IsFolder {
		return sidecarDir(b.RelPath, true), ""
	}
	return sidecarDir(b.RelPath, false), stemOf(b.RelPath) + "."
}

// sidecarDir is the library-relative folder holding the files that sit beside a
// book (its .audiosilo folder, its ebook): a folder book's own ("" for a `book`
// override on the library root), a single-file book's.
func sidecarDir(relPath string, folder bool) string {
	switch {
	case !folder:
		return dirOf(relPath)
	case relPath == ".": // a `book` override on the library root
		return ""
	default:
		return relPath
	}
}

// stemOf is the file name at rel without its last extension ("A/Book.m4b" ->
// "Book"): what a single-file book's files beside it are named after.
func stemOf(rel string) string {
	name := path.Base(rel)
	return strings.TrimSuffix(name, path.Ext(name))
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
// files (a link or anything else is never read). A folder that is gone lists
// nothing; one that can't be listed right now (permissions, a flaky mount) is an
// error, never "no attachments", so what is stored for it stays.
func listAttachments(absDir string) (attachmentFiles, error) {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, absentOK(err)
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
	return out, nil
}

// absentOK is nil for an error that means the file isn't there (or can't be:
// a parent that isn't a folder, a name too long for the disk), else err.
func absentOK(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ENAMETOOLONG) {
		return nil
	}
	return err
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

// isAttachmentsDir reports whether the folder entry at library-relative rel is a
// .audiosilo folder the scan lists: a real folder (the walk never follows a link)
// of exactly that name, not ignored.
func isAttachmentsDir(de fs.DirEntry, rel string, ignore *Ignore) bool {
	return de.IsDir() && de.Name() == metadata.AttachmentsDir && (ignore.Empty() || !ignore.Match(rel, true))
}

// attachmentsSigAt is the signature of book b's attachments, read from disk: for
// a book indexed with no walk (IndexPath) or refreshed alone (RefreshAttachments).
// It looks up only b's own names (at most one per known name), not the whole
// folder, which single-file books share. An error is a folder or file that
// couldn't be looked at right now (never one that isn't there): the caller keeps
// what is stored rather than reading it as none.
func attachmentsSigAt(lib catalog.Library, b *catalog.Book, ignore *Ignore) (string, error) {
	dir, prefix := attachmentsPlace(b)
	rel := path.Join(dir, metadata.AttachmentsDir)
	abs := absOf(lib, rel)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", absentOK(err)
	}
	if !isAttachmentsDir(fs.FileInfoToDirEntry(info), rel, ignore) {
		return "", nil
	}
	files := attachmentFiles{}
	for _, known := range metadata.AttachmentNames {
		fi, err := os.Lstat(filepath.Join(abs, prefix+known))
		if err != nil {
			if err := absentOK(err); err != nil {
				return "", err
			}
			continue
		}
		if fi.Mode().IsRegular() { // as listAttachments: a link or anything else is never read
			files[prefix+known] = fi
		}
	}
	return attachmentsSig(files, prefix), nil
}

// readAttachments reads book b's attachments whose signature is sig ("" for
// none: nothing to read). A file that can't be used (metadata.ErrAttachmentInvalid:
// not the format, too large, a link; logged) is left out, and that reading is
// recorded with its signature, so it isn't read again until it changes. read is
// false, with no files, when a file couldn't be read right now (gone since the
// walk, a flaky mount; logged): the caller records nothing, keeping what is
// stored, so the next scan reads them again. Never a part of the set: a reading
// recorded without one of its files would read as complete.
func (s *Scanner) readAttachments(lib catalog.Library, b *catalog.Book, sig string) (files metadata.AttachmentFiles, read bool) {
	if sig == "" {
		return files, true
	}
	dir, prefix := attachmentsPlace(b)
	read = true
	for _, name := range metadata.AttachmentSigNames(sig) {
		rel := path.Join(dir, metadata.AttachmentsDir, prefix+name)
		p := absOf(lib, rel)
		var err error
		switch name {
		case metadata.AttachmentCharacters:
			files.Characters, err = metadata.ReadCharacters(p)
		case metadata.AttachmentRecaps:
			files.Recaps, err = metadata.ReadRecaps(p)
		case metadata.AttachmentDescription:
			files.Description, err = metadata.ReadDescription(p)
		case metadata.AttachmentChapters:
			files.Chapters, err = metadata.ReadChapterList(p)
		case metadata.AttachmentBridge:
			files.Provenance, err = metadata.ReadProvenance(p)
		}
		if err != nil {
			s.log.Warn("unusable .audiosilo attachment", "library", lib.Name, "path", rel, "err", err)
			read = read && errors.Is(err, metadata.ErrAttachmentInvalid)
		}
	}
	if !read {
		return metadata.AttachmentFiles{}, false
	}
	return files, true
}

// AttachmentsRefresh is what RefreshAttachments indexed for a book: its path and
// which attachments it now has (POST /bridge/rescan's answer in attachments
// mode, as is).
type AttachmentsRefresh struct {
	Path        string          `json:"path"`
	Attachments AttachmentsHeld `json:"attachments"`
}

// AttachmentsHeld says which of a book's attachments are indexed.
type AttachmentsHeld struct {
	Characters  bool `json:"characters"`
	Recaps      bool `json:"recaps"`
	Description bool `json:"description"`
	Chapters    bool `json:"chapters"`
	Bridge      bool `json:"bridge"`
}

// RefreshAttachments reads the .audiosilo/ attachments of the indexed book at
// relPath again and records them, never reading its audio: the Bridge's call
// once it has written them. relPath is the book's path, or a path inside it (a
// part, a disc folder). ErrNotIndexable when no indexed book is there; an error
// (nothing recorded) when the folder couldn't be listed or a file couldn't be
// read right now.
func (s *Scanner) RefreshAttachments(ctx context.Context, lib catalog.Library, relPath string) (*AttachmentsRefresh, error) {
	if _, err := ContentJoin(lib.Root, relPath); err != nil {
		return nil, err
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
	sig, err := attachmentsSigAt(lib, book, ParseIgnore(lib.IgnorePatterns))
	if err != nil {
		return nil, fmt.Errorf("the attachments of %q couldn't be listed: %w", book.RelPath, err)
	}
	files, read := s.readAttachments(lib, book, sig)
	if !read {
		return nil, fmt.Errorf("the attachments of %q couldn't be read", book.RelPath)
	}
	if err := s.cat.SetAttachments(ctx, lib.ID, map[string]catalog.AttachmentsReading{
		book.RelPath: {Sig: sig, Files: files},
	}); err != nil {
		return nil, err
	}
	return &AttachmentsRefresh{Path: book.RelPath, Attachments: AttachmentsHeld{Characters: files.Characters != nil,
		Recaps: files.Recaps != nil, Description: files.Description != nil, Chapters: files.Chapters != nil,
		Bridge: files.Provenance != nil}}, nil
}

// attachmentsOfWalk sets each book's attachments signature from the .audiosilo
// folders the discovery walk listed (audioDirs), by the folder holding them. A
// book whose folder couldn't be listed is Unread: what is stored for it stays.
func attachmentsOfWalk(books []*catalog.Book, side sidecarFiles) {
	if len(side.attachments) == 0 && len(side.unlisted) == 0 {
		return
	}
	for _, b := range books {
		dir, prefix := attachmentsPlace(b)
		if side.unlisted[dir] {
			b.Attachments.Unread = true
			continue
		}
		b.Attachments.Sig = attachmentsSig(side.attachments[dir], prefix)
	}
}

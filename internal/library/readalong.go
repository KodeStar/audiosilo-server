package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// A book's read-along map (Bridge Phase 3c, metadata.ReadalongHead): the scan
// reads its head with the other attachments, and hashes the ebook of a book
// that has one (catalog.ReadalongEbooksDue) so a stale map can be told from a
// fresh one; GET /libraries/{id}/readalong reads the file itself
// (ReadReadalongMap).

// ErrNoReadalong is ReadReadalongMap's answer for a book with no map file to
// serve: none there, its .audiosilo folder not a real folder, or a file that
// isn't a regular one or is too large.
var ErrNoReadalong = errors.New("library: no read-along map")

// EbookDigest is the sha256 (lowercase hex) of the ebook at library-relative
// rel, opened by OpenEbook's rules (noLink as there: a folder book's is never a
// link), and the signature (catalog.EbookSig) of the very file it hashed, so the
// digest is recorded only for the file the index holds. It stops with ctx (a
// request's deadline, a cancelled scan), even part way through a slow share.
func EbookDigest(ctx context.Context, root, rel string, noLink bool) (sha, sig string, err error) {
	f, info, err := OpenEbook(root, rel, noLink)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, f}); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(h.Sum(nil)), catalog.EbookSig(path.Base(rel), info), nil
}

// EbookDigestNow is the sha256 of the ebook (ebookPath) of the book at
// bookPath, idx its index row (catalog.BookReadalongAt): the stored digest
// while the file on disk still has the signature it was computed for (one
// stat; the index's ebook_sig is only as new as the last scan, so an EPUB
// replaced since is never answered with the old file's digest), else hashed
// now and recorded with the new file's signature (HashEbook). ErrNoEbook (or
// fs.ErrNotExist) when there is no such file now.
func (s *Scanner) EbookDigestNow(ctx context.Context, lib catalog.Library, idx catalog.ReadalongIndexed, bookPath, ebookPath string, folder bool) (string, error) {
	_, info, err := lookEbook(lib.Root, ebookPath, folder)
	if err != nil {
		return "", err
	}
	if idx.HashedSig == catalog.EbookSig(path.Base(ebookPath), info) {
		return idx.EbookSHA256, nil
	}
	return s.HashEbook(ctx, lib, bookPath, ebookPath, folder)
}

// ctxReader is r that fails with ctx's error once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// hashReadalongEbooks hashes the ebooks of the library's books that have a
// read-along map and no current digest (catalog.ReadalongEbooksDue; only the
// book at bookPath when it isn't ""), recording each (HashEbook). One that
// can't be read now is logged and left for the next look (the scan, the
// Bridge's rescan, or GET /readalong on demand). It stops with ctx.
func (s *Scanner) hashReadalongEbooks(ctx context.Context, lib catalog.Library, bookPath string) {
	due, err := s.cat.ReadalongEbooksDue(ctx, lib.ID, bookPath)
	if err != nil {
		s.log.Warn("list ebooks to hash failed", "library", lib.Name, "err", err)
		return
	}
	for _, d := range due {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.HashEbook(ctx, lib, d.BookPath, d.EbookPath, d.IsFolder); err != nil {
			s.log.Warn("hash ebook failed", "library", lib.Name, "path", d.EbookPath, "err", err)
		}
	}
}

// HashEbook hashes the ebook (ebookPath) of the book at bookPath now
// (EbookDigest) and records the digest with the signature of the file it hashed
// (only while the index still names that ebook: catalog.SetEbookDigest). A
// failure to record it is logged, never the answer's.
func (s *Scanner) HashEbook(ctx context.Context, lib catalog.Library, bookPath, ebookPath string, folder bool) (string, error) {
	sha, sig, err := EbookDigest(ctx, lib.Root, ebookPath, folder)
	if err != nil {
		return "", err
	}
	if err := s.cat.SetEbookDigest(ctx, lib.ID, bookPath, ebookPath, sig, sha); err != nil {
		s.log.Warn("record ebook digest failed", "library", lib.Name, "path", bookPath, "err", err)
	}
	return sha, nil
}

// ReadReadalongMap reads the read-along map of the book at bookPath (folder
// says whether it is a folder book) from disk now: its bytes and the opened
// file's info. The map is where the Bridge writes it (attachmentsPlace: the
// book's own .audiosilo folder, or beside a single-file book named after its
// stem), that folder a real one (never a link) the library's ignore rules leave
// alone (as the scan lists it: isAttachmentsDir) and the file a regular one
// (metadata.ReadReadalongFile: never through a link, at most
// metadata.MaxReadalongFile bytes). ErrNoReadalong when there is none to serve;
// any other error is one reading it right now.
func ReadReadalongMap(lib catalog.Library, bookPath string, folder bool) ([]byte, fs.FileInfo, error) {
	b := &catalog.Book{RelPath: bookPath, IsFolder: folder}
	dir, prefix := attachmentsPlace(b)
	full, err := ContentJoin(lib.Root, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrNoReadalong, err)
	}
	attDir := filepath.Join(full, metadata.AttachmentsDir)
	info, err := os.Lstat(attDir)
	switch {
	case err == nil && !isAttachmentsDir(fs.FileInfoToDirEntry(info), path.Join(dir, metadata.AttachmentsDir), ParseIgnore(lib.IgnorePatterns)):
		return nil, nil, ErrNoReadalong
	case err != nil && absentOK(err) == nil:
		return nil, nil, ErrNoReadalong
	case err != nil:
		return nil, nil, err
	}
	data, fi, err := readRetryingSwap(metadata.ReadReadalongFile, filepath.Join(attDir, prefix+metadata.AttachmentReadalong))
	switch {
	case err == nil:
		return data, fi, nil
	case errors.Is(err, metadata.ErrAttachmentInvalid), absentOK(err) == nil:
		return nil, nil, fmt.Errorf("%w: %w", ErrNoReadalong, err)
	}
	return nil, nil, err
}

// readRetryingSwap reads the file at p with read, once more when it was
// replaced while being opened (metadata.ErrReplacedWhileOpening): the Bridge
// renames a new map into place ("Make again"), and a request that met the
// rename reads the new file rather than failing. A second replacement in a row
// is the error.
func readRetryingSwap(read func(string) ([]byte, fs.FileInfo, error), p string) ([]byte, fs.FileInfo, error) {
	data, info, err := read(p)
	if errors.Is(err, metadata.ErrReplacedWhileOpening) {
		data, info, err = read(p)
	}
	return data, info, err
}

package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"time"

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
// digest is recorded with the file it is current for. It stops with ctx (a
// request's deadline, a cancelled scan), even part way through a slow share.
// Its open and each of its reads is a metadata.Bounded call (ctx, at most wait
// each; none when 0): on a share that stopped answering one is given up on
// (metadata.GaveUp), while a whole large ebook on a slow share that still
// answers is hashed for as long as ctx lives (a scan's: to the end).
func EbookDigest(ctx context.Context, root, rel string, noLink bool, wait time.Duration) (sha, sig string, err error) {
	at := filepath.Join(root, filepath.FromSlash(rel))
	f, info, err := OpenEbookBounded(ctx, root, rel, noLink, wait)
	if err != nil {
		return "", "", err
	}
	// A read given up on may still return later, into its buffer: the hash
	// stops there, and the file is closed once that read has returned.
	defer func() { _ = f.Close() }()
	h := sha256.New()
	buf := make([]byte, digestReadSize)
	for {
		n, err := metadata.Bounded(ctx, at, wait, func() (int, error) { return f.Read(buf) }, nil)
		h.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), catalog.EbookSig(path.Base(rel), info), nil
}

// digestReadSize is how much of an ebook EbookDigest reads at a time (each
// read a metadata.Bounded call of its own).
const digestReadSize = 1 << 20

// EbookDigestNow is the sha256 of the ebook (ebookPath) of the book at
// bookPath, idx its index row (catalog.BookReadalongAt): the stored digest
// while the file on disk still has the signature it was computed for (one
// stat; the index's ebook_sig is only as new as the last scan, so an EPUB
// replaced since is never answered with the old file's digest), else hashed
// now (EbookDigest) and recorded with the new file's signature, only while the
// index still names that ebook. A competing write prompts a fresh check of
// the signature on disk before retrying catalog.SetEbookDigest. A failure to record it is logged, never the answer's. ErrNoEbook
// (or fs.ErrNotExist) when there is no such file now. The look is a
// metadata.Bounded call (ctx, at most wait), and so are the hash's open and
// each of its reads (EbookDigest): on a share that stopped answering one is
// given up on (metadata.GaveUp).
func (s *Scanner) EbookDigestNow(ctx context.Context, lib catalog.Library, idx catalog.ReadalongIndexed, bookPath, ebookPath string, folder bool, wait time.Duration) (string, error) {
	liveSig := func() (string, error) {
		return metadata.Bounded(ctx, absOf(lib, ebookPath), wait, func() (string, error) {
			_, info, err := lookEbook(lib.Root, ebookPath, folder)
			if err != nil {
				return "", err
			}
			return catalog.EbookSig(path.Base(ebookPath), info), nil
		}, nil)
	}
	current, err := liveSig()
	if err != nil {
		return "", err
	}
	if idx.HashedSig == current {
		return idx.EbookSHA256, nil
	}
	sha, sig, err := EbookDigest(ctx, lib.Root, ebookPath, folder, wait)
	if err != nil {
		return "", err
	}
	// Both the scan and a request can start from the same stale digest. If the
	// first writer hashed a different file, retry only while ours is on disk.
	for was := idx.HashedSig; ; {
		var saved bool
		saved, err = s.cat.SetEbookDigest(ctx, lib.ID, bookPath, ebookPath, was, sig, sha)
		if err != nil || saved {
			break
		}
		idx, err = s.cat.BookReadalongAt(ctx, lib.ID, bookPath)
		if err != nil || idx.HashedSig == was || idx.HashedSig == sig {
			break
		}
		current, err = liveSig()
		if err != nil || current != sig {
			break
		}
		was = idx.HashedSig
	}
	if err != nil && !errors.Is(err, catalog.ErrNotFound) {
		s.log.Warn("record ebook digest failed", "library", lib.Name, "path", bookPath, "err", err)
	}
	return sha, nil
}

// hashReadalongEbooks hashes the ebooks of the library's books that have a
// read-along map and no current digest (catalog.ReadalongEbooksDue; only the
// book at bookPath when it isn't ""), recording each (EbookDigestNow: hashed
// only when the file on disk isn't the one its stored digest is of). One that
// can't be read now is logged and left for the next look (the scan, the
// Bridge's rescan, or GET /readalong on demand). It stops with ctx, and once
// one is given up on (lookTimeout each: a share that stopped answering, where
// the rest would each wait out their time too).
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
		// EbookDigestNow hashes only when the stored digest isn't the file's on
		// disk: a request may have recorded one for a file newer than the index
		// (an attachments rescan before the next scan records its ebook_sig).
		// A competing digest is replaced only after checking the live file again.
		idx := catalog.ReadalongIndexed{HashedSig: d.HashedSig}
		if _, err := s.EbookDigestNow(ctx, lib, idx, d.BookPath, d.EbookPath, d.IsFolder, s.lookTimeout(ctx)); err != nil {
			s.log.Warn("hash ebook failed", "library", lib.Name, "path", d.EbookPath, "err", err)
			if metadata.GaveUp(err) {
				return
			}
		}
	}
}

// readalongLstat is ReadReadalongMap's look at the book's .audiosilo folder:
// metadata.LstatBelow (a test's swaps the folder once it has looked).
var readalongLstat = metadata.LstatBelow

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
	file, err := readalongMapFile(lib, bookPath, folder)
	if err != nil {
		return nil, nil, err
	}
	// metadata reads a map the Bridge renamed in mid-read once more.
	data, fi, err := metadata.ReadReadalongFile(lib.Root, file)
	switch {
	case err == nil:
		return data, fi, nil
	case errors.Is(err, metadata.ErrAttachmentInvalid), absentOK(err) == nil:
		return nil, nil, fmt.Errorf("%w: %w", ErrNoReadalong, err)
	}
	return nil, nil, err
}

// StatReadalongMap is ReadReadalongMap's file looked at, not read: its info
// (never through a link), for a revalidation the index can answer
// (metadata.AttachmentSignature of it in the book's attachments signature).
// ErrNoReadalong when there is none, or it isn't a regular file.
func StatReadalongMap(lib catalog.Library, bookPath string, folder bool) (fs.FileInfo, error) {
	file, err := readalongMapFile(lib, bookPath, folder)
	if err != nil {
		return nil, err
	}
	info, err := metadata.LstatBelow(lib.Root, file)
	switch {
	case err == nil && info.Mode().IsRegular():
		return info, nil
	case err == nil, absentOK(err) == nil:
		return nil, ErrNoReadalong
	}
	return nil, err
}

// readalongMapFile is where the read-along map of the book at bookPath is
// (ReadReadalongMap's placement rules), its .audiosilo folder checked.
func readalongMapFile(lib catalog.Library, bookPath string, folder bool) (string, error) {
	b := &catalog.Book{RelPath: bookPath, IsFolder: folder}
	dir, prefix := attachmentsPlace(b)
	full, err := ContentJoin(lib.Root, dir)
	if metadata.GaveUp(err) {
		return "", err // the join's look a share failed: not known, never no map
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoReadalong, err)
	}
	attDir := filepath.Join(full, metadata.AttachmentsDir)
	info, err := readalongLstat(lib.Root, attDir)
	switch {
	case err == nil && !isAttachmentsDir(fs.FileInfoToDirEntry(info), path.Join(dir, metadata.AttachmentsDir), ParseIgnore(lib.IgnorePatterns)):
		return "", ErrNoReadalong
	case err != nil && absentOK(err) == nil:
		return "", ErrNoReadalong
	case err != nil:
		return "", err
	}
	return filepath.Join(attDir, prefix+metadata.AttachmentReadalong), nil
}

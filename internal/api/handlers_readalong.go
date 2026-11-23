package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/web/spa"
)

// Read-along (Bridge Phase 3c, capability "readalong"): the map the Bridge's
// readalong.align job wrote beside a book (.audiosilo/readalong.json), served
// to the player that follows the narration in the book's ebook, and its state
// on the book (Book.readalong). Transport only: the head's checks are
// metadata.ParseReadalongHead's, the state catalog.ReadalongState's.

// Read-along error codes.
const (
	codeNoReadalong    = "no_readalong"
	codeReadalongStale = "readalong_stale"
	msgNoReadalong     = "this book has no read-along"
	msgReadalongStale  = "this book's read-along was made for another edition of its ebook or audio; make it again"
)

// handleReadalong serves the read-along map of the book at ?path= (exactly the
// book's own path): the file's bytes as JSON, gzipped for a client that accepts
// it (gzipAccepted: never for a Range request), with an ETag of its size and
// mtime, its own for the gzip answer (304 on a match; for a map the index has
// read and still vouches for, without reading the file: readalongUnchanged).
// It is header-auth only (requireAuth: a session or an API key, never
// ?token=), scoped like /ebook. Before sending it re-reads the file and checks it against the index
// now (never trusting the indexed head for the bytes it sends): a map whose
// head fails its checks, a book with no ebook or no map, a part's path or a
// podcast library is 404 no_readalong; a map made from another ebook or other
// audio is 409 readalong_stale; one outside scope 403. The map's read or the
// ebook's look or hash given up on (fileWait: a share that stopped answering),
// or failing as a share does that a retry may get past (metadata.TransientFault),
// is 503 share_not_answering; one refused for good (metadata.LastingFault: a
// permission, the device gone) 404 no_readalong, as for a file gone; any other
// (a limit of the server's own) 500, logged.
func (a *API) handleReadalong(w http.ResponseWriter, r *http.Request) {
	lib, rel, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if lib.Kind == catalog.KindPodcasts {
		writeErrorCode(w, http.StatusNotFound, codeNoReadalong, msgNoReadalong)
		return
	}
	book, ok := a.bookAt(w, r, lib, scope, rel, "no book at this path", "could not load the read-along")
	if !ok {
		return
	}
	if book.RelPath != rel || book.Ebook == nil {
		// The map is addressed by its book alone, and needs the book's ebook.
		writeErrorCode(w, http.StatusNotFound, codeNoReadalong, msgNoReadalong)
		return
	}
	wantGzip := gzipAccepted(r)
	if a.readalongUnchanged(w, r, lib, book, wantGzip) {
		return
	}
	type readalongMap struct {
		data []byte
		info fs.FileInfo
	}
	m, err := metadata.Bounded(r.Context(), filepath.Join(lib.Root, filepath.FromSlash(book.RelPath)), a.fileWait, func() (readalongMap, error) {
		data, info, err := a.readalongRead(*lib, book.RelPath, book.IsFolder)
		return readalongMap{data, info}, err
	}, nil)
	data, info := m.data, m.info
	switch {
	case metadata.GaveUp(err) || metadata.TransientFault(err):
		writeShareNotAnswering(w)
		return
	case errors.Is(err, library.ErrNoReadalong), metadata.LastingFault(err):
		writeErrorCode(w, http.StatusNotFound, codeNoReadalong, msgNoReadalong)
		return
	case err != nil:
		a.log.Warn("read read-along failed", "library", lib.ID, "path", book.RelPath, "err", err)
		writeError(w, http.StatusInternalServerError, "could not load the read-along")
		return
	}
	head, err := metadata.ParseReadalongHead(data)
	if err != nil {
		a.log.Warn("unusable read-along map", "library", lib.ID, "path", book.RelPath, "err", err)
		writeErrorCode(w, http.StatusNotFound, codeNoReadalong, msgNoReadalong)
		return
	}
	ctx := r.Context()
	idx, err := a.cat.BookReadalongAt(ctx, lib.ID, book.RelPath)
	var digest string
	if err == nil {
		digest, err = a.scanner.EbookDigestNow(ctx, *lib, idx, book.RelPath, book.Ebook.Path, book.IsFolder, a.fileWait)
	}
	switch {
	case metadata.TransientFault(err):
		writeShareNotAnswering(w)
		return
	case errors.Is(err, library.ErrNoEbook), metadata.NotExist(err), metadata.LastingFault(err):
		writeErrorCode(w, http.StatusNotFound, codeNoReadalong, msgNoReadalong)
		return
	case err != nil:
		a.writeCatalogError(w, err, "hash ebook failed", "could not load the read-along", "library", lib.ID, "path", book.RelPath)
		return
	}
	files := catalog.BookAudioFiles(book.RelPath, book.IsFolder, book.Duration, book.Files)
	if st := catalog.ReadalongState(head, digest, files); st.State != catalog.ReadalongReady {
		writeJSON(w, http.StatusConflict, map[string]string{"error": msgReadalongStale, "code": codeReadalongStale,
			"reason": st.Reason})
		return
	}
	w.Header().Add("Vary", "Accept-Encoding")
	c := conditional{etag: readalongETag(info, wantGzip), cacheControl: readalongCache}
	if c.notModified(w, r) {
		return
	}
	c.setHeaders(w)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	body := data
	if wantGzip {
		if gz := spa.Gzip(data, gzipLevel); gz != nil {
			h.Set("Content-Encoding", "gzip")
			body = gz
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// readalongUnchanged answers a revalidation of an unchanged, still current map
// with a 304 without reading it (server#265), and reports whether it did: when
// r's If-None-Match names the map's ETag as the file is now, the index's head
// was read from that very file (its signature in the book's attachments
// signature) and that head still holds against the ebook (its digest, read
// from the index while the file is unchanged) and the audio. Anything else
// (no If-None-Match, a changed map, one the index hasn't read, a look that
// failed) is left to the full answer, which reads and checks the file itself;
// but a look at the map or the ebook given up on (fileWait: a share that
// stopped answering), or failing as a share does that a retry may get past, is
// answered 503 share_not_answering here, as the full answer's read of the same
// files would only wait it out a second time.
func (a *API) readalongUnchanged(w http.ResponseWriter, r *http.Request, lib *catalog.Library, book *catalog.Book, gzip bool) bool {
	inm := r.Header.Get("If-None-Match")
	if inm == "" {
		return false
	}
	// The index first: a book it holds no head for touches no share.
	ctx := r.Context()
	idx, err := a.cat.BookReadalongAt(ctx, lib.ID, book.RelPath)
	if err != nil || idx.Head == nil {
		return false
	}
	info, err := metadata.LookBounded(ctx, filepath.Join(lib.Root, filepath.FromSlash(book.RelPath)), a.fileWait, func() (fs.FileInfo, error) {
		return library.StatReadalongMap(*lib, book.RelPath, book.IsFolder)
	})
	if metadata.GaveUp(err) || metadata.TransientFault(err) {
		writeShareNotAnswering(w)
		return true
	}
	if err != nil {
		return false
	}
	c := conditional{etag: readalongETag(info, gzip), cacheControl: readalongCache}
	if !spa.ETagListMatches(inm, c.etag) ||
		!slices.Contains(strings.Split(idx.AttachmentsSig, ","), metadata.AttachmentSignature(metadata.AttachmentReadalong, info)) {
		return false
	}
	digest, err := a.scanner.EbookDigestNow(ctx, *lib, idx, book.RelPath, book.Ebook.Path, book.IsFolder, a.fileWait)
	switch {
	case metadata.GaveUp(err) || metadata.TransientFault(err):
		// The full answer would wait on the same ebook again for its 503.
		writeShareNotAnswering(w)
		return true
	case err != nil:
		return false
	}
	files := catalog.BookAudioFiles(book.RelPath, book.IsFolder, book.Duration, book.Files)
	if catalog.ReadalongState(idx.Head, digest, files).State != catalog.ReadalongReady {
		return false
	}
	w.Header().Add("Vary", "Accept-Encoding")
	return c.notModified(w, r)
}

// readalongETag is the map's ETag as the file info has it. The gzip answer is
// another representation, so it has an ETag of its own ("-gz", as spa.Files'
// are; server#265): a cache never pairs one with the other's bytes. It is
// chosen by the request, before any gzipping, so a 304 costs none; a map gzip
// can't shrink goes out plain under it, which is still the one answer that
// ETag ever names.
func readalongETag(info fs.FileInfo, gzip bool) string {
	etag := fileETag(info)
	if gzip {
		etag = etag[:len(etag)-1] + `-gz"`
	}
	return etag
}

// readalongCache is a map's Cache-Control: the Bridge may rewrite it at any
// moment, so it is revalidated every time.
const readalongCache = "private, no-cache"

// fileETag is the ETag of a file served as it is on disk (/ebook, /readalong):
// its size and mtime.
func fileETag(info fs.FileInfo) string {
	return fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano())
}

// readalongOfBook is Book.readalong for book (readalongOf): nil when it has no
// ebook.
func (a *API) readalongOfBook(ctx context.Context, lib *catalog.Library, book *catalog.Book) *catalog.Readalong {
	if book.Ebook == nil {
		return nil
	}
	return a.readalongOf(ctx, lib, book.RelPath, book.IsFolder, book.Ebook.Path,
		catalog.BookAudioFiles(book.RelPath, book.IsFolder, book.Duration, book.Files))
}

// readalongOf is Book.readalong for a book with an ebook (its own path, whether
// it is a folder book, its ebook's path, and its audio files as a map names
// them: catalog.BookAudioFiles): nil when it has no valid map in the index, the
// ebook's digest library.Scanner.EbookDigestNow's. A failure is logged and
// reads as no block: the book's page must still answer.
func (a *API) readalongOf(ctx context.Context, lib *catalog.Library, bookPath string, folder bool, ebookPath string, files []metadata.ReadalongFile) *catalog.Readalong {
	idx, err := a.cat.BookReadalongAt(ctx, lib.ID, bookPath)
	if err != nil {
		a.log.Warn("read read-along state failed", "library", lib.ID, "path", bookPath, "err", err)
		return nil
	}
	if idx.Head == nil {
		return nil
	}
	sha, err := a.scanner.EbookDigestNow(ctx, *lib, idx, bookPath, ebookPath, folder, a.fileWait)
	if err != nil {
		// An ebook gone since the last scan is no read-along, not a failure.
		if !errors.Is(err, library.ErrNoEbook) && !errors.Is(err, fs.ErrNotExist) {
			a.log.Warn("hash ebook failed", "library", lib.ID, "path", ebookPath, "err", err)
		}
		return nil
	}
	return catalog.ReadalongState(idx.Head, sha, files)
}

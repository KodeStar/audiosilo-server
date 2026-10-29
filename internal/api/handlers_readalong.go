package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"

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
// it, with an ETag of its size and mtime (304 on a match). It is header-auth
// only (requireAuth: a session or an API key, never ?token=), scoped like
// /ebook. Before sending it re-reads the file and checks it against the index
// now (never trusting the indexed head for the bytes it sends): a map whose
// head fails its checks, a book with no ebook or no map, a part's path or a
// podcast library is 404 no_readalong; a map made from another ebook or other
// audio is 409 readalong_stale; one outside scope 403; a file that can't be
// read now 500, logged.
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
	data, info, err := library.ReadReadalongMap(*lib, book.RelPath, book.IsFolder)
	switch {
	case errors.Is(err, library.ErrNoReadalong):
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
		digest, err = a.digestOf(ctx, lib, idx, book.RelPath, book.Ebook.Path, book.IsFolder)
	}
	switch {
	case errors.Is(err, library.ErrNoEbook), errors.Is(err, fs.ErrNotExist):
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
	c := conditional{etag: fileETag(info), cacheControl: readalongCache}
	if c.notModified(w, r) {
		return
	}
	c.setHeaders(w)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	body := data
	if spa.AcceptsGzip(r.Header.Get("Accept-Encoding")) {
		if gz := spa.Gzipped(data); gz != nil {
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

// readalongCache is a map's Cache-Control: the Bridge may rewrite it at any
// moment, so it is revalidated every time.
const readalongCache = "private, no-cache"

// fileETag is the ETag of a file served as it is on disk (/ebook, /readalong):
// its size and mtime.
func fileETag(info fs.FileInfo) string {
	return fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano())
}

// digestOf is the sha256 of the ebook (ebookPath) of the book at bookPath, idx
// its index row (catalog.BookReadalongAt): the stored one while the file on
// disk still has the signature it was computed for (one stat:
// library.EbookSigNow; the index's ebook_sig is only as new as the last scan,
// so an EPUB replaced since would otherwise answer with the old file's
// digest), else hashed now and recorded with the new file's signature, so the
// next ask doesn't hash it again (library.Scanner.HashEbook).
func (a *API) digestOf(ctx context.Context, lib *catalog.Library, idx catalog.ReadalongIndexed, bookPath, ebookPath string, folder bool) (string, error) {
	sig, err := library.EbookSigNow(lib.Root, ebookPath, folder)
	if err != nil {
		return "", err
	}
	if idx.HashedSig != "" && idx.HashedSig == sig && idx.EbookSHA256 != "" {
		return idx.EbookSHA256, nil
	}
	return a.scanner.HashEbook(ctx, *lib, bookPath, ebookPath, folder)
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
// them: catalog.BookAudioFiles): nil when it has no valid map in the index. The
// ebook's digest is digestOf's (the stored one while the file on disk is the
// one it was computed for, else hashed now and recorded). A failure is logged
// and reads as no block: the book's page must still answer.
func (a *API) readalongOf(ctx context.Context, lib *catalog.Library, bookPath string, folder bool, ebookPath string, files []metadata.ReadalongFile) *catalog.Readalong {
	idx, err := a.cat.BookReadalongAt(ctx, lib.ID, bookPath)
	if err != nil {
		a.log.Warn("read read-along state failed", "library", lib.ID, "path", bookPath, "err", err)
		return nil
	}
	if idx.Head == nil {
		return nil
	}
	sha, err := a.digestOf(ctx, lib, idx, bookPath, ebookPath, folder)
	if err != nil {
		a.log.Warn("hash ebook failed", "library", lib.ID, "path", ebookPath, "err", err)
		return nil
	}
	return catalog.ReadalongState(idx.Head, sha, files)
}

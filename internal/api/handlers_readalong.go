package api

import (
	"bytes"
	"compress/gzip"
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
	sha, err := a.ebookDigest(r.Context(), lib, book.RelPath, book.Ebook.Path, book.IsFolder)
	switch {
	case errors.Is(err, library.ErrNoEbook), errors.Is(err, fs.ErrNotExist):
		writeErrorCode(w, http.StatusNotFound, codeNoReadalong, msgNoReadalong)
		return
	case err != nil:
		a.writeCatalogError(w, err, "hash ebook failed", "could not load the read-along", "library", lib.ID, "path", book.RelPath)
		return
	}
	files := catalog.BookAudioFiles(book.RelPath, book.IsFolder, book.Duration, book.Files)
	if st := catalog.ReadalongState(head, sha, files); st.State != catalog.ReadalongReady {
		writeJSON(w, http.StatusConflict, map[string]string{"error": msgReadalongStale, "code": codeReadalongStale,
			"reason": st.Reason})
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "private, no-cache")
	h.Add("Vary", "Accept-Encoding")
	etag := fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano())
	h.Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && spa.ETagListMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	body := data
	if spa.AcceptsGzip(r.Header.Get("Accept-Encoding")) {
		if gz := gzipBytes(data); gz != nil {
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

// gzipBytes is data gzipped, nil when that fails.
func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil || zw.Close() != nil {
		return nil
	}
	return buf.Bytes()
}

// ebookDigest is the sha256 of the ebook (ebookPath) of the book at bookPath:
// the index's when it is current (computed for the ebook the book has now),
// else hashed now (library.EbookDigest) and recorded for the next ask.
func (a *API) ebookDigest(ctx context.Context, lib *catalog.Library, bookPath, ebookPath string, folder bool) (string, error) {
	idx, err := a.cat.BookReadalongAt(ctx, lib.ID, bookPath)
	if err != nil {
		return "", err
	}
	if idx.DigestCurrent {
		return idx.EbookSHA256, nil
	}
	return a.hashEbook(ctx, lib, bookPath, ebookPath, folder)
}

// hashEbook hashes the book's ebook now and records the digest (only while the
// index still holds that file: catalog.SetEbookDigest); a failure to record it
// is logged, never the answer's.
func (a *API) hashEbook(ctx context.Context, lib *catalog.Library, bookPath, ebookPath string, folder bool) (string, error) {
	sha, sig, err := library.EbookDigest(lib.Root, ebookPath, folder)
	if err != nil {
		return "", err
	}
	if err := a.cat.SetEbookDigest(ctx, lib.ID, bookPath, sig, sha); err != nil {
		a.log.Warn("record ebook digest failed", "library", lib.ID, "path", bookPath, "err", err)
	}
	return sha, nil
}

// readalongOf is Book.readalong for a book (its own path, whether it is a
// folder book, its ebook's path, "" for none, and its audio files as a map
// names them: catalog.BookAudioFiles): nil when it has no ebook or no valid
// map in the index. The ebook's digest is the index's, hashed now when it isn't
// current (a map just found; once per ebook). A failure is logged and reads as
// no block: the book's page must still answer.
func (a *API) readalongOf(ctx context.Context, lib *catalog.Library, bookPath string, folder bool, ebookPath string, files []metadata.ReadalongFile) *catalog.Readalong {
	if ebookPath == "" {
		return nil
	}
	idx, err := a.cat.BookReadalongAt(ctx, lib.ID, bookPath)
	if err != nil {
		a.log.Warn("read read-along state failed", "library", lib.ID, "path", bookPath, "err", err)
		return nil
	}
	if idx.Head == nil {
		return nil
	}
	sha := idx.EbookSHA256
	if !idx.DigestCurrent {
		if sha, err = a.hashEbook(ctx, lib, bookPath, ebookPath, folder); err != nil {
			a.log.Warn("hash ebook failed", "library", lib.ID, "path", ebookPath, "err", err)
			return nil
		}
	}
	return catalog.ReadalongState(idx.Head, sha, files)
}

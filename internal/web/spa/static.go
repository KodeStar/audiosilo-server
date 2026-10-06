package spa

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Files serves static files with a strong ETag (a hash of the bytes) and, for the
// text types a client accepts gzip for, a gzip form. Safe for concurrent use.
//
// Both are worked out once per file and kept in memory, not per request. An
// embedded build (the console, an embedded player, the connect pages) never
// changes while the process runs, so each file is hashed and compressed once. A
// player served from web_dir on disk can change under the running server, so an
// entry is keyed on the file's path and checked against its size and modification
// time: a replaced file is worked out again on its next request. That costs one
// stat per request (which serving needed anyway) instead of compressing the
// bundle on every page load, and a file can only be served under a stale ETag if
// it changed without changing size or mtime, the same blind spot Last-Modified
// has.
type Files struct {
	mu   sync.Mutex
	m    map[string]*fileRep
	docs map[string]*docRep
}

// docRep is an HTML document's bytes and the CSP they make (the player's hashes
// its inline scripts), kept per file version like fileRep, so a deep link or a
// revalidation doesn't read and hash the document again.
type docRep struct {
	size int64
	mod  time.Time
	once sync.Once
	err  error
	data []byte
	csp  string
}

// fileRep is what Files keeps about one file.
type fileRep struct {
	size int64
	mod  time.Time

	once   sync.Once
	err    error
	etag   string // quoted, strong: the identity bytes
	gzETag string // the same with -gz: the gzip form is another representation
	gz     []byte // nil when the file isn't compressed (type, size, or no gain)
}

const (
	// maxFiles bounds the cache; past it the cache starts over (a build has far
	// fewer files, so this only matters for a web_dir whose files keep changing).
	maxFiles = 4096
	// maxCompress is the largest file kept compressed in memory.
	maxCompress = 16 << 20
)

// compressibleExts are the file types worth gzipping: text. Images, fonts and
// audio are compressed already.
var compressibleExts = map[string]bool{
	".html": true, ".js": true, ".mjs": true, ".css": true, ".json": true, ".svg": true,
	".webmanifest": true, ".map": true, ".txt": true, ".wasm": true,
}

// compressible reports whether a file named name is served gzipped to a client
// that accepts it.
func compressible(name string) bool { return compressibleExts[strings.ToLower(path.Ext(name))] }

// NewFiles returns an empty Files.
func NewFiles() *Files { return &Files{m: map[string]*fileRep{}, docs: map[string]*docRep{}} }

// Document returns the HTML document name's bytes (info its stat, content its
// bytes from the start) and csp(bytes), worked out once per version of the file.
func (f *Files) Document(name string, info fs.FileInfo, content io.Reader, csp func([]byte) string) ([]byte, string, error) {
	f.mu.Lock()
	d := f.docs[name]
	if d == nil || d.size != info.Size() || !d.mod.Equal(info.ModTime()) {
		if len(f.docs) >= maxFiles {
			clear(f.docs)
		}
		d = &docRep{size: info.Size(), mod: info.ModTime()}
		f.docs[name] = d
	}
	f.mu.Unlock()
	d.once.Do(func() {
		if d.data, d.err = io.ReadAll(content); d.err == nil {
			d.csp = csp(d.data)
		}
	})
	if d.err != nil {
		f.mu.Lock()
		if f.docs[name] == d {
			delete(f.docs, name) // not kept: the next request tries again
		}
		f.mu.Unlock()
		return nil, "", d.err
	}
	return d.data, d.csp, nil
}

// ServeFS serves the regular file name from fsys (404 when there is none). The
// caller sets any headers besides the representation's own first.
func (f *Files) ServeFS(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) {
	info, content, done, ok := openFile(fsys, name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	defer done()
	f.Serve(w, r, name, info, content)
}

// openFile opens the regular file name in fsys for serving: its stat and its
// bytes from the start. embed.FS and os.DirFS files seek, so they stream;
// anything else is read once. done closes the file; ok is false when there is
// no such file (or it can't be read).
func openFile(fsys fs.FS, name string) (info fs.FileInfo, content io.ReadSeeker, done func(), ok bool) {
	if !fs.ValidPath(name) {
		return nil, nil, nil, false
	}
	file, err := fsys.Open(name)
	if err != nil {
		return nil, nil, nil, false
	}
	done = func() { _ = file.Close() } // read-only
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		done()
		return nil, nil, nil, false
	}
	if rs, isSeeker := file.(io.ReadSeeker); isSeeker {
		return info, rs, done, true
	}
	data, err := io.ReadAll(file)
	if err != nil {
		done()
		return nil, nil, nil, false
	}
	return info, bytes.NewReader(data), done, true
}

// Serve writes the file name (info its stat, content its bytes from the start)
// as the answer to r, after the headers the caller has set (Content-Type,
// Cache-Control, the CSP). It adds the ETag and Vary, answers a matching
// If-None-Match with 304, and sends the gzip form when the client accepts it,
// the type is text and the request asks for no range; a Range request always
// gets the identity bytes, through http.ServeContent as before.
func (f *Files) Serve(w http.ResponseWriter, r *http.Request, name string, info fs.FileInfo, content io.ReadSeeker) {
	if compressible(name) {
		// The answer depends on Accept-Encoding even when this one isn't gzipped,
		// so a shared cache must not hand the gzip form to a client without it.
		w.Header().Add("Vary", "Accept-Encoding")
	}
	rep, err := f.rep(name, info, content)
	if err != nil {
		// A transient read error must not be cached (an asset's caller set a
		// year-long immutable Cache-Control), as http.ServeContent's errors aren't.
		w.Header().Del("Cache-Control")
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if rep.gz != nil && r.Header.Get("Range") == "" && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		serveGzip(w, r, name, info.ModTime(), rep)
		return
	}
	// ServeContent honours the ETag for If-None-Match, If-Match and If-Range, and
	// handles Range, HEAD and Content-Length. Embedded files carry no
	// modification time; it then omits Last-Modified.
	w.Header().Set("ETag", rep.etag)
	http.ServeContent(w, r, name, info.ModTime(), content)
}

// rep returns the file's validator and gzip form, working them out on first use
// (or when the file changed) from content, which it leaves at the start.
func (f *Files) rep(name string, info fs.FileInfo, content io.ReadSeeker) (*fileRep, error) {
	f.mu.Lock()
	rep := f.m[name]
	if rep == nil || rep.size != info.Size() || !rep.mod.Equal(info.ModTime()) {
		if len(f.m) >= maxFiles {
			clear(f.m)
		}
		rep = &fileRep{size: info.Size(), mod: info.ModTime()}
		f.m[name] = rep
	}
	f.mu.Unlock()
	rep.once.Do(func() { rep.err = rep.load(name, content) })
	if rep.err != nil {
		// Not kept: the next request tries again.
		f.mu.Lock()
		if f.m[name] == rep {
			delete(f.m, name)
		}
		f.mu.Unlock()
		return nil, rep.err
	}
	return rep, nil
}

// load hashes content and, for a compressible type, compresses it, then seeks
// content back to its start for serving.
func (rep *fileRep) load(name string, content io.ReadSeeker) error {
	sum := sha256.New()
	if compressible(name) && rep.size <= maxCompress {
		data, err := io.ReadAll(content)
		if err != nil {
			return err
		}
		sum.Write(data)
		rep.gz = gzipped(data)
	} else if _, err := io.Copy(sum, content); err != nil {
		return err
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tag := hex.EncodeToString(sum.Sum(nil)[:16])
	rep.etag, rep.gzETag = `"`+tag+`"`, `"`+tag+`-gz"`
	return nil
}

// gzipped compresses data at the default level (a few percent larger than the
// best, several times faster on the first request for a big bundle), or returns
// nil when that doesn't make it smaller.
func gzipped(data []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil || zw.Close() != nil || buf.Len() >= len(data) {
		return nil
	}
	return buf.Bytes()
}

// serveGzip writes rep's gzip form. http.ServeContent can't: it leaves out
// Content-Length whenever Content-Encoding is set and offers byte ranges, which
// here would be ranges of the identity bytes. Conditional requests follow
// ServeContent's rules for GET and HEAD: If-None-Match decides when present,
// otherwise If-Modified-Since.
func serveGzip(w http.ResponseWriter, r *http.Request, name string, mod time.Time, rep *fileRep) {
	h := w.Header()
	h.Set("ETag", rep.gzETag)
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", ContentType(name))
	}
	if hasModTime(mod) {
		h.Set("Last-Modified", mod.UTC().Format(http.TimeFormat))
	}
	if notModified(r, rep.gzETag, mod) {
		// RFC 9110 15.4.5: no representation metadata beyond the validators; as
		// http.ServeContent does, keep the ETag and drop Last-Modified.
		h.Del("Content-Type")
		h.Del("Content-Length")
		h.Del("Last-Modified")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Length", strconv.Itoa(len(rep.gz)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(rep.gz)
	}
}

// notModified reports whether a GET or HEAD for a representation with this ETag
// and modification time can be answered with 304.
func notModified(r *http.Request, etag string, mod time.Time) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		return ETagListMatches(inm, etag)
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" && hasModTime(mod) {
		t, err := http.ParseTime(ims)
		return err == nil && !mod.Truncate(time.Second).After(t)
	}
	return false
}

// hasModTime reports whether mod is a real modification time (embedded files
// have none), as http.ServeContent decides.
func hasModTime(mod time.Time) bool { return !mod.IsZero() && !mod.Equal(time.Unix(0, 0)) }

// ETagListMatches reports whether an If-None-Match list names etag ("*" names
// any), comparing weakly as RFC 9110 13.1.2 has it. The api's covers use it too.
func ETagListMatches(list, etag string) bool {
	for _, t := range strings.Split(list, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || strings.TrimPrefix(t, "W/") == etag {
			return true
		}
	}
	return false
}

// acceptsGzip reports whether an Accept-Encoding value allows gzip: listed (or
// x-gzip) with a non-zero q, or not listed while "*" is.
func acceptsGzip(header string) bool {
	gzipQ, starQ := -1.0, -1.0
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(part, ";")
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		switch strings.ToLower(strings.TrimSpace(coding)) {
		case "gzip", "x-gzip":
			gzipQ = q
		case "*":
			starQ = q
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return starQ > 0
}

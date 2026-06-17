// Package media serves audiobook bytes to clients. It uses http.ServeContent so
// HTTP Range requests (seek/scrub, resumable downloads) work out of the box,
// and extracts embedded cover art on demand. On-the-fly transcoding is a
// Phase C addition; today the file is served directly.
package media

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dhowden/tag"
)

// ServeFile streams absPath to the client with Range support. When download is
// true a Content-Disposition attachment header is set so browsers save the file.
func ServeFile(w http.ResponseWriter, r *http.Request, absPath string, download bool) {
	f, err := os.Open(absPath)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if download {
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(filepath.Base(absPath)))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	// ServeContent negotiates Range, sets Content-Type from the extension, and
	// handles conditional/partial responses.
	http.ServeContent(w, r, filepath.Base(absPath), info.ModTime(), f)
}

// EmbeddedCover returns embedded cover art from absPath, if present.
func EmbeddedCover(absPath string) (data []byte, mime string, ok bool) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, "", false
	}
	defer f.Close()
	md, err := tag.ReadFrom(f)
	if err != nil {
		return nil, "", false
	}
	pic := md.Picture()
	if pic == nil || len(pic.Data) == 0 {
		return nil, "", false
	}
	mime = pic.MIMEType
	if mime == "" {
		mime = "image/jpeg"
	}
	return pic.Data, mime, true
}

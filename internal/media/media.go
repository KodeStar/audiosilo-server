// Package media serves audiobook bytes to clients. It uses http.ServeContent so
// HTTP Range requests (seek/scrub, resumable downloads) work out of the box,
// and extracts embedded cover art on demand. On-the-fly transcoding is a
// Phase C addition; today the file is served directly.
package media

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dhowden/tag"
)

// sniffAudioType inspects the leading bytes of f to identify the audio container
// from its actual content (not the file extension), then seeks back to the
// start. Returns "" if unrecognized. This is what lets us serve a correct
// Content-Type even for mislabeled files; Go's http.DetectContentType doesn't
// recognize .m4b/.aax and yields application/octet-stream.
func sniffAudioType(f *os.File) string {
	var buf [16]byte
	n, _ := io.ReadFull(f, buf[:])
	_, _ = f.Seek(0, io.SeekStart) // rewind so ServeContent reads from the top
	b := buf[:n]
	switch {
	case len(b) >= 4 && string(b[0:4]) == "fLaC":
		return "audio/flac"
	case len(b) >= 4 && string(b[0:4]) == "OggS":
		return "audio/ogg"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "audio/wav"
	case len(b) >= 3 && string(b[0:3]) == "ID3":
		return "audio/mpeg" // ID3 tag — MP3
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xF6 == 0xF0:
		// ADTS AAC: 12-bit sync (0xFFF) with layer bits == 00. Checked before
		// the MP3 case because its sync word also matches the MPEG mask.
		return "audio/aac" // raw .aac (e.g. ADTS chapter files)
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		return "audio/mpeg" // MPEG-1/2 Audio Layer III (MP3)
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return "audio/mp4" // ISO base media (m4a/m4b/mp4) — AAC or ALAC inside
	}
	return ""
}

// audioContentType maps an audiobook file extension to its MIME type. Go's mime
// table doesn't know .m4b/.aax, so without this ServeContent falls back to
// application/octet-stream; combined with the nosniff header that makes strict
// players (iOS AVPlayer) refuse the stream. Empty string means "let
// ServeContent decide".
func audioContentType(ext string) string {
	switch strings.ToLower(ext) {
	case ".m4b", ".m4a", ".aax", ".mp4":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".aac":
		return "audio/aac"
	case ".flac":
		return "audio/flac"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".opus":
		return "audio/opus"
	case ".wav", ".wave":
		return "audio/wav"
	default:
		return ""
	}
}

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
	// Set an explicit audio Content-Type so strict players (iOS AVPlayer) accept
	// the stream; otherwise ServeContent yields application/octet-stream for
	// .m4b/.aax and the nosniff header blocks playback. Detect from the file's
	// actual bytes, falling back to the extension.
	ct := sniffAudioType(f)
	if ct == "" {
		ct = audioContentType(filepath.Ext(absPath))
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// ServeContent negotiates Range, sets Content-Type from the extension (unless
	// already set above), and handles conditional/partial responses.
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

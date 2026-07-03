// Package media serves audiobook bytes to clients. It uses http.ServeContent so
// HTTP Range requests (seek/scrub, resumable downloads) work out of the box, and
// extracts embedded cover art on demand. For files whose codec a browser can't
// decode it can transcode to MP3 on the fly via ffmpeg (see Transcode).
package media

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dhowden/tag"
)

// browserCodecs are audio codecs mainstream browsers decode natively, so they
// can be streamed directly instead of transcoded. Keys are ffprobe codec_name
// values (DirectPlayable is only ever called with book.Codec, which is populated
// verbatim from ffprobe) - e.g. AAC-in-MP4 reports "aac" (not the "mp4a" tag) and
// WAV reports "pcm_s16le" (not the "wav" container name).
var browserCodecs = map[string]bool{
	"aac": true, "mp3": true, "mp2": true,
	"flac": true, "opus": true, "vorbis": true, "pcm_s16le": true,
}

// DirectPlayable reports whether codec plays natively in browsers. An empty
// codec (ffprobe unavailable / not yet probed) is treated as playable: the
// client streams directly and can fall back to ?transcode=1 if that fails.
func DirectPlayable(codec string) bool {
	if codec == "" {
		return true
	}
	return browserCodecs[strings.ToLower(codec)]
}

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
		return "audio/mpeg" // ID3 tag - MP3
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xF6 == 0xF0:
		// ADTS AAC: 12-bit sync (0xFFF) with layer bits == 00. Checked before
		// the MP3 case because its sync word also matches the MPEG mask.
		return "audio/aac" // raw .aac (e.g. ADTS chapter files)
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		return "audio/mpeg" // MPEG-1/2 Audio Layer III (MP3)
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return "audio/mp4" // ISO base media (m4a/m4b/mp4) - AAC or ALAC inside
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

// Transcode streams absPath re-encoded to MP3 via ffmpeg, starting startSec into
// the file. It is the fallback for codecs browsers can't decode natively. The
// output is not byte-seekable (no Range / Content-Length); a client seeks by
// re-requesting with a new startSec. The ffmpeg process is bound to the request
// context, so a client disconnect kills it. log may be nil.
func Transcode(w http.ResponseWriter, r *http.Request, absPath, ffmpegPath string, startSec float64, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if _, err := os.Stat(absPath); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	args := []string{"-nostdin", "-loglevel", "error"}
	if startSec > 0 {
		// Input-side seek (fast): ffmpeg jumps near startSec before decoding.
		args = append(args, "-ss", strconv.FormatFloat(startSec, 'f', 3, 64))
	}
	args = append(args,
		"-i", absPath,
		"-vn",                // drop any cover-art/video stream
		"-c:a", "libmp3lame", // broadly compatible output
		"-b:a", "128k",
		"-f", "mp3",
		"pipe:1",
	)
	cmd := exec.CommandContext(r.Context(), ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "transcode failed", http.StatusInternalServerError)
		return
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		http.Error(w, "transcode failed", http.StatusInternalServerError)
		return
	}
	// Headers must go out before the body; no Accept-Ranges since the stream is
	// not byte-seekable.
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, stdout); err != nil {
		// Client disconnect / broken pipe is normal (seek, pause, navigate away).
		log.Debug("transcode copy ended", "path", absPath, "err", err)
	}
	if err := cmd.Wait(); err != nil && r.Context().Err() == nil {
		// Only a real ffmpeg failure (not a client cancel) is worth warning about.
		log.Warn("ffmpeg transcode failed", "path", absPath, "err", err, "stderr", strings.TrimSpace(stderr.String()))
	}
}

// HasFFmpeg reports whether an ffmpeg binary is resolvable at ffmpegPath (or on
// PATH when ffmpegPath is "ffmpeg"). Empty path means transcoding is disabled.
func HasFFmpeg(ffmpegPath string) bool {
	if ffmpegPath == "" {
		return false
	}
	_, err := exec.LookPath(ffmpegPath)
	return err == nil
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

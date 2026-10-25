// Package media serves audiobook bytes to clients. It uses http.ServeContent so
// HTTP Range requests (seek/scrub, resumable downloads) work out of the box, and
// extracts embedded cover art on demand. For files whose codec a browser can't
// decode it can transcode to MP3 on the fly via ffmpeg (see Transcode), and it
// remuxes MP3 in Matroska into a cached plain MP3 (see Remuxer).
package media

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

// containerFormats are the book formats (file extensions, no dot) whose
// container no browser plays, whatever the codec inside: Matroska audio (#161).
// DirectPlayable is about the stored bytes; MP3 in .mka is served as its remux
// to MP3 while ffmpeg is configured (Remuxer, Remuxable), which the API's
// direct_playable takes into account.
var containerFormats = map[string]bool{"mka": true}

// DirectPlayable reports whether a book of format (its file extension, no dot,
// as books.format) and codec plays natively in browsers. A container no browser
// plays (containerFormats) never does. An empty codec (ffprobe unavailable / not
// yet probed) is otherwise treated as playable: the client streams directly and
// can fall back to ?transcode=1 if that fails.
func DirectPlayable(format, codec string) bool {
	if containerFormats[strings.ToLower(format)] {
		return false
	}
	if codec == "" {
		return true
	}
	return browserCodecs[strings.ToLower(codec)]
}

// DirectPlayableSQL is DirectPlayable as an SQL condition over the format column
// formatCol and the codec column codecCol, for callers that filter on it in a
// query (the admin catalog). The names are this package's constants, inlined as
// literals.
func DirectPlayableSQL(formatCol, codecCol string) string {
	return "(lower(" + formatCol + ") NOT IN (" + sqlList(containerFormats) + ") AND (" +
		codecCol + " = '' OR lower(" + codecCol + ") IN (" + sqlList(browserCodecs) + ")))"
}

// sqlList is the keys of set as a sorted list of SQL string literals.
func sqlList(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, "'"+k+"'")
	}
	sort.Strings(out)
	return strings.Join(out, ",")
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
	case len(b) >= 4 && b[0] == 0x1A && b[1] == 0x45 && b[2] == 0xDF && b[3] == 0xA3:
		return "audio/x-matroska" // EBML header: Matroska (.mka), MP3 or other codecs inside
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
	case ".mka":
		return "audio/x-matroska"
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
	ServeOpened(w, r, f, download)
}

// ServeOpened serves an already verified descriptor without reopening its path.
// The caller owns the descriptor and any generation validator.
func ServeOpened(w http.ResponseWriter, r *http.Request, f *os.File, download bool) {
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "not found", 404)
		return
	}
	ServeOpenedAs(w, r, f, filepath.Base(f.Name()), info.ModTime(), download)
}

// SetAttachment makes the response a download saved as name (strconv.Quote
// gives the quoted-string form browsers expect).
func SetAttachment(h http.Header, name string) {
	h.Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
}

// ServeOpenedAs serves the open file f with Range under the name name (the
// download filename and the extension fallback for its type) and modTime as
// its Last-Modified: a remuxed copy is served as the source's stem with .mp3
// and the copy's own mtime.
func ServeOpenedAs(w http.ResponseWriter, r *http.Request, f *os.File, name string, modTime time.Time, download bool) {
	if download {
		SetAttachment(w.Header(), name)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	// Set an explicit audio Content-Type so strict players (iOS AVPlayer) accept
	// the stream; otherwise ServeContent yields application/octet-stream for
	// .m4b/.aax and the nosniff header blocks playback. Detect from the file's
	// actual bytes, falling back to the extension.
	ct := sniffAudioType(f)
	if ct == "" {
		ct = audioContentType(filepath.Ext(name))
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// ServeContent negotiates Range, sets Content-Type from the extension (unless
	// already set above), and handles conditional/partial responses.
	http.ServeContent(w, r, name, modTime, f)
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
	transcodeInput(w, r, inputSpec{url: absPath}, ffmpegPath, startSec, log)
}

// TranscodeOpened transcodes an already verified descriptor (a podcast
// episode's committed generation) without reopening its path: f is ffmpeg's
// stdin, read seekably through the fd: protocol when this ffmpeg has it (so
// -ss is a fast input seek and a file whose index sits at its end, a
// non-faststart m4a, demuxes), else as the old non-seekable pipe:0.
func TranscodeOpened(w http.ResponseWriter, r *http.Request, f *os.File, ffmpegPath string, startSec float64, log *slog.Logger) {
	// The child shares this descriptor's offset: start it at the top.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "transcode failed", http.StatusInternalServerError)
		return
	}
	in := inputFor(ffmpegPath)
	in.file = f
	transcodeInput(w, r, in, ffmpegPath, startSec, log)
}

// inputSpec is how ffmpeg reads its input: a path (proto ""), or the
// descriptor file handed over as its stdin through the fd: protocol (seekable)
// or pipe:0 (not seekable, for an ffmpeg older than 6.1).
type inputSpec struct {
	proto, url string
	file       *os.File
}

var (
	fdInput   = inputSpec{proto: "fd", url: "fd:"}
	pipeInput = inputSpec{proto: "pipe", url: "pipe:0"}
)

// fdSupport remembers, per ffmpeg path, whether its input protocols include
// fd (an *fdProbe), so each binary is asked once per process, however many
// first requests arrive together. A probe that couldn't run (its 5 s timeout,
// a failed start) is no answer: it is dropped, and the next request asks again.
var fdSupport sync.Map

// fdProbe is one ffmpeg binary's answer, settled once by probeFD.
type fdProbe struct {
	once    sync.Once
	fd, ran bool
}

// inputFor is the stdin input spec for ffmpegPath: fd: when that ffmpeg lists
// the fd input protocol, else pipe:0.
func inputFor(ffmpegPath string) inputSpec {
	v, _ := fdSupport.LoadOrStore(ffmpegPath, &fdProbe{})
	p := v.(*fdProbe)
	p.once.Do(func() { p.fd, p.ran = probeFD(ffmpegPath) })
	if !p.ran {
		fdSupport.CompareAndDelete(ffmpegPath, p)
	}
	if p.fd {
		return fdInput
	}
	return pipeInput
}

// probeFD asks ffmpegPath for its protocols (5 s at most) and reports whether
// the Input section lists fd, and whether the probe ran at all.
func probeFD(ffmpegPath string) (fd, ran bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-protocols").Output()
	if err != nil {
		return false, false
	}
	return listsInputProtocol(string(out), "fd"), true
}

// listsInputProtocol reports whether ffmpeg -protocols output lists name in its
// Input section (the lines between "Input:" and "Output:").
func listsInputProtocol(out, name string) bool {
	input := false
	for line := range strings.SplitSeq(out, "\n") {
		switch line = strings.TrimSpace(line); line {
		case "Input:":
			input = true
		case "Output:":
			input = false
		default:
			if input && line == name {
				return true
			}
		}
	}
	return false
}

// transcodeArgs is the ffmpeg command line that re-encodes in to MP3 on
// stdout from startSec. A path or the seekable fd: input seeks on the input
// side (-ss before -i: ffmpeg jumps near startSec before decoding); pipe:0
// can't seek, so there -ss follows -i and decodes up to it. A descriptor input
// is the only thing ffmpeg may open (-protocol_whitelist): publisher bytes that
// probe as a playlist may not open secondary local files.
func transcodeArgs(in inputSpec, startSec float64) []string {
	args := []string{"-nostdin", "-loglevel", "error"}
	if in.proto != "" {
		args = append(args, "-protocol_whitelist", in.proto)
	}
	var seek []string
	if startSec > 0 {
		seek = []string{"-ss", strconv.FormatFloat(startSec, 'f', 3, 64)}
	}
	if in.proto == "pipe" {
		args = append(append(args, "-i", in.url), seek...)
	} else {
		args = append(append(args, seek...), "-i", in.url)
	}
	return append(args,
		"-vn",                // drop any cover-art/video stream
		"-c:a", "libmp3lame", // broadly compatible output
		"-b:a", "128k",
		"-f", "mp3",
		"pipe:1",
	)
}

func transcodeInput(w http.ResponseWriter, r *http.Request, in inputSpec, ffmpegPath string, startSec float64, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if r.Method == http.MethodHead {
		// net/http discards a HEAD's body: answer its headers without running
		// ffmpeg over the whole file for nothing.
		writeTranscodeHeader(w)
		return
	}
	logPath := in.url
	cmd := exec.CommandContext(r.Context(), ffmpegPath, transcodeArgs(in, startSec)...)
	if in.file != nil {
		logPath = in.file.Name()
		cmd.Stdin = in.file
	}
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
	writeTranscodeHeader(w) // before the body
	if _, err := io.Copy(w, stdout); err != nil {
		// Client disconnect / broken pipe is normal (seek, pause, navigate away).
		log.Debug("transcode copy ended", "path", logPath, "err", err)
	}
	if err := cmd.Wait(); err != nil && r.Context().Err() == nil {
		// Only a real ffmpeg failure (not a client cancel) is worth warning about.
		log.Warn("ffmpeg transcode failed", "path", logPath, "err", err, "stderr", strings.TrimSpace(stderr.String()))
	}
}

// writeTranscodeHeader sends a transcode's status and headers: MP3, never
// cached, and no Accept-Ranges since the stream is not byte-seekable.
func writeTranscodeHeader(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
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

// coverImageTypes are the types cover art is served as (what
// http.DetectContentType names them).
var coverImageTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true, "image/bmp": true,
}

// coverMIME is the Content-Type embedded art is served with: what its bytes are,
// never what the tag claims, because the cover endpoint serves it from this
// origin. A tag declaring text/html (or bytes that are no image at all) would
// otherwise turn a crafted audio file into a page served by the server. ok is
// false for anything that isn't an image.
func coverMIME(data []byte) (string, bool) {
	mime := http.DetectContentType(data)
	return mime, coverImageTypes[mime]
}

// EmbeddedCover returns embedded cover art from absPath, if present, with its
// sniffed image type (see coverMIME); art that isn't an image is no art.
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
	if mime, ok = coverMIME(pic.Data); !ok {
		return nil, "", false
	}
	return pic.Data, mime, true
}

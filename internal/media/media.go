// Package media serves audiobook bytes to clients. It uses http.ServeContent so
// HTTP Range requests (seek/scrub, resumable downloads) work out of the box, and
// extracts embedded cover art on demand. For files whose codec a browser can't
// decode it can transcode to MP3 on the fly via ffmpeg (see Transcode). Two
// FileCaches keep copies ffmpeg made: MP3 in Matroska remuxed into a plain MP3
// (see NewRemuxer), and a small AAC copy of a file for a watch (see
// NewRenditionCache).
package media

import (
	"context"
	"errors"
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

	"github.com/kodestar/audiosilo-server/internal/metadata"
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
// to MP3 while ffmpeg is configured (NewRemuxer, Remuxable), which the API's
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
// Its open and stat are one metadata.Bounded call (the request's context, at
// most wait, the per-mount cap of a scan's reads): a file on a share that
// stopped answering is given up on, never a handler held for good (the reads
// that follow are the client's to wait for). That error (metadata.GaveUp) is
// returned with nothing written, for the caller to answer; every other outcome
// is answered here and returns nil. Only a regular file is served
// (metadata.OpenRegular): a FIFO named as audio is a 404 at once, never an open
// waiting for a writer, given up on and counted on its mount for good. A 404
// carries no Cache-Control a caller set for the file it expected.
func ServeFile(w http.ResponseWriter, r *http.Request, absPath string, download bool, wait time.Duration) error {
	type opened struct {
		f    *os.File
		info os.FileInfo
	}
	o, err := metadata.Bounded(r.Context(), absPath, wait, func() (opened, error) {
		f, info, err := metadata.OpenRegular(absPath)
		return opened{f, info}, err
	}, func(o opened) { _ = o.f.Close() })
	switch {
	case metadata.GaveUp(err):
		return err
	case err != nil:
		w.Header().Del("Cache-Control")
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	defer o.f.Close()
	ServeOpened(w, r, o.f, download)
	return nil
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
// its Last-Modified (none for the zero time): a cached copy (a remux, a
// rendition) is served as the source's stem with its own extension and no
// Last-Modified, its etag set by the caller.
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
// context, so a client disconnect kills it, and so does a client that has read
// nothing for idle (TranscodeIdle: the chapter editor's paused stream; 0 never
// ends a stream for that). log may be nil. Its look at the
// file is a metadata.Bounded stat (the request's context, at most wait), as
// ServeFile's open: one given up on (metadata.GaveUp) is returned with nothing
// written, for the caller to answer; every other outcome is answered here and
// returns nil.
func Transcode(w http.ResponseWriter, r *http.Request, absPath, ffmpegPath string, startSec float64, wait, idle time.Duration, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	switch _, err := metadata.StatBounded(r.Context(), absPath, wait); {
	case metadata.GaveUp(err):
		return err
	case err != nil:
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	transcodeInput(w, r, inputSpec{url: absPath}, ffmpegPath, startSec, idle, log)
	return nil
}

// TranscodeOpened transcodes an already verified descriptor (a podcast
// episode's committed generation) without reopening its path: f is ffmpeg's
// stdin, read seekably through the fd: protocol when this ffmpeg has it (so
// -ss is a fast input seek and a file whose index sits at its end, a
// non-faststart m4a, demuxes), else as the old non-seekable pipe:0. idle is
// Transcode's.
func TranscodeOpened(w http.ResponseWriter, r *http.Request, f *os.File, ffmpegPath string, startSec float64, idle time.Duration, log *slog.Logger) {
	// The child shares this descriptor's offset: start it at the top.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "transcode failed", http.StatusInternalServerError)
		return
	}
	in := inputFor(ffmpegPath)
	in.file = f
	transcodeInput(w, r, in, ffmpegPath, startSec, idle, log)
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

// TranscodeIdle is how long a transcode the idle end applies to waits for its
// client to read before it ends the stream (copyIdle): a paused <audio> stops
// reading once its buffer is full, yet keeps the connection, and with it one
// of the server's few transcode slots and an ffmpeg, for as long as the page
// stays open. A playing client reads every few seconds (a browser buffers a
// few minutes of it at most). The api applies it only to the console's
// chapter editor, which asks for its stream again after a 10 s pause; the web
// player does not ask again for a track it loaded and never played, and a
// Cast receiver asks for nothing again on resume, so theirs get no idle end
// (the api's transcodeIdleFor).
const TranscodeIdle = 10 * time.Minute

// transcodeInput runs ffmpeg over in from startSec and streams its MP3 to w,
// ending it (and ffmpeg) when the client has read nothing for idle.
func transcodeInput(w http.ResponseWriter, r *http.Request, in inputSpec, ffmpegPath string, startSec float64, idle time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if r.Method == http.MethodHead {
		// net/http discards a HEAD's body: answer its headers without running
		// ffmpeg over the whole file for nothing.
		writeTranscodeHeader(w)
		return
	}
	// Ended with the request, and at once when the copy fails (copyIdle's
	// deadline): net/http ends the request's context on a failed write too, but
	// ffmpeg, blocked writing to a pipe nobody reads any more, must never be
	// left for cmd.Wait to wait on.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	logPath := in.url
	cmd := exec.CommandContext(ctx, ffmpegPath, transcodeArgs(in, startSec)...)
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
	if err := copyIdle(w, stdout, idle); err != nil {
		// Client disconnect / broken pipe is normal (seek, pause, navigate away),
		// and so is a client that stopped reading (copyIdle).
		log.Debug("transcode copy ended", "path", logPath, "err", err)
		cancel()
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		// Only a real ffmpeg failure (not a client cancel) is worth warning about.
		log.Warn("ffmpeg transcode failed", "path", logPath, "err", err, "stderr", strings.TrimSpace(stderr.String()))
	}
}

// copyIdle copies src to w as io.Copy does, each write given idle to reach
// the client (a write deadline, moved on before every write): one the client
// has not read in that time fails, ending the copy. A ResponseWriter with no
// deadlines (a test's recorder) copies with none.
func copyIdle(w http.ResponseWriter, src io.Reader, idle time.Duration) error {
	if idle <= 0 {
		_, err := io.Copy(w, src)
		return err
	}
	rc := http.NewResponseController(w)
	_, err := io.Copy(deadlineWriter{w: w, rc: rc, idle: idle}, src)
	if err == nil {
		// The bytes still buffered reach the client under the deadline too:
		// net/http's own flush after the handler has none.
		if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
			err = ferr
		}
	}
	// The connection outlives the answer (keep-alive): its next one has no deadline.
	_ = rc.SetWriteDeadline(time.Time{})
	return err
}

// deadlineWriter is copyIdle's writer: each Write moves the write deadline on.
type deadlineWriter struct {
	w    io.Writer
	rc   *http.ResponseController
	idle time.Duration
}

func (d deadlineWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(d.idle)) // ErrNotSupported: no deadline
	return d.w.Write(p)
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

// EmbeddedCover returns embedded cover art from absPath with its sniffed image
// type (see coverMIME): nil data and no error when the file holds none (art
// that isn't an image is no art, and so is a file whose tags don't read). An
// error is a file that couldn't be opened or read right now: its open and tag
// read are one metadata.Bounded call (ctx, at most timeout, the per-mount cap
// of a scan's reads), so a share that stopped answering costs at most timeout
// (metadata.GaveUp), never a caller held for good; a FIFO is turned away at
// once (metadata.OpenRegular), never waited on for a writer.
func EmbeddedCover(ctx context.Context, absPath string, timeout time.Duration) (data []byte, mime string, err error) {
	type art struct {
		data []byte
		mime string
	}
	a, err := metadata.Bounded(ctx, absPath, timeout, func() (art, error) {
		f, _, err := metadata.OpenRegular(absPath)
		if err != nil {
			return art{}, err
		}
		defer f.Close()
		md, err := metadata.ReadTags(f)
		if err != nil {
			return art{}, nil
		}
		pic := md.Picture()
		if pic == nil || len(pic.Data) == 0 {
			return art{}, nil
		}
		mime, ok := coverMIME(pic.Data)
		if !ok {
			return art{}, nil
		}
		return art{pic.Data, mime}, nil
	}, nil)
	return a.data, a.mime, err
}

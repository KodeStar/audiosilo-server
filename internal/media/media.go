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

// ServeFile streams absPath, a file its caller joined and checked under the
// library root root, to the client with Range support. When download is
// true a Content-Disposition attachment header is set so browsers save the file.
// The file is opened through root (metadata.OpenRegularBelow), so the open
// can't leave the library whatever changed on disk since the caller's check
// (a folder or the file swapped for a link out of it): that is a 404.
// Its open and stat are one metadata.Bounded call (the request's context, at
// most wait, the per-mount cap of a scan's reads): a file on a share that
// stopped answering is given up on, never a handler held for good (the reads
// that follow are the client's to wait for). That error, or a transient one
// (openLibraryFile: metadata.GaveUp either way), is returned with nothing
// written, for the caller to answer (a 503 share_not_answering); every other
// outcome is answered here and returns nil, a lasting refusal (a permission)
// the 404 a file gone is. Only a regular file is served
// (metadata.OpenRegular): a FIFO named as audio is a 404 at once, never an open
// waiting for a writer, given up on and counted on its mount for good. A 404
// carries no Cache-Control a caller set for the file it expected.
func ServeFile(w http.ResponseWriter, r *http.Request, root, absPath string, download bool, wait time.Duration) error {
	f, info, err := openLibraryFile(r.Context(), root, absPath, wait)
	switch {
	case metadata.GaveUp(err):
		return err
	case err != nil:
		w.Header().Del("Cache-Control")
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	defer f.Close()
	// Named as asked for: a link's file opens under its own name.
	ServeOpenedAs(w, r, f, filepath.Base(absPath), info.ModTime(), download)
	return nil
}

// openBelow is the open of a library file a request named (ServeFile,
// Transcode, a file cache's run): metadata.OpenRegularBelowBounded, a test's
// to inject a share's errors.
var openBelow = metadata.OpenRegularBelowBounded

// openLibraryFile is openBelow with an open that failed as a share does that a
// retry may get past made one given up on (transientGaveUp), so its caller
// tells a share that did not answer (a 503 share_not_answering) from the file
// gone with metadata.GaveUp alone.
func openLibraryFile(ctx context.Context, root, abs string, wait time.Duration) (*os.File, os.FileInfo, error) {
	f, info, err := openBelow(ctx, root, abs, wait)
	return f, info, transientGaveUp(err)
}

// transientGaveUp is err made a look given up on (metadata.AsGaveUp) when it
// is a share's fault a retry may get past (metadata.TransientFault: EIO,
// ESTALE), which a route answers 503 share_not_answering, never as the file
// gone; else err as it is (a lasting refusal, a permission, is the 404).
func transientGaveUp(err error) error {
	if metadata.TransientFault(err) {
		return metadata.AsGaveUp(err)
	}
	return err
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

// SetAttachment makes the response a download saved as name: its
// quoted-string filename (strconv.Quote, which escapes only `"` and `\` in
// printable ASCII) and, for a name with any other byte, an ASCII stand-in
// there (an underscore for each other rune) with the name itself as RFC 6266's
// filename* (RFC 8187's UTF-8 percent-encoding), which browsers prefer: a
// quoted-string has no escape for a rune past ASCII (Quote's \u3000, an
// ideographic space, is saved as "u3000"), and raw UTF-8 in one is no
// standard's.
func SetAttachment(h http.Header, name string) {
	const disposition = "attachment; filename="
	if printableASCII(name) {
		h.Set("Content-Disposition", disposition+strconv.Quote(name))
		return
	}
	fallback := strings.Map(func(r rune) rune {
		if r < ' ' || r > '~' {
			return '_'
		}
		return r
	}, name)
	h.Set("Content-Disposition", disposition+strconv.Quote(fallback)+
		"; filename*=UTF-8''"+extValue(strings.ToValidUTF8(name, "\uFFFD")))
}

// printableASCII reports whether s is printable ASCII only.
func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// extValue is s as RFC 8187's value-chars: its attr-chars as they are, every
// other byte percent-encoded.
func extValue(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0xF])
	}
	return b.String()
}

// StemName is the download name of a copy made from the file at path (a
// remux, a rendition, a transcode): its file name with ext for its extension.
func StemName(path, ext string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) + ext
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

// Transcode streams absPath, a file its caller joined and checked under the
// library root root, re-encoded to MP3 via ffmpeg, starting startSec into
// the file. It is the fallback for codecs browsers can't decode natively. The
// output is not byte-seekable (no Range / Content-Length); a client seeks by
// re-requesting with a new startSec. The ffmpeg process is bound to the request
// context, so a client disconnect kills it, and so does a client that has read
// nothing for idle (TranscodeIdle: the chapter editor's paused stream, a
// download its client stopped reading; 0 never ends a stream for that). log
// may be nil. The file is opened through root as ServeFile opens it
// (metadata.OpenRegularBelow, one metadata.Bounded call: the request's
// context, at most wait) and ffmpeg reads that descriptor (fileInput), never
// the path: a name along it swapped for a link out of the library after the
// caller's check is refused (a 404), never followed by ffmpeg's own open. An
// open given up on (openLibraryFile), or the path's re-check where ffmpeg is
// handed one (fileInput), is returned with nothing written, for the caller to
// answer (ServeFile's rule);
// every other outcome is answered here and returns nil. ended reports a
// stream that ran to its end (transcodeInput).
// download makes the stream an attachment named for the file (its stem with
// the MP3 extension), set only on the stream's own answer, never an error's.
func Transcode(w http.ResponseWriter, r *http.Request, root, absPath, ffmpegPath string, startSec float64, download bool, wait, idle time.Duration, log *slog.Logger) (ended bool, err error) {
	f, _, err := openLibraryFile(r.Context(), root, absPath, wait)
	switch {
	case metadata.GaveUp(err):
		return false, err
	case err != nil:
		http.Error(w, "not found", http.StatusNotFound)
		return false, nil
	}
	defer f.Close()
	if r.Method == http.MethodHead {
		// Answered with no ffmpeg run (transcodeInput): no probe for the input
		// either.
		return transcodeInput(w, r, inputSpec{}, absPath, ffmpegPath, startSec, download, idle, log), nil
	}
	in, cleanup, err := fileInput(r.Context(), ffmpegPath, f, root, absPath, wait)
	switch {
	case metadata.GaveUp(err):
		return false, err // the re-check's looks, on a share that stopped answering
	case err != nil:
		// The path no longer leads to the file opened (fileInput's re-check
		// where ffmpeg is handed a path): as a file gone.
		http.Error(w, "not found", http.StatusNotFound)
		return false, nil
	}
	defer cleanup()
	return transcodeInput(w, r, in, absPath, ffmpegPath, startSec, download, idle, log), nil
}

// TranscodeOpened transcodes an already verified descriptor (a podcast
// episode's committed generation) without reopening its path: f is ffmpeg's
// stdin, read seekably through the fd: protocol when this ffmpeg has it (so
// -ss is a fast input seek and a file whose index sits at its end, a
// non-faststart m4a, demuxes), else as the old non-seekable pipe:0 (never a
// path: publisher bytes that probe as a playlist may open no other file).
// download, idle and ended are Transcode's (the attachment named for f's
// name).
func TranscodeOpened(w http.ResponseWriter, r *http.Request, f *os.File, ffmpegPath string, startSec float64, download bool, idle time.Duration, log *slog.Logger) (ended bool) {
	if r.Method == http.MethodHead {
		// As Transcode's: no ffmpeg run, so no probe for the input either.
		return transcodeInput(w, r, inputSpec{}, f.Name(), ffmpegPath, startSec, download, idle, log)
	}
	// The child shares this descriptor's offset: start it at the top.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "transcode failed", http.StatusInternalServerError)
		return false
	}
	in := inputFor(ffmpegPath)
	in.file = f
	return transcodeInput(w, r, in, f.Name(), ffmpegPath, startSec, download, idle, log)
}

// fileInput is how ffmpeg reads f, the library file at path under root,
// opened through root (Transcode), without resolving its path again: f as
// its stdin through the fd: protocol when this ffmpeg has it (inputFor), else
// handed over as metadata.ToolInput hands every tool a library file (its
// ToolInputBounded: ctx, at most wait), read by the file protocol: on Unix
// the child's descriptor 3 through a link named for path's extension, read
// seekably (an input -ss; an MP4 whose index sits at its end demuxes, which
// pipe:0 can't), never a path in the library; elsewhere path resolved,
// checked to be under root and f, and held so until cleanup. Since the file
// protocol opens any local file, the input may only be an audio container
// (transcodeArgs: metadata.InputFormatArgs), never a playlist naming others;
// fd: and pipe:0 open none whatever the demuxer, which is why a podcast
// episode, publisher bytes, never takes this fallback. cleanup, called once
// ffmpeg is done, lets go of what was made for it. An error is a path that no
// longer leads to f, or the looks given up on (metadata.GaveUp).
func fileInput(ctx context.Context, ffmpegPath string, f *os.File, root, path string, wait time.Duration) (in inputSpec, cleanup func(), err error) {
	if in := inputFor(ffmpegPath); in.proto == fdInput.proto {
		in.file = f
		return in, func() {}, nil
	}
	input, extra, cleanup, err := metadata.ToolInputBounded(ctx, f, root, path, wait)
	if err != nil {
		return inputSpec{}, nil, transientGaveUp(err)
	}
	return inputSpec{proto: "file", url: "file:" + input, extra: extra}, cleanup, nil
}

// inputSpec is how ffmpeg reads its input: the descriptor file handed over as
// its stdin through the fd: protocol (seekable) or pipe:0 (not seekable, for
// an ffmpeg older than 6.1), or what metadata.ToolInput hands a tool, read by
// the file protocol: extra its inherited descriptors (exec.Cmd.ExtraFiles),
// none for a path. proto is the one protocol ffmpeg may open
// (-protocol_whitelist).
type inputSpec struct {
	proto, url string
	file       *os.File
	extra      []*os.File
}

var (
	fdInput   = inputSpec{proto: "fd", url: "fd:"}
	pipeInput = inputSpec{proto: "pipe", url: "pipe:0"}
)

// fdSupport remembers, per ffmpeg path, whether its input protocols include
// fd (an *fdProbe), so each binary is asked once per process, however many
// first requests arrive together, and no transcode or seek (each ?t= starts
// one) runs a probe of its own. A probe that couldn't run is no answer: one
// that ran past fdProbeTimeout (a busy host, a cold binary) is dropped, and
// the next request asks again, so a slow start never stands as "no fd:" (the
// caches' pipe:0, which can't demux an MP4 whose index sits at its end); one
// that failed (no such binary, an exit status) stands for fdProbeRetry, so an
// ffmpeg that won't answer is not asked again by every request.
var fdSupport sync.Map

// fdProbeRetry is how long a probe that failed stands as "no fd:".
const fdProbeRetry = time.Minute

// fdProbeTimeout bounds one probe; a variable for a test.
var fdProbeTimeout = 5 * time.Second

// fdProbe is one ffmpeg binary's answer, settled once by probeFD at at.
type fdProbe struct {
	once              sync.Once
	fd, ran, timedOut bool
	at                time.Time
}

// inputFor is the stdin input spec for ffmpegPath: fd: when that ffmpeg lists
// the fd input protocol, else pipe:0.
func inputFor(ffmpegPath string) inputSpec {
	v, _ := fdSupport.LoadOrStore(ffmpegPath, &fdProbe{})
	p := v.(*fdProbe)
	p.once.Do(func() { p.fd, p.ran, p.timedOut = probeFD(ffmpegPath); p.at = time.Now() })
	if !p.ran && (p.timedOut || time.Since(p.at) >= fdProbeRetry) {
		fdSupport.CompareAndDelete(ffmpegPath, p)
	}
	if p.fd {
		return fdInput
	}
	return pipeInput
}

// probeFD asks ffmpegPath for its protocols (fdProbeTimeout at most) and
// reports whether the Input section lists fd, whether the probe ran at all,
// and, when it didn't, whether it ran out of time.
func probeFD(ffmpegPath string) (fd, ran, timedOut bool) {
	ctx, cancel := context.WithTimeout(context.Background(), fdProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-protocols").Output()
	if err != nil {
		return false, false, ctx.Err() != nil
	}
	return listsInputProtocol(string(out), "fd"), true, false
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
// stdout from startSec. A seekable input (fd:, /dev/fd/3, a path) seeks on the
// input side (-ss before -i: ffmpeg jumps near startSec before decoding);
// pipe:0 can't seek, so there -ss follows -i and decodes up to it. The input's
// protocol is the only one ffmpeg may open (-protocol_whitelist): never the
// network, and through fd: or pipe:0 no other file either. The input may only
// be an audio container (metadata.InputFormatArgs, the one list every tool
// run on a library file takes), never a manifest or a playlist naming other
// inputs (DASH, HLS, ffconcat), which the file protocol would open: another
// library's audio, transcoded to whoever could stream the crafted file.
func transcodeArgs(in inputSpec, startSec float64) []string {
	args := append([]string{"-nostdin", "-loglevel", "error", "-protocol_whitelist", in.proto}, metadata.InputFormatArgs()...)
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
// chapter editor, which asks for its stream again after a 10 s pause, and to
// a download (?download=1), whose slot no other ask takes over and whose
// client reads as fast as it can until it is paused or gone; the web player
// does not ask again for a track it loaded and never played, and a Cast
// receiver asks for nothing again on resume, so their streams get no idle end
// (the api's transcodeIdleFor).
const TranscodeIdle = 10 * time.Minute

// transcodeWaitDelay bounds Wait's wait for a killed ffmpeg's output pipes
// (exec.Cmd.WaitDelay): a pipe held open past ffmpeg's end never keeps the
// handler, or the process unreaped.
const transcodeWaitDelay = 5 * time.Second

// setInput hands in's descriptors to cmd: those the file protocol's input
// names (fileInput: metadata.ToolInput's), else file as its stdin.
func setInput(cmd *exec.Cmd, in inputSpec) {
	cmd.ExtraFiles = in.extra
	if in.file != nil {
		cmd.Stdin = in.file
	}
}

// transcodeInput runs ffmpeg over in from startSec and streams its MP3 to w,
// ending it (and ffmpeg) when the client has read nothing for idle; logPath
// names the file in its log lines and a download's name. It reports
// whether the stream ran to its end: ffmpeg's output came to its end, all of it
// written, while the request's context was live. One cut short (the context
// ended, so ffmpeg was killed, which ends its output too; a write that
// failed) did not, and neither did a HEAD or a failure to start.
func transcodeInput(w http.ResponseWriter, r *http.Request, in inputSpec, logPath, ffmpegPath string, startSec float64, download bool, idle time.Duration, log *slog.Logger) (ended bool) {
	if log == nil {
		log = slog.Default()
	}
	name := ""
	if download {
		name = StemName(logPath, transcodeExt)
	}
	if r.Method == http.MethodHead {
		// net/http discards a HEAD's body: answer its headers without running
		// ffmpeg over the whole file for nothing.
		writeTranscodeHeader(w, name)
		return false
	}
	// Ended with the request, and at once when the copy fails (copyIdle's
	// deadline): net/http ends the request's context on a failed write too, but
	// ffmpeg, blocked writing to a pipe nobody reads any more, must never be
	// left for cmd.Wait to wait on.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpegPath, transcodeArgs(in, startSec)...)
	// Once ctx ends (ffmpeg killed), Wait gives its output pipes this long to
	// close before it closes them itself: ffmpeg is always reaped.
	cmd.WaitDelay = transcodeWaitDelay
	setInput(cmd, in)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "transcode failed", http.StatusInternalServerError)
		return false
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		http.Error(w, "transcode failed", http.StatusInternalServerError)
		return false
	}
	writeTranscodeHeader(w, name) // before the body
	out := &endWatch{r: stdout, ctx: ctx}
	err = copyIdle(w, out, idle)
	if err != nil {
		// Client disconnect / broken pipe is normal (seek, pause, navigate away),
		// and so is a client that stopped reading (copyIdle).
		log.Debug("transcode copy ended", "path", logPath, "err", err)
		cancel()
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		// Only a real ffmpeg failure (not a client cancel) is worth warning about.
		log.Warn("ffmpeg transcode failed", "path", logPath, "err", err, "stderr", strings.TrimSpace(stderr.String()))
	}
	return err == nil && out.ended
}

// endWatch is ffmpeg's output as transcodeInput copies it, noting whether it
// came to its end while ctx (the one ffmpeg runs under) was live: looked at as
// the end is read, before the bytes still buffered are flushed and before
// ffmpeg is waited for, so an end of ctx after that (a later ask taking the
// transcode's slot over) leaves the stream ended. ffmpeg killed as ctx ends
// ends its output too, but ctx has ended by then.
type endWatch struct {
	r     io.Reader
	ctx   context.Context
	ended bool
}

func (e *endWatch) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		e.ended = e.ctx.Err() == nil
	}
	return n, err
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

// transcodeExt is a transcode's file extension (it is MP3), for a download's
// name.
const transcodeExt = ".mp3"

// writeTranscodeHeader sends a transcode's status and headers: MP3, never
// cached, and no Accept-Ranges since the stream is not byte-seekable; a
// non-empty downloadName makes it an attachment of that name.
func writeTranscodeHeader(w http.ResponseWriter, downloadName string) {
	if downloadName != "" {
		SetAttachment(w.Header(), downloadName)
	}
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

// EmbeddedCoverBelow returns embedded cover art from absPath, a file its
// caller joined and checked under the library root root (a request's, or the
// scan's cover backfill), with its sniffed image type (see coverMIME): nil
// data and no error when the file holds none (art that isn't an image is no
// art, and so is a file whose tags don't read). The file is opened through
// root (metadata.OpenRegularBelow): the open can't leave the library whatever
// changed on disk since the check. An error is a file that couldn't be opened
// or read right now: its open and tag read are one metadata.Bounded call
// (ctx, at most timeout, the per-mount cap of a scan's reads), so a share
// that stopped answering costs at most timeout (metadata.GaveUp), never a
// caller held for good; a FIFO is turned away at once, never waited on for a
// writer.
func EmbeddedCoverBelow(ctx context.Context, root, absPath string, timeout time.Duration) (data []byte, mime string, err error) {
	type art struct {
		data []byte
		mime string
	}
	a, err := metadata.Bounded(ctx, absPath, timeout, func() (art, error) {
		f, _, err := metadata.OpenRegularBelow(root, absPath)
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

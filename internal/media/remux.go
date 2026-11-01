package media

import (
	"log/slog"
	"strings"
	"time"
)

// The remux cache's bounds (server#174): the total its copies may hold, how
// many run at once (an I/O-bound stream copy, not an encoder: its own limit,
// apart from the transcode slots) and how long one may take.
const (
	remuxCacheBytes = 8 << 30
	remuxConcurrent = 2
	remuxTimeout    = 30 * time.Minute
)

// Remuxable reports whether a book of format (its file extension, no dot) and
// codec is MP3 in Matroska, which the remux cache (NewRemuxer) copies
// losslessly into a plain MP3 every client plays.
func Remuxable(format, codec string) bool {
	return strings.EqualFold(format, "mka") && strings.EqualFold(codec, "mp3")
}

// NewRemuxer opens the remux cache in dir: a Matroska audio file holding MP3
// turned into a cached plain MP3 (a lossless stream copy with the chapters as
// ID3 CHAP frames), so a client that can't open the container (iOS, every
// browser) plays it with Range seeks at the same stream URL. ErrRefused from
// its Open means the stored bytes are the answer. log may be nil.
func NewRemuxer(dir string, log *slog.Logger) (*FileCache, error) {
	return newFileCache(dir, cacheSpec{
		ext: ".mp3", maxBytes: remuxCacheBytes, concurrent: remuxConcurrent, timeout: remuxTimeout,
		args: remuxArgs, etag: func(key string, _ int64) string { return remuxETag(key) },
		refused: "not remuxed: serving Matroska as stored",
		lowDisk: "remux skipped: too little free disk for the remux cache; transcoding Matroska live",
	}, log)
}

// remuxETag is the strong validator of key's copy: the key names the source's
// path, size and mtime, and the remux is bitexact, so the same key is always
// the same bytes, across a re-remux after an eviction too.
func remuxETag(key string) string { return `"remux-` + key + `"` }

// remuxArgs is the ffmpeg command line that copies the first audio stream of
// the Matroska on stdin (in) into an MP3 at out, chapters included. The demuxer
// is forced and the input may only be the descriptor, so crafted bytes can't
// make ffmpeg open other files; -xerror fails the run on a read error, where
// ffmpeg would otherwise end the input there and exit 0 with a short copy (a
// network mount's hiccup). Bitexact output is byte-identical every run,
// so an If-Range resume holds across a re-remux. ID3v2.3 carries the chapters
// as CHAP frames, and the muxer writes a Xing/Info header (a real duration for
// iOS).
func remuxArgs(in inputSpec, out string) []string {
	return []string{
		"-nostdin", "-loglevel", "error", "-xerror",
		"-protocol_whitelist", in.proto,
		"-f", "matroska", "-i", in.url,
		"-map", "0:a:0", "-c:a", "copy", "-map_chapters", "0",
		"-fflags", "+bitexact", "-id3v2_version", "3",
		"-f", "mp3", "-y", "file:" + out,
	}
}

package media

import (
	"log/slog"
	"strconv"
	"time"
)

// RenditionWatch is the one rendition kind: a small AAC copy of an audio file
// for a watch to download (Track W2).
const RenditionWatch = "watch"

// RenditionKinds are the rendition kinds this server makes (GET /server's
// top-level "renditions" while they are on).
var RenditionKinds = []string{RenditionWatch}

// The watch rendition cache's bounds: the total its copies may hold, one
// encode at a time (an encoder pins a core, unlike the remux's stream copy),
// how long one may take (a 40 h book is minutes at ffmpeg's AAC speed; the
// bound is for a stuck run), and how many files a book's prepare (Queue) may
// have waiting at once across the server.
const (
	renditionCacheBytes = 10 << 30
	renditionConcurrent = 1
	renditionTimeout    = 2 * time.Hour
	renditionQueueMax   = 256
	// renditionVersion is the watch encode's version, part of the cache key and
	// the ETag: a change to renditionArgs bumps it, so old copies are never
	// served for the new encode (they age out of the LRU).
	renditionVersion = "v1"
)

// NewRenditionCache opens the watch rendition cache in dir: each audio file of
// a book encoded once to AAC-LC, mono, 48 kbps, 44.1 kHz in MP4 (renditionArgs),
// the same duration as the source so positions and chapters hold, cached
// beside the library (which stays read-only). ErrRefused from its Open means
// ffmpeg can't decode the file. log may be nil.
func NewRenditionCache(dir string, log *slog.Logger) (*FileCache, error) {
	return newFileCache(dir, cacheSpec{
		ext: ".m4a", tag: RenditionWatch + "-" + renditionVersion,
		maxBytes: renditionCacheBytes, concurrent: renditionConcurrent, timeout: renditionTimeout,
		queueMax: renditionQueueMax, args: renditionArgs, etag: renditionETag,
		refused: "no watch rendition: ffmpeg refused the file",
		lowDisk: "watch rendition skipped: too little free disk for the rendition cache",
	}, log)
}

// renditionETag is the strong validator of key's copy of size bytes: the key
// names the source's path, size, mtime and the encode's version, and the size
// tells two encodes apart should a re-encode after an eviction ever differ, so
// a resumed download (If-Range) never splices two encodes.
func renditionETag(key string, size int64) string {
	return `"rw1-` + key + "-" + strconv.FormatInt(size, 10) + `"`
}

// renditionArgs is the ffmpeg command line that encodes the first audio stream
// of the file on stdin (in) into the watch rendition at out: AAC-LC (ffmpeg's
// own encoder; HE-AAC needs the non-free libfdk_aac), one channel, 48 kbps,
// 44.1 kHz, in MP4 with its index first (+faststart: a watch player starts
// before the end arrives). No video (cover art), no metadata, no chapters (the
// watch reads the book's from the API). The input may only be the descriptor
// (-protocol_whitelist), so crafted bytes that probe as a playlist can't make
// ffmpeg open other files; the demuxer is ffmpeg's guess, since the library
// holds every audio format. Bitexact output names no encoder version, so the
// same ffmpeg writes the same bytes every run. Unlike the remux there is no
// -xerror: it makes a decode error (a damaged MP3 frame, which playback
// skips) fail the whole encode.
func renditionArgs(in inputSpec, out string) []string {
	return []string{
		"-nostdin", "-loglevel", "error",
		"-protocol_whitelist", in.proto, "-i", in.url,
		"-map", "0:a:0", "-vn", "-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
		"-c:a", "aac", "-b:a", "48k", "-ac", "1", "-ar", "44100",
		"-fflags", "+bitexact", "-flags:a", "+bitexact",
		"-movflags", "+faststart", "-f", "mp4", "-y", "file:" + out,
	}
}

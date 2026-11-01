package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// RenditionWatch is the one rendition kind: a small AAC copy of an audio file
// for a watch to download (Track W2).
const RenditionWatch = "watch"

// The watch rendition cache's bounds: the total its copies may hold, one
// encode at a time (an encoder pins a core, unlike the remux's stream copy),
// how long one may take (a 40 h book is minutes at ffmpeg's AAC speed; the
// bound is for a stuck run), and how many encodes may be waiting or running at
// once across the server (a book's prepare, Queue, and a stream's Open alike).
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
		queueMax: renditionQueueMax, args: renditionArgs, etag: renditionETag, check: renditionCheck,
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

// renditionShortMax is how much shorter than its source a watch copy may be
// (AAC's priming and a gapless MP3's padding are milliseconds; a read error
// that ended the encode early is more).
const renditionShortMax = 1.0

// errShortCopy is a watch copy shorter than its source by more than
// renditionShortMax: not cached, and not remembered (a read error partway, an
// EIO from a NAS, makes ffmpeg stop and still exit 0, since there is no
// -xerror), so the next request encodes it again.
var errShortCopy = errors.New("the rendition is shorter than its source")

// renditionCheck looks at the watch copy ffmpeg wrote at out from src (size
// bytes): errNoAudio when it holds no audio track (ffmpeg exits 0 having
// written an MP4 with no track when it read no audio at all: an M4B whose index
// sits at its end, read through pipe:0 by an ffmpeg without the fd protocol,
// which can't seek back to the samples), errShortCopy when it is short of the
// source's exact duration (metadata.ExactDuration; a source whose duration is
// only an estimate, a VBR MP3 with no header, isn't held to it, nor is a copy
// the probe can't measure).
func renditionCheck(ctx context.Context, src *os.File, size int64, out string) error {
	f, err := os.Open(out)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !metadata.MP4HasTrack(f, info.Size()) {
		return errNoAudio
	}
	want, ok := metadata.ExactDuration(ctx, src, size, strings.TrimPrefix(filepath.Ext(src.Name()), "."))
	if !ok {
		return nil
	}
	// A copy the probe can't measure is let through as a source it can't is:
	// a probe gap must not hold back every rendition.
	got, ok := metadata.ExactDuration(ctx, f, info.Size(), "m4a")
	if ok && got < want-renditionShortMax {
		return fmt.Errorf("%w: %.2f s of %.2f s", errShortCopy, got, want)
	}
	return nil
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
// skips) fail the whole encode; a read error that ends the encode early is
// caught by renditionCheck instead (a short copy).
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

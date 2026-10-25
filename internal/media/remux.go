package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-server/internal/diskspace"
)

// The remux cache's bounds (server#174): the total its files may hold (an LRU
// by mtime, which a hit bumps), the free space a remux must leave on the cache's
// disk, how many run at once (an I/O-bound stream copy, not an encoder: its own
// limit, apart from the transcode slots), how long one may take, and how many
// refused files are remembered. A hit bumps its copy's mtime at most once per
// remuxTouchEvery: a player's run of Range requests is one use, not one inode
// write each.
const (
	remuxCacheBytes = 8 << 30
	remuxMinFree    = 1 << 30
	remuxConcurrent = 2
	remuxTimeout    = 30 * time.Minute
	remuxFailedMax  = 1024
	remuxTouchEvery = 10 * time.Minute
)

var (
	// ErrNotRemuxable: ffmpeg refused the file (not one MP3 stream in Matroska:
	// the mp3 muxer takes exactly one MP3 audio stream). Remembered for this
	// file's size and mtime, so it is not tried again until the file changes.
	ErrNotRemuxable = errors.New("media: not remuxable to mp3")
	// ErrRemuxUnavailable: the remux was skipped (too little free disk for it,
	// the source changed while it waited, or its cached copy went before it
	// could be opened).
	ErrRemuxUnavailable = errors.New("media: remux unavailable")
)

// Remuxable reports whether a book of format (its file extension, no dot) and
// codec is MP3 in Matroska, which Remuxer copies losslessly into a plain MP3
// every client plays.
func Remuxable(format, codec string) bool {
	return strings.EqualFold(format, "mka") && strings.EqualFold(codec, "mp3")
}

// Remuxer turns a Matroska audio file holding MP3 into a cached plain MP3 (a
// lossless stream copy with the chapters as ID3 CHAP frames), so a client that
// can't open the container (iOS, every browser) plays it with Range seeks at
// the same stream URL. Its files live in dir, named by the source's path, size
// and mtime: a changed source is a new key, and the old copy ages out of the
// LRU. A remux is shared by every request for the key at once (single-flight)
// and runs under the server's context, never a request's.
type Remuxer struct {
	dir      string
	maxBytes int64
	log      *slog.Logger
	sem      chan struct{}
	// freeSpace reads the free space on dir's disk (diskspace.Of); run starts
	// ffmpeg. Fields so tests can fake the disk and count the runs.
	freeSpace func(path string) (uint64, bool)
	run       func(ctx context.Context, ffmpegPath string, args []string, stdin *os.File) error

	mu       sync.Mutex
	inflight map[string]*remuxFlight
	failed   map[string]struct{}
	// evictMu serializes the sweeps; lowDisk is set while the disk guard is
	// refusing, so it warns once per spell.
	evictMu sync.Mutex
	lowDisk atomic.Bool
}

// remuxFlight is one remux in progress: done closes once err is set.
type remuxFlight struct {
	done chan struct{}
	err  error
}

// NewRemuxer opens the cache in dir (created 0700), removes the .part files a
// stopped process left and trims it to its cap. log may be nil.
func NewRemuxer(dir string, log *slog.Logger) (*Remuxer, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Remuxer{
		dir: dir, maxBytes: remuxCacheBytes, log: log,
		sem:       make(chan struct{}, remuxConcurrent),
		freeSpace: func(path string) (uint64, bool) { _, free, ok := diskspace.Of(path); return free, ok },
		run:       runFFmpeg,
		inflight:  map[string]*remuxFlight{},
		failed:    map[string]struct{}{},
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".part") {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	m.evict("")
	return m, nil
}

// remuxKey names abs's cached copy: its path, size and mtime, hashed.
func remuxKey(abs string, info os.FileInfo) string {
	sum := sha256.Sum256([]byte(abs + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00" +
		strconv.FormatInt(info.ModTime().UnixNano(), 10)))
	return hex.EncodeToString(sum[:])[:32]
}

func (m *Remuxer) cachePath(key string) string { return filepath.Join(m.dir, key+".mp3") }

// Open returns the remuxed MP3 of the Matroska file abs (the caller has
// SafeJoined it) and the source's FileInfo, whose mtime is the response's
// Last-Modified (so validators outlive an eviction: the remux is
// deterministic). The caller closes the file. A cold key remuxes under runCtx
// (the server's lifetime, bounded by the remux timeout) while this call waits
// on ctx: a request that leaves doesn't stop the remux the others wait for.
// The errors are ErrNotRemuxable, ErrRemuxUnavailable, ctx's, or a failure to
// read abs or to run ffmpeg (serve the stored bytes for any of them).
func (m *Remuxer) Open(ctx, runCtx context.Context, ffmpegPath, abs string) (*os.File, os.FileInfo, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("media: remux source %s is not a regular file", abs)
	}
	key := remuxKey(abs, info)

	// The hit check runs under mu, so a leader (which renames its copy into
	// place before leaving inflight) is never missed between the two.
	m.mu.Lock()
	if f, ok := m.hit(key); ok {
		m.mu.Unlock()
		return f, info, nil
	}
	if _, refused := m.failed[key]; refused {
		m.mu.Unlock()
		return nil, nil, ErrNotRemuxable
	}
	fl, waiting := m.inflight[key]
	if !waiting {
		fl = &remuxFlight{done: make(chan struct{})}
		m.inflight[key] = fl
		go m.lead(runCtx, ffmpegPath, key, abs, info, fl)
	}
	m.mu.Unlock()

	select {
	case <-fl.done:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	if fl.err != nil {
		return nil, nil, fl.err
	}
	f, err := os.Open(m.cachePath(key))
	if err != nil {
		// Evicted between the rename and this open (another remux's sweep).
		return nil, nil, ErrRemuxUnavailable
	}
	return f, info, nil
}

// hit opens key's cached copy if it is there and marks it used (its mtime is
// the LRU's clock, bumped at most once per remuxTouchEvery).
func (m *Remuxer) hit(key string) (*os.File, bool) {
	path := m.cachePath(key)
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	if fi, err := f.Stat(); err == nil && time.Since(fi.ModTime()) > remuxTouchEvery {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
	}
	return f, true
}

// lead runs key's remux of abs (info is what the key was made from) and
// settles fl for every waiter.
func (m *Remuxer) lead(runCtx context.Context, ffmpegPath, key, abs string, info os.FileInfo, fl *remuxFlight) {
	err := m.remux(runCtx, ffmpegPath, key, abs, info)
	m.mu.Lock()
	if errors.Is(err, ErrNotRemuxable) {
		if len(m.failed) >= remuxFailedMax {
			for k := range m.failed { // drop one: the map stays bounded
				delete(m.failed, k)
				break
			}
		}
		m.failed[key] = struct{}{}
	}
	delete(m.inflight, key)
	fl.err = err
	m.mu.Unlock()
	close(fl.done)
}

// remux writes key's copy: a temporary .part file renamed into place once
// ffmpeg succeeds, then a sweep that keeps it.
func (m *Remuxer) remux(runCtx context.Context, ffmpegPath, key, abs string, info os.FileInfo) error {
	ctx, cancel := context.WithTimeout(runCtx, remuxTimeout)
	defer cancel()
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	size := uint64(info.Size())
	if free, ok := m.freeSpace(m.dir); ok && (free < size || free-size < remuxMinFree) {
		if !m.lowDisk.Swap(true) {
			m.log.Warn("remux skipped: too little free disk for the remux cache; serving Matroska as stored",
				"dir", m.dir, "free_bytes", free, "needed_bytes", size+remuxMinFree)
		}
		return ErrRemuxUnavailable
	}
	m.lowDisk.Store(false)

	// Opened only once a slot is free, so queued remuxes hold no descriptor;
	// a source that changed since the key was made is not cached under it.
	src, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer src.Close()
	if now, err := src.Stat(); err != nil || !now.Mode().IsRegular() || now.Size() != info.Size() || !now.ModTime().Equal(info.ModTime()) {
		return ErrRemuxUnavailable
	}

	tmp, err := os.CreateTemp(m.dir, key+".*.part")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpName) }() // gone after the rename; a failed run's leftover
	if err := m.run(ctx, ffmpegPath, remuxArgs(inputFor(ffmpegPath), tmpName), src); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Only ffmpeg's own refusal is the file's; one that didn't start is not.
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return err
		}
		m.log.Info("not remuxed: serving Matroska as stored", "path", src.Name(), "err", err)
		return fmt.Errorf("%w: %v", ErrNotRemuxable, err)
	}
	if err := os.Rename(tmpName, m.cachePath(key)); err != nil {
		return err
	}
	m.evict(key)
	return nil
}

// remuxArgs is the ffmpeg command line that copies the first audio stream of
// the Matroska on stdin (in) into an MP3 at out, chapters included. The demuxer
// is forced and the input may only be the descriptor, so crafted bytes can't
// make ffmpeg open other files; bitexact output is byte-identical every run,
// so an If-Range resume holds across a re-remux. ID3v2.3 carries the chapters
// as CHAP frames, and the muxer writes a Xing/Info header (a real duration for
// iOS).
func remuxArgs(in inputSpec, out string) []string {
	return []string{
		"-nostdin", "-loglevel", "error",
		"-protocol_whitelist", in.proto,
		"-f", "matroska", "-i", in.url,
		"-map", "0:a:0", "-c:a", "copy", "-map_chapters", "0",
		"-fflags", "+bitexact", "-id3v2_version", "3",
		"-f", "mp3", "-y", "file:" + out,
	}
}

// runFFmpeg runs ffmpegPath with args and stdin under ctx; the error carries
// its stderr.
func runFFmpeg(ctx context.Context, ffmpegPath string, args []string, stdin *os.File) error {
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stdin = stdin
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// evict trims the cache to its cap, least recently used (oldest mtime) first,
// never removing keep's copy (just written: a single file over the cap is still
// served, and goes on a later sweep). A removal that fails (a file open on
// Windows) is retried next sweep.
func (m *Remuxer) evict(keep string) {
	m.evictMu.Lock()
	defer m.evictMu.Unlock()
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	type cached struct {
		name string
		size int64
		used time.Time
	}
	var files []cached
	var total int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".mp3") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, cached{e.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if total <= m.maxBytes {
			return
		}
		if f.name == keep+".mp3" {
			continue
		}
		if err := os.Remove(filepath.Join(m.dir, f.name)); err == nil || errors.Is(err, fs.ErrNotExist) {
			total -= f.size
		}
	}
}

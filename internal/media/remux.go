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
	"testing"
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
	// ErrNotRemuxable: the file is not one to remux, so its stored bytes are the
	// answer. Either ffmpeg refused it (not one MP3 stream in Matroska: the mp3
	// muxer takes exactly one MP3 audio stream), which is remembered for this
	// file's size and mtime so it is not tried again until the file changes, or
	// there is no regular file at the path (gone, a folder).
	ErrNotRemuxable = errors.New("media: not remuxable to mp3")
	// ErrRemuxUnavailable: the remux was skipped (too little free disk for it,
	// the source changed while it waited or while ffmpeg read it, or its cached
	// copy went before it could be opened).
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
	// running counts the remuxes and background warms under way (Wait).
	running sync.WaitGroup
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

// FakeFreeSpaceForTests makes the disk guard read free bytes free on the
// cache's disk, for tests of what is served when it refuses. It panics outside
// a test binary (as auth.UseCheapHashingForTests does).
func (m *Remuxer) FakeFreeSpaceForTests(free uint64) {
	if !testing.Testing() {
		panic("media.FakeFreeSpaceForTests called outside a test binary")
	}
	m.freeSpace = func(string) (uint64, bool) { return free, true }
}

// HoldRemuxesForTests makes every remux wait for gate to close before ffmpeg
// runs, for tests of a request that finds its remux cold. It panics outside a
// test binary.
func (m *Remuxer) HoldRemuxesForTests(gate <-chan struct{}) {
	if !testing.Testing() {
		panic("media.HoldRemuxesForTests called outside a test binary")
	}
	run := m.run
	m.run = func(ctx context.Context, ffmpegPath string, args []string, stdin *os.File) error {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		return run(ctx, ffmpegPath, args, stdin)
	}
}

// Wait blocks until the remuxes and background warms under way have ended
// (tests, before their data folder goes).
func (m *Remuxer) Wait() { m.running.Wait() }

// remuxETag is the strong validator of key's copy: the key names the source's
// path, size and mtime, and the remux is bitexact, so the same key is always
// the same bytes, across a re-remux after an eviction too.
func remuxETag(key string) string { return `"remux-` + key + `"` }

// remuxKey names abs's cached copy: its path, size and mtime, hashed.
func remuxKey(abs string, info os.FileInfo) string {
	sum := sha256.Sum256([]byte(abs + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00" +
		strconv.FormatInt(info.ModTime().UnixNano(), 10)))
	return hex.EncodeToString(sum[:])[:32]
}

func (m *Remuxer) cachePath(key string) string { return filepath.Join(m.dir, key+".mp3") }

// Open returns the remuxed MP3 of the Matroska file abs (the caller has
// SafeJoined it) and its strong ETag (remuxETag); the copy's own mtime is its
// Last-Modified, never the source's, so a validator a client kept for the
// stored Matroska never matches it. The caller closes the file. A cold key
// remuxes under runCtx (the server's lifetime, bounded by the remux timeout)
// while this call waits on ctx: a request that leaves, or stops waiting,
// doesn't stop the remux the others wait for.
// The errors are ErrNotRemuxable (serve the stored bytes), ctx's, or one that
// says the remux can't be had now: ErrRemuxUnavailable, runCtx's, or a
// failure to read abs, run ffmpeg or write the cache.
func (m *Remuxer) Open(ctx, runCtx context.Context, ffmpegPath, abs string) (*os.File, string, error) {
	info, err := os.Stat(abs)
	switch {
	case err != nil:
		// Nothing to remux: the stored file's answer (a 404) is the right one.
		return nil, "", fmt.Errorf("%w: %v", ErrNotRemuxable, err)
	case !info.Mode().IsRegular():
		return nil, "", fmt.Errorf("%w: %s is not a regular file", ErrNotRemuxable, abs)
	}
	key := remuxKey(abs, info)

	// A hit needs no lock (players send many Range requests). A miss is checked
	// again under mu, so a leader that renamed its copy into place and left
	// inflight in between is never missed.
	if f, ok := m.hit(key); ok {
		return f, remuxETag(key), nil
	}
	m.mu.Lock()
	if f, ok := m.hit(key); ok {
		m.mu.Unlock()
		return f, remuxETag(key), nil
	}
	if _, refused := m.failed[key]; refused {
		m.mu.Unlock()
		return nil, "", ErrNotRemuxable
	}
	fl, waiting := m.inflight[key]
	if !waiting {
		fl = &remuxFlight{done: make(chan struct{})}
		m.inflight[key] = fl
		m.running.Add(1)
		go m.lead(runCtx, ffmpegPath, key, abs, info, fl)
	}
	m.mu.Unlock()

	select {
	case <-fl.done:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	if fl.err != nil {
		return nil, "", fl.err
	}
	f, err := os.Open(m.cachePath(key))
	if err != nil {
		// Evicted between the rename and this open (another remux's sweep).
		return nil, "", ErrRemuxUnavailable
	}
	return f, remuxETag(key), nil
}

// WarmAll remuxes the Matroska files paths in the background under runCtx, one
// after another (so a book's warm holds at most one of the remux slots), each
// skipped when its copy is cached, it is known not to remux, or it is being
// remuxed already (it is then waited for). It returns at once.
func (m *Remuxer) WarmAll(runCtx context.Context, ffmpegPath string, paths []string) {
	m.running.Go(func() {
		for _, abs := range paths {
			if f, _, err := m.Open(runCtx, runCtx, ffmpegPath, abs); err == nil {
				f.Close()
			}
			if runCtx.Err() != nil {
				return
			}
		}
	})
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
	defer m.running.Done()
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

// remux writes key's copy: a temporary .part file, synced and renamed into
// place once ffmpeg succeeds on a source that didn't change meanwhile, then a
// sweep that keeps it.
func (m *Remuxer) remux(runCtx context.Context, ffmpegPath, key, abs string, info os.FileInfo) error {
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-runCtx.Done():
		return runCtx.Err()
	}
	// The timeout bounds the run, not the wait for a slot.
	ctx, cancel := context.WithTimeout(runCtx, remuxTimeout)
	defer cancel()
	// Made again if it went (<data>/cache cleared while the server runs).
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	size := uint64(info.Size())
	if free, ok := m.freeSpace(m.dir); ok && (free < size || free-size < remuxMinFree) {
		if !m.lowDisk.Swap(true) {
			m.log.Warn("remux skipped: too little free disk for the remux cache; transcoding Matroska live",
				"dir", m.dir, "free_bytes", free, "needed_bytes", size+remuxMinFree)
		}
		return ErrRemuxUnavailable
	}
	m.lowDisk.Store(false)

	// Opened only once a slot is free, so queued remuxes hold no descriptor;
	// a source that changed since the key was made, or while ffmpeg read it,
	// is not cached under it.
	src, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer src.Close()
	if !unchanged(src, info) {
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
		if !refused(err, tmpName) {
			return err
		}
		m.log.Info("not remuxed: serving Matroska as stored", "path", src.Name(), "err", err)
		return fmt.Errorf("%w: %v", ErrNotRemuxable, err)
	}
	if !unchanged(src, info) {
		return ErrRemuxUnavailable
	}
	if err := syncFile(tmpName); err != nil {
		return err
	}
	if err := os.Rename(tmpName, m.cachePath(key)); err != nil {
		return err
	}
	m.evict(key)
	return nil
}

// unchanged reports whether the open source still has the size and mtime its
// key was made from.
func unchanged(src *os.File, info os.FileInfo) bool {
	now, err := src.Stat()
	return err == nil && now.Mode().IsRegular() && now.Size() == info.Size() && now.ModTime().Equal(info.ModTime())
}

// refused reports whether ffmpeg's failure err is its refusal of the file: an
// exit status before it wrote anything to out (the mp3 muxer checks the
// streams as it starts). A run that didn't start, was killed, or failed
// partway (a full disk, a read error) is not the file's, so it isn't
// remembered.
func refused(err error, out string) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() <= 0 {
		return false
	}
	written, serr := os.Stat(out)
	return serr == nil && written.Size() == 0
}

// syncFile flushes path's bytes to disk before the rename that names it, so a
// power cut can't leave the cache's name on a copy that never reached the
// disk (as jsonfile.Write does).
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

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

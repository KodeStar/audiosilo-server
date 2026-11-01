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

// The bounds every file cache shares: the free space a run must leave on the
// cache's disk, how many refused files are remembered, and how often a hit
// bumps its copy's mtime (a player's run of Range requests is one use, not one
// inode write each). The rest are each cache's (cacheSpec).
const (
	cacheMinFree   = 1 << 30
	cacheFailedMax = 1024
	// cacheRefusalTTL is how long a refusal is remembered (a changed file is a
	// new key at once): what looks like the file's fault may be the
	// environment's (an EIO as ffmpeg opened it, an ffmpeg missing an encoder,
	// a disk full at the first write), so it is tried again after it.
	cacheRefusalTTL = time.Hour
	cacheTouchEvery = 10 * time.Minute
	// cacheWarmQueue bounds the books whose warm (WarmAll) is under way or
	// queued: past it a warm is dropped (its first stream makes the copy as
	// before).
	cacheWarmQueue = 16
)

var (
	// ErrRefused: the file is not one this cache makes a copy of, so its stored
	// bytes are the answer (the remux) or the request fails (a rendition).
	// Either ffmpeg refused it (the remux's mp3 muxer takes exactly one MP3
	// audio stream; a rendition needs an audio stream ffmpeg decodes, and a
	// copy as long as its source), which is remembered for this file's size and
	// mtime for cacheRefusalTTL, or there is no regular file at the path
	// (gone, a folder).
	ErrRefused = errors.New("media: ffmpeg refused the file")
	// ErrUnavailable: the run was skipped (too little free disk for it, as many
	// runs in flight as the cache allows, the source changed while it waited or
	// while ffmpeg read it, or its cached copy went before it could be opened).
	ErrUnavailable = errors.New("media: cached copy unavailable")
	// ErrNoSource joins ErrRefused when there is no regular file at the path,
	// so a caller that answers that case itself (a rendition's 404) can tell it
	// from ffmpeg's refusal.
	ErrNoSource = errors.New("media: no regular file at the path")
	// ErrPending: Open stopped waiting (its ctx ended) while the run goes on
	// for the next request; it wraps ctx's error.
	ErrPending = errors.New("media: cached copy still being made")
)

// cacheSpec is what a FileCache makes: the ffmpeg command line that writes a
// copy, the copies' extension and validator, its bounds, and how its log lines
// read. The remux (NewRemuxer) and the watch rendition (NewRenditionCache) are
// the two.
type cacheSpec struct {
	// ext is the copies' file extension (".mp3"); only files with it are the
	// cache's (the LRU's sweep).
	ext string
	// tag joins the key (a rendition's kind and version), so a changed encode
	// is a new key; "" for the remux, whose keys predate it.
	tag string
	// maxBytes caps the copies' total (an LRU by mtime); concurrent is how many
	// runs at once; timeout bounds one run from when it has a slot.
	maxBytes   int64
	concurrent int
	timeout    time.Duration
	// queueMax bounds the runs in flight: with that many, neither Open nor
	// Queue starts another (0: Open is unbounded, and Queue starts nothing).
	queueMax int
	// args is the ffmpeg command line reading in and writing out.
	args func(in inputSpec, out string) []string
	// check, when set, looks at a copy ffmpeg wrote and exited 0 on (out, the
	// .part) beside its source (src, size bytes, unchanged since the key was
	// made): an error matching errCopyRefused (no audio, a short copy) is a
	// refusal, remembered as an exit status before anything was written is;
	// any other is a copy not to cache now (one that can't be read), tried
	// again next time.
	check func(ctx context.Context, src *os.File, size int64, out string) error
	// etag is the strong validator of key's copy of size bytes.
	etag func(key string, size int64) string
	// refused and lowDisk are the log lines of a refusal and of the disk guard.
	refused, lowDisk string
}

// FileCache turns a library file into a cached copy made by ffmpeg (a cacheSpec:
// the MP3-in-Matroska remux, a watch rendition), served with Range from the
// cache. Its files live in dir, named by the source's path, size and mtime (and
// the spec's tag): a changed source is a new key, and the old copy ages out of
// the LRU. A run is shared by every request for the key at once (single-flight)
// and runs under the server's context, never a request's.
type FileCache struct {
	spec cacheSpec
	dir  string
	log  *slog.Logger
	sem  chan struct{}
	// freeSpace reads the free space on dir's disk (diskspace.Of); run starts
	// ffmpeg. Fields so tests can fake the disk and count the runs.
	freeSpace func(path string) (uint64, bool)
	run       func(ctx context.Context, ffmpegPath string, args []string, stdin *os.File) error
	// now is the refusals' clock (a field so tests can move it).
	now func() time.Time

	mu       sync.Mutex
	inflight map[string]*cacheFlight
	// failed holds the remembered refusals, each until when (cacheRefusalTTL).
	failed map[string]time.Time
	// evictMu serializes the sweeps; lowDisk is set while the disk guard is
	// refusing, so it warns once per spell.
	evictMu sync.Mutex
	lowDisk atomic.Bool
	// reserved is the source bytes of the runs past the disk guard and still
	// writing, which the guard counts as taken: this cache's, and those of the
	// caches on its disk that share it (ShareDiskGuard).
	reserved *atomic.Int64
	// running counts the runs and background warms under way (Wait).
	running sync.WaitGroup
	// warming holds the warms under way or queued, by their first path (so a
	// book's /item and /chapters warm it once); warmSlot lets one run at a
	// time, so warms hold at most one of the slots and a stream the other.
	warmMu   sync.Mutex
	warming  map[string]struct{}
	warmSlot chan struct{}
}

// cacheFlight is one run in progress: done closes once err is set.
type cacheFlight struct {
	done chan struct{}
	err  error
}

// newFileCache opens spec's cache in dir (created 0700), removes the .part
// files a stopped process left and trims it to its cap. log may be nil.
func newFileCache(dir string, spec cacheSpec, log *slog.Logger) (*FileCache, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &FileCache{
		spec: spec, dir: dir, log: log,
		sem:       make(chan struct{}, spec.concurrent),
		freeSpace: func(path string) (uint64, bool) { _, free, ok := diskspace.Of(path); return free, ok },
		run:       runFFmpeg,
		inflight:  map[string]*cacheFlight{},
		now:       time.Now,
		failed:    map[string]time.Time{},
		reserved:  new(atomic.Int64),
		warming:   map[string]struct{}{},
		warmSlot:  make(chan struct{}, 1),
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
func (m *FileCache) FakeFreeSpaceForTests(free uint64) {
	if !testing.Testing() {
		panic("media.FakeFreeSpaceForTests called outside a test binary")
	}
	m.freeSpace = func(string) (uint64, bool) { return free, true }
}

// HoldRunsForTests makes every run wait for gate to close before ffmpeg
// starts, for tests of a request that finds its copy cold. It panics outside a
// test binary.
func (m *FileCache) HoldRunsForTests(gate <-chan struct{}) {
	if !testing.Testing() {
		panic("media.HoldRunsForTests called outside a test binary")
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

// ShareDiskGuard makes other's disk guard count m's runs still writing, and m's
// count other's (one reservation), for two caches on one disk (the remux and
// the rendition caches, both under <data>/cache), so a run in each can't pass
// on the same free space. Call it before either cache's first run.
func (m *FileCache) ShareDiskGuard(other *FileCache) { other.reserved = m.reserved }

// Wait blocks until the runs and background warms under way have ended (the
// launcher at shutdown, once their context is cancelled; tests, before their
// data folder goes).
func (m *FileCache) Wait() { m.running.Wait() }

// key names abs's cached copy: its path, size and mtime (and the spec's tag),
// hashed.
func (m *FileCache) key(abs string, info os.FileInfo) string {
	s := abs + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
	if m.spec.tag != "" {
		s += "\x00" + m.spec.tag
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}

func (m *FileCache) cachePath(key string) string { return filepath.Join(m.dir, key+m.spec.ext) }

// Ext is the copies' file extension (".mp3", ".m4a"), for a download's name.
func (m *FileCache) Ext() string { return m.spec.ext }

// source stats abs, the file a copy is made of: ErrRefused joined with
// ErrNoSource when there is no regular file there (the stored file's answer, a
// 404, is the right one).
func source(abs string) (os.FileInfo, error) {
	info, err := os.Stat(abs)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %w: %v", ErrRefused, ErrNoSource, err)
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("%w: %w: %s", ErrRefused, ErrNoSource, abs)
	}
	return info, nil
}

// Open returns the cached copy of abs (the caller has SafeJoined it) and its
// strong ETag (the spec's); the copy's own mtime is its Last-Modified, never
// the source's, so a validator a client kept for the stored file never matches
// it. The caller closes the file. A cold key runs under runCtx (the server's
// lifetime, bounded by the spec's timeout) while this call waits on ctx: a
// request that leaves, or stops waiting, doesn't stop the run the others wait
// for. The errors are ErrRefused (with ErrNoSource when abs is no regular
// file), ErrPending wrapping ctx's error when ctx ended first, or one that says
// the copy can't be had now: ErrUnavailable (also for a cold key while the
// spec's queueMax runs are in flight), runCtx's, or a failure to read abs, run
// ffmpeg or write the cache.
func (m *FileCache) Open(ctx, runCtx context.Context, ffmpegPath, abs string) (*os.File, string, error) {
	info, err := source(abs)
	if err != nil {
		return nil, "", err
	}
	key := m.key(abs, info)
	f, fl, err := m.begin(runCtx, ffmpegPath, key, abs, info, true)
	if f != nil || err != nil {
		return m.opened(key, f, err)
	}
	if fl == nil {
		return nil, "", ErrUnavailable
	}
	select {
	case <-fl.done:
	case <-ctx.Done():
		return nil, "", fmt.Errorf("%w: %w", ErrPending, ctx.Err())
	}
	if fl.err != nil {
		return nil, "", fl.err
	}
	f, err = os.Open(m.cachePath(key))
	if err != nil {
		// Evicted between the rename and this open (another run's sweep).
		return nil, "", ErrUnavailable
	}
	return m.opened(key, f, nil)
}

// opened is Open's answer for key's open copy f (or err): f with its ETag.
func (m *FileCache) opened(key string, f *os.File, err error) (*os.File, string, error) {
	if err != nil {
		return nil, "", err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, "", err
	}
	return f, m.spec.etag(key, info.Size()), nil
}

// begin is a request for key's copy of abs (info is what the key was made
// from): the copy when cached (marked used), else ErrRefused for a known
// refusal, else the flight making it, started under runCtx when there is none,
// start allows it and the spec's queueMax isn't reached (else a nil flight).
func (m *FileCache) begin(runCtx context.Context, ffmpegPath, key, abs string, info os.FileInfo, start bool) (*os.File, *cacheFlight, error) {
	// A hit needs no lock (players send many Range requests). A miss is checked
	// again under mu, so a leader that renamed its copy into place and left
	// inflight in between is never missed.
	if f, ok := m.hit(key); ok {
		return f, nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.hit(key); ok {
		return f, nil, nil
	}
	if m.refusedLocked(key) {
		return nil, nil, ErrRefused
	}
	fl, waiting := m.inflight[key]
	if !waiting && start && (m.spec.queueMax == 0 || len(m.inflight) < m.spec.queueMax) {
		fl = &cacheFlight{done: make(chan struct{})}
		m.inflight[key] = fl
		m.running.Add(1)
		go m.lead(runCtx, ffmpegPath, key, abs, info, fl)
	}
	return nil, fl, nil
}

// CacheState is where a file's copy stands (Status, Queue).
type CacheState string

const (
	// CacheReady: the copy is cached.
	CacheReady CacheState = "ready"
	// CachePending: the copy is being made or waits for a slot.
	CachePending CacheState = "pending"
	// CacheFailed: ffmpeg refused the file (remembered for cacheRefusalTTL or
	// until it changes), or there is no regular file at the path.
	CacheFailed CacheState = "failed"
	// CacheMissing: neither cached nor being made (never asked for, evicted,
	// or a run that failed for a reason not the file's).
	CacheMissing CacheState = "missing"
)

// Status is where abs's copy stands, and its size when ready, without starting
// a run or marking the copy used.
func (m *FileCache) Status(abs string) (CacheState, int64) {
	info, err := source(abs)
	if err != nil {
		return CacheFailed, 0
	}
	key := m.key(abs, info)
	// A key in flight or refused has no copy (a run renames its copy into
	// place before it leaves inflight), so the cache file is stat'ed once,
	// outside the lock.
	m.mu.Lock()
	refused := m.refusedLocked(key)
	_, running := m.inflight[key]
	m.mu.Unlock()
	switch {
	case refused:
		return CacheFailed, 0
	case running:
		return CachePending, 0
	}
	if fi, err := os.Stat(m.cachePath(key)); err == nil {
		return CacheReady, fi.Size()
	}
	return CacheMissing, 0
}

// Queue starts making abs's copy under runCtx unless it is cached, refused or
// under way, and returns where it stands then (as Status). It never waits for
// the run. A cache with no queueMax, or as many runs in flight, starts nothing:
// the file stays missing for the caller to ask again.
func (m *FileCache) Queue(runCtx context.Context, ffmpegPath, abs string) (CacheState, int64) {
	info, err := source(abs)
	if err != nil {
		return CacheFailed, 0
	}
	key := m.key(abs, info)
	f, fl, err := m.begin(runCtx, ffmpegPath, key, abs, info, m.spec.queueMax > 0)
	switch {
	case f != nil:
		defer f.Close()
		if fi, err := f.Stat(); err == nil {
			return CacheReady, fi.Size()
		}
		return CacheReady, 0
	case err != nil:
		return CacheFailed, 0
	case fl != nil:
		return CachePending, 0
	}
	return CacheMissing, 0
}

// WarmAll makes the copies of paths in the background under runCtx, one
// after another and one warm at a time (so warms hold at most one of the
// slots), each skipped when its copy is cached (not marked used: a book looked
// at is not one played), it is known to be refused, or it is under way already
// (it is then waited for). It returns at once, and does nothing when the same
// paths are being warmed already or cacheWarmQueue warms are.
func (m *FileCache) WarmAll(runCtx context.Context, ffmpegPath string, paths []string) {
	if len(paths) == 0 {
		return
	}
	id := paths[0]
	m.warmMu.Lock()
	if _, dup := m.warming[id]; dup || len(m.warming) >= cacheWarmQueue {
		m.warmMu.Unlock()
		return
	}
	m.warming[id] = struct{}{}
	m.warmMu.Unlock()
	m.running.Go(func() {
		defer func() {
			m.warmMu.Lock()
			delete(m.warming, id)
			m.warmMu.Unlock()
		}()
		select {
		case m.warmSlot <- struct{}{}:
			defer func() { <-m.warmSlot }()
		case <-runCtx.Done():
			return
		}
		for _, abs := range paths {
			if runCtx.Err() != nil {
				return
			}
			if st, _ := m.Status(abs); st == CacheReady {
				continue
			}
			if f, _, err := m.Open(runCtx, runCtx, ffmpegPath, abs); err == nil {
				f.Close()
			}
		}
	})
}

// hit opens key's cached copy if it is there and marks it used (its mtime is
// the LRU's clock, bumped at most once per cacheTouchEvery).
func (m *FileCache) hit(key string) (*os.File, bool) {
	path := m.cachePath(key)
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	if fi, err := f.Stat(); err == nil && time.Since(fi.ModTime()) > cacheTouchEvery {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
	}
	return f, true
}

// lead runs key's copy of abs (info is what the key was made from) and
// settles fl for every waiter.
func (m *FileCache) lead(runCtx context.Context, ffmpegPath, key, abs string, info os.FileInfo, fl *cacheFlight) {
	defer m.running.Done()
	err := m.make(runCtx, ffmpegPath, key, abs, info)
	m.mu.Lock()
	if errors.Is(err, ErrRefused) {
		m.rememberLocked(key)
	}
	delete(m.inflight, key)
	fl.err = err
	m.mu.Unlock()
	close(fl.done)
}

// refusedLocked reports whether key's refusal is remembered and not expired,
// forgetting an expired one. The caller holds mu.
func (m *FileCache) refusedLocked(key string) bool {
	until, ok := m.failed[key]
	if ok && !m.now().Before(until) {
		delete(m.failed, key)
		return false
	}
	return ok
}

// rememberLocked remembers key's refusal for cacheRefusalTTL, the map bounded
// by cacheFailedMax (expired entries go first, else an arbitrary one). The
// caller holds mu.
func (m *FileCache) rememberLocked(key string) {
	now := m.now()
	if len(m.failed) >= cacheFailedMax {
		for k, until := range m.failed {
			if !now.Before(until) {
				delete(m.failed, k)
			}
		}
		for k := range m.failed {
			if len(m.failed) < cacheFailedMax {
				break
			}
			delete(m.failed, k)
		}
	}
	m.failed[key] = now.Add(cacheRefusalTTL)
}

// make writes key's copy: a temporary .part file, synced and renamed into
// place once ffmpeg succeeds on a source that didn't change meanwhile, then a
// sweep that keeps it.
func (m *FileCache) make(runCtx context.Context, ffmpegPath, key, abs string, info os.FileInfo) error {
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-runCtx.Done():
		return runCtx.Err()
	}
	// The timeout bounds the run, not the wait for a slot.
	ctx, cancel := context.WithTimeout(runCtx, m.spec.timeout)
	defer cancel()
	// Made again if it went (<data>/cache cleared while the server runs).
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	// The guard counts the other runs still writing as taken (all of them, the
	// sharing caches' too: the part written already is counted twice, on the
	// safe side), so two at once can't each pass on the same free space. The
	// source's size stands for the copy's: a remux's is about it, a rendition's
	// smaller.
	size := info.Size()
	need := uint64(m.reserved.Add(size))
	defer m.reserved.Add(-size)
	if free, ok := m.freeSpace(m.dir); ok && (free < need || free-need < cacheMinFree) {
		if !m.lowDisk.Swap(true) {
			m.log.Warn(m.spec.lowDisk, "dir", m.dir, "free_bytes", free, "needed_bytes", need+cacheMinFree)
		}
		return ErrUnavailable
	}
	m.lowDisk.Store(false)

	// Opened only once a slot is free, so queued runs hold no descriptor; a
	// source that changed since the key was made, or while ffmpeg read it, is
	// not cached under it.
	src, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer src.Close()
	if !unchanged(src, info) {
		return ErrUnavailable
	}

	tmp, err := os.CreateTemp(m.dir, key+".*.part")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpName) }() // gone after the rename; a failed run's leftover
	if err := m.run(ctx, ffmpegPath, m.spec.args(inputFor(ffmpegPath), tmpName), src); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !refused(err, tmpName) {
			return err
		}
		return m.refuse(src.Name(), err)
	}
	if !unchanged(src, info) {
		return ErrUnavailable
	}
	if m.spec.check != nil {
		if err := m.spec.check(ctx, src, info.Size(), tmpName); errors.Is(err, errCopyRefused) {
			return m.refuse(src.Name(), err)
		} else if err != nil {
			return err
		}
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

// errCopyRefused is the spec's check refusing a copy ffmpeg wrote and exited 0
// on: remembered as ffmpeg's own refusal is.
var errCopyRefused = errors.New("ffmpeg exited 0 but the copy is refused")

// errNoAudio is a copy that holds no audio track.
var errNoAudio = fmt.Errorf("%w: it holds no audio", errCopyRefused)

// refuse logs ffmpeg's refusal (err) of the source at path and returns it as
// ErrRefused, which lead remembers.
func (m *FileCache) refuse(path string, err error) error {
	m.log.Info(m.spec.refused, "path", path, "err", err)
	return fmt.Errorf("%w: %v", ErrRefused, err)
}

// unchanged reports whether the open source still has the size and mtime its
// key was made from.
func unchanged(src *os.File, info os.FileInfo) bool {
	now, err := src.Stat()
	return err == nil && now.Mode().IsRegular() && now.Size() == info.Size() && now.ModTime().Equal(info.ModTime())
}

// refused reports whether ffmpeg's failure err is its refusal of the file: an
// exit status before it wrote anything to out (a muxer checks the streams as
// it starts; a source with no audio stream fails its -map). A run that didn't
// start, was killed, or failed partway (a full disk, a read error) is not the
// file's, so it isn't remembered. One that is may still be the environment's
// (an EIO at open, an ffmpeg without the encoder): the memory expires
// (cacheRefusalTTL).
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

// runFFmpeg runs ffmpegPath with args and stdin under ctx; the error carries
// the start and end of its stderr (stderrLog).
func runFFmpeg(ctx context.Context, ffmpegPath string, args []string, stdin *os.File) error {
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stdin = stdin
	var stderr stderrLog
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// stderrKeep is how much of a run's stderr a stderrLog keeps at each end.
const stderrKeep = 2 << 10

// stderrLog keeps the first and the last stderrKeep bytes written to it, so a
// failed run's error stays small: without -xerror ffmpeg logs a line for every
// damaged frame it skips (megabytes for a badly damaged MP3), and the error is
// logged by every request that waited on the run.
type stderrLog struct {
	head, tail []byte
	dropped    int
}

func (l *stderrLog) Write(p []byte) (int, error) {
	n := len(p)
	if room := stderrKeep - len(l.head); room > 0 {
		k := min(room, len(p))
		l.head = append(l.head, p[:k]...)
		p = p[k:]
	}
	l.tail = append(l.tail, p...)
	// Trimmed once it holds twice what it keeps, so each byte is copied at
	// most once on the way out.
	if over := len(l.tail) - stderrKeep; over >= stderrKeep {
		l.dropped += over
		l.tail = append(l.tail[:0], l.tail[over:]...)
	}
	return n, nil
}

func (l *stderrLog) String() string {
	tail, dropped := l.tail, l.dropped
	if over := len(tail) - stderrKeep; over > 0 {
		tail, dropped = tail[over:], dropped+over
	}
	if dropped == 0 {
		return string(l.head) + string(tail)
	}
	return string(l.head) + "\n[" + strconv.Itoa(dropped) + " bytes of ffmpeg's log left out]\n" + string(tail)
}

// evict trims the cache to its cap, least recently used (oldest mtime) first,
// never removing keep's copy (just written: a single file over the cap is still
// served, and goes on a later sweep). Only the spec's copies count: another
// cache's files beside them are not this one's. A removal that fails (a file
// open on Windows) is retried next sweep.
func (m *FileCache) evict(keep string) {
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
		if !strings.HasSuffix(e.Name(), m.spec.ext) {
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
		if total <= m.spec.maxBytes {
			return
		}
		if f.name == keep+m.spec.ext {
			continue
		}
		if err := os.Remove(filepath.Join(m.dir, f.name)); err == nil || errors.Is(err, fs.ErrNotExist) {
			total -= f.size
		}
	}
}

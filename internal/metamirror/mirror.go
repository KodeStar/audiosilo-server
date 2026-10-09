// Package metamirror keeps a local copy of the community metadata service's
// database for mirror mode (metadata.mode: mirror): the SQLite release artifact
// audiosilo-meta publishes on GitHub (meta.sqlite, the CC0 core and the CC BY-SA
// community layer in one file), downloaded once a day and served in-process by
// metaserve's own API handler (pkg/query), so the server answers its metadata
// questions without looking any book up over the internet.
//
// Mirror is the runner: a daily, durable schedule (the last check is recorded on
// disk, so a restart does not download again), a conditional release-list
// request, a disk guard, the verified download (pkg/release), an atomic swap to
// the new copy with a grace period for in-flight queries, and the status the
// admin console shows. Any failure keeps the copy it has. internal/meta puts it
// in front of the remote service (meta.Service.SetMirror), which still answers
// whatever the copy can't.
package metamirror

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/query"
	"github.com/kodestar/audiosilo-meta/pkg/release"

	"github.com/kodestar/audiosilo-server/internal/diskspace"
)

// DirName is the mirror's folder in the data directory. It holds only derived
// data (the copy and its state), so it is not in backups, and a server started
// in remote mode deletes it (Remove).
const DirName = "meta-mirror"

// Dir is the mirror's folder for a data directory.
func Dir(dataDir string) string { return filepath.Join(dataDir, DirName) }

// The schedule.
const (
	// checkInterval is how often the release list is asked, measured from the
	// last check recorded on disk. Data releases come several times a day; a
	// copy a day old is fresh enough for book details, and each new copy is a
	// ~450 MB download.
	checkInterval = 24 * time.Hour
	// retryInterval is how soon a failed check is tried again.
	retryInterval = time.Hour
	// startDelay is how long after start a mirror with no copy checks: soon, but
	// not in the middle of the start's own work (the library scan).
	startDelay = 30 * time.Second
	// disabledPoll is how often a due check looks again while metadata is off
	// (no lookups and no downloads then).
	disabledPoll = time.Minute
	// enabledPoll is how often a running check looks whether metadata was
	// turned off meanwhile (it then stops, its download with it).
	enabledPoll = time.Second
	// swapGrace is how long a replaced copy stays open for the queries that
	// started on it before it is closed and deleted.
	swapGrace = 60 * time.Second
)

// The disk guard: a download needs room for the decompressed copy (about 4.5
// times the gz asset today, or the current copy's size when that is larger: the
// catalogue only grows) beside the copy being served, plus headroom so the
// volume is never filled to the last byte.
const (
	expansionTimesTwo = 9 // 4.5x, in halves
	diskHeadroom      = 512 << 20
)

// The copy's states (Status.State). Opening covers the seconds query.Open's
// integrity checks take over a 1.8 GB copy: the one on disk at Run's start, or a
// finished download before it is swapped in. Lookups go where they went
// meanwhile: the remote service (fallback) without a copy answering, the current
// copy over an update.
const (
	StateEmpty       = "empty"       // no copy and no failed attempt yet
	StateDownloading = "downloading" // a download is running (over a copy or not)
	StateOpening     = "opening"     // a copy is being opened
	StateReady       = "ready"       // a usable copy answers
	StateError       = "error"       // no usable copy, and the last attempt failed
)

// Status is the copy's state for the admin console (GET /admin/system's
// metadata.mirror and POST /admin/meta/mirror/check).
type Status struct {
	State         string    `json:"state"`
	Tag           string    `json:"tag,omitempty"`
	BuiltAt       time.Time `json:"built_at,omitzero"`
	SchemaVersion int       `json:"schema_version,omitempty"`
	// SchemaNewer: the copy's artifact schema is newer than this server's code
	// knows (query.MaxSchemaVersion). It still answers; a question it can't is
	// asked of the remote service.
	SchemaNewer  bool      `json:"schema_newer,omitempty"`
	SizeBytes    int64     `json:"size_bytes,omitempty"`
	CheckedAt    time.Time `json:"checked_at,omitzero"`
	NextCheckAt  time.Time `json:"next_check_at,omitzero"`
	DownloadedAt time.Time `json:"downloaded_at,omitzero"`
	// Progress is the download's compressed bytes, only while downloading.
	Progress *Progress `json:"progress,omitempty"`
	// Error is the last failed check, kept while a working copy answers.
	Error string `json:"error,omitempty"`
	// Fallback: lookups are going to the remote service (no usable copy).
	Fallback bool `json:"fallback"`
}

// Progress is a running download's compressed bytes; Total is 0 when the
// release declares no size.
type Progress struct {
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

// Options configures a Mirror. Every field may be left zero.
type Options struct {
	// Enabled reports the live metadata.enabled: while it is false nothing is
	// checked or downloaded. Nil means always.
	Enabled func() bool
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// FreeBytes reports the space free on the volume holding path (the disk
	// guard); nil means the real one (internal/diskspace).
	FreeBytes func(path string) (uint64, error)
	// Logger receives the mirror's notices; nil means slog.Default().
	Logger *slog.Logger
	// SiteURL is the metadata site (metadata.base_url): the handler writes it
	// into the links its answers carry, so they match the remote service's.
	SiteURL string
	// Repo is the repository the data releases come from; "" means
	// release.DefaultRepo (tests: a fake's).
	Repo string
	// Release configures the release client (User-Agent, logger; tests: a fake
	// GitHub's address). New adds release.WithBaseSize itself: the download's
	// decompression bound follows the copy held.
	Release []release.Option
}

// Mirror keeps the local copy and serves metaserve's API over it.
type Mirror struct {
	dir       string
	client    *release.Client
	enabled   func() bool
	now       func() time.Time
	freeBytes func(string) (uint64, error)
	log       *slog.Logger
	qlog      *log.Logger // the query layer's notices, through log
	handler   http.Handler

	cur  atomic.Pointer[query.DB]
	wake chan struct{}
	// openDB opens a copy: query.Open (tests: one that waits, to watch the opening).
	openDB func(path, tag string) (*query.DB, error)

	// The schedule's lengths: the constants above, fields only so tests can
	// shrink them.
	interval, retry, startAfter, grace, enabledPoll time.Duration

	// done and total are the running download's progress (compressed bytes).
	done, total atomic.Int64

	// mu guards what follows. st is the one record of the copy held (its tag,
	// build, schema and size: Status reads them there) and of the checks.
	mu          sync.Mutex
	st          state
	first       time.Time // when the first check is due without a copy (start + startDelay)
	opened      bool      // Run has opened the copy the state names (open)
	checked     bool      // a check ran since start
	checking    bool
	due         bool // CheckNow asked for a check that hasn't started yet
	downloading bool
	openingNew  bool               // a downloaded copy is being opened, not swapped in yet
	installing  string             // the path an install is writing and opening ("" when none)
	retiring    map[*query.DB]bool // replaced copies inside their grace
	closed      bool
}

// New prepares the mirror in dir (created 0700 when missing) and reads its
// state. It is cheap: the copy the state names is opened by Run (query.Open's
// integrity checks read the whole 1.8 GB copy, seconds a server's start must not
// wait for), and until then lookups go to the remote service. It does not touch
// the network either; Run does.
func New(dir string, opts Options) (*Mirror, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Mirror{
		dir:     dir,
		enabled: opts.Enabled, now: opts.Now, freeBytes: opts.FreeBytes, log: opts.Logger,
		wake:     make(chan struct{}, 1),
		interval: checkInterval, retry: retryInterval, startAfter: startDelay, grace: swapGrace,
		enabledPoll: enabledPoll,
		retiring:    map[*query.DB]bool{},
		openDB:      query.Open,
	}
	if m.enabled == nil {
		m.enabled = func() bool { return true }
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.freeBytes == nil {
		m.freeBytes = freeBytes
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	m.qlog = slog.NewLogLogger(m.log.Handler(), slog.LevelWarn)
	m.handler = query.NewHandler(m.Current, query.HandlerOptions{Logger: m.qlog, SiteURL: opts.SiteURL})
	m.client = release.New(opts.Repo, "", append(slices.Clone(opts.Release), release.WithBaseSize(m.sizeBytes))...)

	m.st = m.loadState()
	m.first = m.now().Add(m.startAfter)
	return m, nil
}

// freeBytes is the real disk guard reading.
func freeBytes(path string) (uint64, error) {
	_, free, ok := diskspace.Of(path)
	if !ok {
		return 0, errors.New("the free space could not be read")
	}
	return free, nil
}

// validTag is the shape a release tag must have to name a file in the mirror's
// folder (meta-<tag>.sqlite). The tag is data from GitHub: one with a path
// separator or a dot-dot must never reach a path.
var validTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (m *Mirror) copyPath(tag string) string {
	return filepath.Join(m.dir, "meta-"+tag+".sqlite")
}

// open opens the copy the state names and deletes what a stopped process left
// in the folder. Run calls it first; it does its work once.
func (m *Mirror) open() {
	m.mu.Lock()
	done := m.opened
	m.mu.Unlock()
	if done {
		return
	}
	m.openCopy()
	m.sweep()
	m.mu.Lock()
	m.opened = true
	m.mu.Unlock()
}

// openCopy opens the copy the state names. A copy that is missing, of another
// size than the state recorded, or that query.Open refuses is forgotten (the
// state keeps its last check, so the schedule still holds, but no ETag: the next
// check must list the releases to download one again).
func (m *Mirror) openCopy() {
	m.mu.Lock()
	tag, size := m.st.Tag, m.st.SizeBytes
	m.mu.Unlock()
	if tag == "" {
		return
	}
	db, size, why, err := m.openFile(tag, size)

	m.mu.Lock()
	defer m.mu.Unlock()
	if db == nil {
		m.log.Warn("metadata mirror: the local copy can't be used; a new one will be downloaded",
			"tag", tag, "reason", why, "err", err)
		m.st.forgetCopy()
		m.saveState()
		return
	}
	// The state's facts about the copy come from the copy itself.
	info := db.Info()
	m.st.BuiltAt, m.st.SchemaVersion, m.st.SizeBytes = info.BuiltAt, info.SchemaVersion, size
	m.cur.Store(db)
}

// openFile opens the copy of tag, checked against the size the state recorded
// (0: not recorded). Without a usable one it says why.
func (m *Mirror) openFile(tag string, size int64) (db *query.DB, actual int64, why string, err error) {
	if !validTag.MatchString(tag) {
		return nil, 0, "bad tag", nil
	}
	path := m.copyPath(tag)
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return nil, 0, "missing", err
	case size > 0 && fi.Size() != size:
		return nil, 0, "size changed", nil
	}
	if db, err = m.openDB(path, tag); err != nil {
		return nil, 0, "does not open", err
	}
	db.SetLogger(m.qlog)
	return db, fi.Size(), "", nil
}

// sweep deletes what a stopped process left in the folder: download temp files
// (release.TempFile), a state file half written, and any copy that isn't the
// current one (a swap stopped between the new copy's rename and the old one's
// deletion, or a download whose open failed).
func (m *Mirror) sweep() {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		m.log.Warn("metadata mirror: read its folder", "err", err)
		return
	}
	keep := ""
	if db := m.cur.Load(); db != nil {
		keep = filepath.Base(db.Path())
	}
	for _, e := range entries {
		name := e.Name()
		stale := release.TempFile(name) || name == stateTemp ||
			(strings.HasPrefix(name, "meta-") && strings.HasSuffix(name, ".sqlite") && name != keep)
		if !stale || e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(m.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			m.log.Warn("metadata mirror: delete a leftover file", "file", name, "err", err)
		}
	}
}

// Current is the copy answering now, nil until a usable one exists. The query
// handler loads it once per request, so a swap never splits one answer.
func (m *Mirror) Current() *query.DB { return m.cur.Load() }

// Ready reports whether a usable copy is loaded (meta.Mirror).
func (m *Mirror) Ready() bool { return m.cur.Load() != nil }

// Handler is metaserve's API over the copy (meta.Mirror): it answers 503 while
// there is none, and internal/meta's transport sends a 5xx to the remote
// service.
func (m *Mirror) Handler() http.Handler { return m.handler }

// sizeBytes is the current copy's size, 0 without one: the download's
// decompression bound never falls below twice it (release.WithBaseSize), so the
// bound follows a growing catalogue.
func (m *Mirror) sizeBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.SizeBytes
}

// Run opens the copy the state names, then checks for a new copy on the
// schedule until ctx ends: shortly after start when there is no copy, then once
// a day from the last check recorded on disk, an hour after a failed one, and
// whenever CheckNow asks. Nothing is checked while metadata is off; Wake (metadata
// turned back on) makes it look again at once, but only a check that is due runs.
func (m *Mirror) Run(ctx context.Context) {
	m.open()
	for {
		wait := m.untilDue()
		if !m.enabled() {
			wait = max(wait, disabledPoll)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-m.wake:
			timer.Stop()
		}
		// A wake or the disabled poll can come before the check is due: it
		// re-reads the schedule (CheckNow made it due now).
		if !m.enabled() || m.untilDue() > 0 {
			continue
		}
		m.checkWhileEnabled(ctx)
	}
}

// checkWhileEnabled runs one check that stops when metadata is turned off while
// it runs: a download in flight ends with it, and (like a server stopping) the
// check records nothing, so it is due again once metadata is back on.
func (m *Mirror) checkWhileEnabled(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(m.enabledPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !m.enabled() {
					m.log.Info("metadata mirror: metadata was turned off; the check in flight stops")
					cancel()
					return
				}
			}
		}
	}()
	m.check(ctx)
}

// CheckNow wakes Run for a check now ("Check now" in the console): from here
// the status says the next check is due now. It never blocks; while a check is
// running it does nothing (that check is the answer).
func (m *Mirror) CheckNow() {
	m.mu.Lock()
	busy := m.checking
	m.due = m.due || !busy
	m.mu.Unlock()
	if busy {
		return
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Wake makes Run look at the schedule again now (metadata turned back on: its
// disabled poll would otherwise wait up to a minute). Unlike CheckNow it makes
// nothing due: a check runs only if one already is. It never blocks.
func (m *Mirror) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// untilDue is how long until the next check is due (0 when it is).
func (m *Mirror) untilDue() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return max(m.nextLocked().Sub(m.now()), 0)
}

// nextLocked is when the next check is due: now when CheckNow asked; without a
// copy, shortly after start for the first one; else a day after the last check,
// or an hour after it when it failed.
func (m *Mirror) nextLocked() time.Time {
	if m.due {
		return m.now()
	}
	last := m.st.CheckedAt
	if (!m.checked && m.st.Tag == "") || last.IsZero() {
		return m.first
	}
	// A clock set back must not push the schedule out by the difference.
	if now := m.now(); last.After(now) {
		last = now
	}
	if m.st.LastError != "" {
		return last.Add(m.retry)
	}
	return last.Add(m.interval)
}

// check runs one check and records its outcome on disk. A check that ends
// because ctx did (the server stopping) records nothing.
func (m *Mirror) check(ctx context.Context) {
	m.mu.Lock()
	m.checking, m.checked, m.due = true, true, false
	etag := ""
	// Only a server holding a copy may ask "changed since?": one without has to
	// see the list to download anything, whatever it saw last time.
	if m.cur.Load() != nil {
		etag = m.st.ETag
	}
	m.mu.Unlock()

	err := m.update(ctx, etag)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.checking, m.downloading, m.openingNew = false, false, false
	if ctx.Err() != nil {
		return
	}
	m.st.CheckedAt = m.now()
	m.st.LastError = ""
	if err != nil {
		m.st.LastError = err.Error()
		if m.cur.Load() != nil {
			m.log.Warn("metadata mirror: check failed; the current copy stays in use", "err", err)
		} else {
			m.log.Warn("metadata mirror: check failed; lookups go to the online service until a copy is downloaded", "err", err)
		}
	}
	m.saveState()
}

// update asks for the newest data release and, when it is not the copy held,
// installs it.
func (m *Mirror) update(ctx context.Context, etag string) error {
	rel, newETag, notModified, err := m.client.LatestData(ctx, etag)
	// Stored even alongside an error: a list with no data release is a cheap 304
	// next time, until it changes.
	m.setETag(newETag)
	switch {
	case err != nil:
		return fmt.Errorf("couldn't list the metadata releases: %w", err)
	case notModified:
		return nil
	}
	m.mu.Lock()
	held := m.st.Tag == rel.Tag
	m.mu.Unlock()
	if held {
		return nil
	}
	if err := m.install(ctx, rel); err != nil {
		// A failure past the list forgets the ETag: the next check has to see
		// the list again, or it would answer 304 and never retry this release.
		m.setETag("")
		return err
	}
	return nil
}

// install downloads rel's copy, opens it and swaps it in, after checking its
// tag can name a file and the disk has room for it.
func (m *Mirror) install(ctx context.Context, rel *release.Release) error {
	if !validTag.MatchString(rel.Tag) {
		return fmt.Errorf("the release tag %q can't name a file", rel.Tag)
	}
	asset, _ := rel.Asset(release.DataAsset) // LatestData only returns releases carrying it
	if err := m.guardDisk(asset.Size); err != nil {
		return err
	}

	path := m.copyPath(rel.Tag)
	// A release swapped back in within the grace of the copy it once was (A
	// replaced by B, then A again) downloads to that copy's name: the retiring
	// handle is closed now, never deleted (its file is about to be this
	// download), and no retire deletes the path while it is installing.
	m.mu.Lock()
	m.downloading, m.installing = true, path
	var reused []*query.DB
	for db := range m.retiring {
		if db.Path() == path {
			delete(m.retiring, db)
			reused = append(reused, db)
		}
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.installing = ""
		m.mu.Unlock()
	}()
	for _, db := range reused {
		if err := db.Close(); err != nil {
			m.log.Warn("metadata mirror: close a replaced copy", "err", err)
		}
	}
	m.done.Store(0)
	m.total.Store(asset.Size)
	m.log.Info("metadata mirror: downloading a new copy", "tag", rel.Tag, "bytes", asset.Size)
	res, err := m.client.DownloadData(ctx, rel, path, func(done, total int64) {
		m.done.Store(done)
		m.total.Store(total)
	})
	if err != nil {
		return fmt.Errorf("couldn't download %s: %w", rel.Tag, err)
	}
	// The download is done; opening it takes seconds more, which the status
	// says (not a bar stuck at 100%).
	m.mu.Lock()
	m.downloading, m.openingNew = false, true
	m.mu.Unlock()
	db, err := m.openDB(path, rel.Tag)
	if err != nil {
		if rmErr := os.Remove(path); rmErr != nil {
			m.log.Warn("metadata mirror: delete a copy that doesn't open", "err", rmErr)
		}
		return fmt.Errorf("the downloaded copy %s doesn't open: %w", rel.Tag, err)
	}
	db.SetLogger(m.qlog)
	m.swap(db, rel, res)
	return nil
}

// guardDisk refuses a download the mirror's volume has no room for.
func (m *Mirror) guardDisk(declared int64) error {
	need := max(declared/2*expansionTimesTwo, m.sizeBytes()) + diskHeadroom
	free, err := m.freeBytes(m.dir)
	if err != nil {
		// Not knowing is not a reason to refuse: the download's own bound and a
		// failed write still keep the current copy.
		m.log.Warn("metadata mirror: couldn't read the free disk space; downloading anyway", "err", err)
		return nil
	}
	if free < uint64(need) {
		return fmt.Errorf("not enough disk space: need %s, have %s", formatBytes(uint64(need)), formatBytes(free))
	}
	return nil
}

// swap makes db the copy that answers, records it, and retires the one it
// replaces: closed and deleted after the grace, so the queries that started on
// it finish.
func (m *Mirror) swap(db *query.DB, rel *release.Release, res release.Result) {
	info := db.Info()
	m.mu.Lock()
	old := m.cur.Swap(db)
	m.openingNew = false
	m.st.Tag = rel.Tag
	m.st.PublishedAt = rel.PublishedAt
	m.st.BuiltAt = info.BuiltAt
	m.st.SchemaVersion = info.SchemaVersion
	m.st.SHA256 = res.SHA256
	m.st.SizeBytes = res.Bytes
	m.st.DownloadedAt = m.now()
	m.saveState()
	if old != nil {
		m.retiring[old] = true
		time.AfterFunc(m.grace, func() { m.retire(old) })
	}
	m.mu.Unlock()
	if info.SchemaVersion > query.MaxSchemaVersion {
		m.log.Warn("metadata mirror: the new copy is newer than this server understands; update the server",
			"schema_version", info.SchemaVersion, "understands", query.MaxSchemaVersion)
	}
	m.log.Info("metadata mirror: answering from the new copy", "tag", rel.Tag, "bytes", res.Bytes)
}

// retire closes and deletes a replaced copy, unless Close already did.
func (m *Mirror) retire(db *query.DB) {
	m.mu.Lock()
	ok := m.retiring[db]
	delete(m.retiring, db)
	m.mu.Unlock()
	if ok {
		m.closeAndDelete(db)
	}
}

// closeAndDelete closes a replaced copy and deletes its file, unless that file
// is in use again: the current copy's, or the one an install is writing and
// opening. A release swapped back in (the newest one withdrawn) is downloaded
// to the same name, and deleting it would leave the copy without a file. The
// check and the deletion hold mu, so an install can't start between them.
func (m *Mirror) closeAndDelete(db *query.DB) {
	if err := db.Close(); err != nil {
		m.log.Warn("metadata mirror: close a replaced copy", "err", err)
	}
	path := db.Path()
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := m.cur.Load(); path == m.installing || (cur != nil && cur.Path() == path) {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Warn("metadata mirror: delete a replaced copy", "err", err)
	}
}

func (m *Mirror) setETag(etag string) {
	m.mu.Lock()
	m.st.ETag = etag
	m.mu.Unlock()
}

// Status reports the copy for the console. The copy's facts are the state's
// (openCopy and swap keep them), shown while that copy answers.
func (m *Mirror) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	ready := m.cur.Load() != nil
	s := Status{
		CheckedAt: m.st.CheckedAt,
		Error:     m.st.LastError,
		Fallback:  !ready,
	}
	// A running check has no next one yet: it is set when this one ends.
	if !m.checking {
		s.NextCheckAt = m.nextLocked()
	}
	switch {
	case m.downloading:
		s.State = StateDownloading
		s.Progress = &Progress{Done: m.done.Load(), Total: m.total.Load()}
	case m.openingNew || (!m.opened && !ready && m.st.Tag != ""):
		// A new download being opened, or the copy the state names at Run's
		// start until it answers (not through open's sweep after it does): no
		// failure, and (at start) no copy answering yet either.
		s.State = StateOpening
	case ready:
		s.State = StateReady
	case m.st.LastError != "":
		s.State = StateError
	default:
		s.State = StateEmpty
	}
	if ready {
		s.Tag = m.st.Tag
		s.BuiltAt = m.st.BuiltAt
		s.SchemaVersion = m.st.SchemaVersion
		s.SchemaNewer = m.st.SchemaVersion > query.MaxSchemaVersion
		s.SizeBytes = m.st.SizeBytes
		s.DownloadedAt = m.st.DownloadedAt
	}
	return s
}

// Close closes the copy and any replaced one still in its grace (deleting
// those). Call it once Run has returned; the mirror answers nothing after.
func (m *Mirror) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	retiring := m.retiring
	m.retiring = map[*query.DB]bool{}
	m.mu.Unlock()
	// Before the current copy is let go: its file is kept when a retiring one
	// shares it.
	for db := range retiring {
		m.closeAndDelete(db)
	}
	return m.cur.Swap(nil).Close()
}

// Remove deletes a data directory's mirror folder, for a server started in
// remote mode: the copy is derived data, and nothing reads it there. It reports
// whether there was one.
func Remove(dataDir string) (bool, error) {
	dir := Dir(dataDir)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return true, os.RemoveAll(dir)
}

// formatBytes writes a size in decimal units, as the console does (1 kB =
// 1000 B; one decimal below 10).
func formatBytes(n uint64) string {
	units := [...]string{"B", "kB", "MB", "GB", "TB"}
	v, i := float64(n), 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	digits := 0
	if v < 10 && i > 0 {
		digits = 1
	}
	return strings.TrimSuffix(strconv.FormatFloat(v, 'f', digits, 64), ".0") + " " + units[i]
}

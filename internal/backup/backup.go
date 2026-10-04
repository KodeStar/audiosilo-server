// Package backup makes, keeps and restores copies of the server's database.
//
// A backup is the whole SQLite database written with VACUUM INTO into the backups
// folder (<data>/backups unless config.yaml's backups.dir says otherwise), as one
// self-contained file: audiosilo-<UTC time>-<kind>.db. The library index in it can
// be rebuilt from the files on disk; what it is for is everything else: accounts,
// progress, bookmarks, notes, metadata edits, shares, invites, devices, history and
// the console's own records. config.yaml is not in it.
//
// Backups run on a schedule (Settings > Backups) and when an admin asks. The
// schedule keeps the newest `keep` scheduled backups; manual backups and the copies
// made before a restore stay until an admin deletes them.
//
// A restore never swaps the database under a running server: RequestRestore checks
// the file and leaves a marker (restore.json in the data folder), and the next start
// applies it (ApplyPendingRestore) before the database opens, keeping a copy of the
// database it replaces in the backups folder. The outcome is recorded in
// restore-result.json for the console.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// Kinds of backup, the last part of a backup's file name.
const (
	KindScheduled     = "scheduled"
	KindManual        = "manual"
	KindBeforeRestore = "before-restore"
)

// Errors.
var (
	// ErrNotFound: no backup has that name.
	ErrNotFound = errors.New("no such backup")
	// ErrBusy: a backup is already being made.
	ErrBusy = errors.New("a backup is already being made")
)

// dirName is the backups folder's name inside the data folder (the default).
const dirName = "backups"

// Files in the data folder that carry a restore across a restart.
const (
	markerFile = "restore.json"
	resultFile = "restore-result.json"
)

// timeLayout is the UTC time in a backup's name.
const timeLayout = "20060102-150405Z"

// nameRE is what a backup's file name may be: the prefix, anything plain, .db. Only
// such files are listed, downloaded, deleted or restored, so a name from a request
// can never leave the folder or touch another file. Backups this server makes are
// audiosilo-<time>-<kind>.db; a backup copied in from elsewhere keeps its name.
var nameRE = regexp.MustCompile(`^audiosilo-[A-Za-z0-9][A-Za-z0-9._-]{0,100}\.db$`)

// madeRE splits a name this server made into its time and kind.
var madeRE = regexp.MustCompile(`^audiosilo-(\d{8}-\d{6}Z)-(scheduled|manual|before-restore)\.db$`)

// validName reports whether name can be a backup's file name.
func validName(name string) bool { return nameRE.MatchString(name) && !strings.Contains(name, "..") }

// Backup is one backup file.
type Backup struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	// Kind is KindScheduled, KindManual or KindBeforeRestore; a file copied in from
	// elsewhere under another name reads as manual (its time is the file's).
	Kind string `json:"kind"`
}

// Result is how the newest backup attempt went.
type Result struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Trigger string    `json:"trigger"` // KindScheduled or KindManual
	Name    string    `json:"name,omitempty"`
	// Error is a short, plain reason ("" when OK); details are in the log.
	Error string `json:"error,omitempty"`
}

// Status is what the console shows about backups now.
type Status struct {
	Dir     string `json:"dir"`
	Running bool   `json:"running"`
	// Last is the newest attempt since the server started (nil before one);
	// Latest is the newest backup in the folder, whenever it was made.
	Last   *Result    `json:"last"`
	Latest *Backup    `json:"latest"`
	Next   *time.Time `json:"next"` // the next scheduled backup (nil when off)
}

// Service makes and keeps backups. Safe for concurrent use.
type Service struct {
	db      *store.DB
	dataDir string
	dir     string
	log     *slog.Logger
	now     func() time.Time
	loc     *time.Location
	wake    chan struct{}
	readDir func(string) ([]os.DirEntry, error) // os.ReadDir; a test counts the listings

	// OnFailure, when set, hears about every backup that failed (notifications).
	OnFailure func(r Result)

	mu       sync.Mutex
	schedule Schedule
	keep     int
	running  bool
	last     *Result
	// tried is when a scheduled backup was last attempted, so one that fails
	// waits for its next slot instead of retrying every minute.
	tried  time.Time
	anchor time.Time // when Run started: what a server with no scheduled backup counts from
	// newest is the newest scheduled backup in the folder (zero: none) as of the
	// latest listing (List), what the schedule counts from once newestKnown. The
	// scheduler asks every minute and the folder may be on a NAS, so a tick never
	// lists it: the first one does, then every listing made anyway (the backups
	// page, the status, pruning) keeps it current. Backups this process makes
	// count through tried in between.
	newest      time.Time
	newestKnown bool
}

// New returns the service for the database db, kept in dir (dirName in dataDir when
// empty).
func New(db *store.DB, dataDir, dir string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		db: db, dataDir: dataDir, dir: backupDir(dataDir, dir), log: log,
		now: time.Now, loc: time.Local, wake: make(chan struct{}, 1), keep: 7, readDir: os.ReadDir,
	}
}

// backupDir is the backups folder for a data folder and the configured backups.dir.
func backupDir(dataDir, configured string) string {
	if configured != "" {
		return configured
	}
	return filepath.Join(dataDir, dirName)
}

// SetSettings applies the schedule and how many scheduled backups to keep (both
// already checked by config).
func (s *Service) SetSettings(schedule string, keep int) {
	sch, err := ParseSchedule(schedule)
	if err != nil {
		s.log.Warn("backup schedule not understood; scheduled backups are off", "schedule", schedule)
	}
	s.mu.Lock()
	s.schedule, s.keep = sch, max(keep, 1)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run makes scheduled backups until ctx ends, checking once a minute. Leftovers of
// a backup a stopped server was making are removed first.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.anchor = s.now()
	s.mu.Unlock()
	s.removeTemp()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
		if s.due() {
			_, _ = s.Create(ctx, KindScheduled)
		}
	}
}

// Status reports the folder, whether a backup is being made, the newest attempt and
// backup, and the next scheduled one.
func (s *Service) Status() Status {
	list, _ := s.List()
	return s.StatusOf(list)
}

// StatusOf is Status given the folder's backups (List), for a caller that has
// listed them already.
func (s *Service) StatusOf(list []Backup) Status {
	next := s.next()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Dir: s.dir, Running: s.running}
	for _, b := range list { // newest first
		if b.Kind != KindBeforeRestore {
			st.Latest = &b
			break
		}
	}
	if s.last != nil {
		l := *s.last
		st.Last = &l
	}
	if !next.IsZero() {
		st.Next = &next
	}
	return st
}

// next is when the next scheduled backup is due (zero when off): its slot after
// the newest scheduled backup (after Run started when there is none), or after
// the last attempt when that is newer. Counting from the newest backup, not from
// the start, is what makes a server that was off at its time catch up.
func (s *Service) next() time.Time {
	s.mu.Lock()
	sch, anchor, tried := s.schedule, s.anchor, s.tried
	s.mu.Unlock()
	if sch.Off() {
		return time.Time{}
	}
	from := s.newestScheduled()
	if from.IsZero() {
		from = anchor
	}
	if tried.After(from) {
		from = tried
	}
	if from.IsZero() {
		from = s.now()
	}
	return sch.Next(from, s.loc)
}

func (s *Service) due() bool {
	n := s.next()
	return !n.IsZero() && !n.After(s.now())
}

// newestScheduled is the newest scheduled backup's time (zero when there is
// none) as of the latest listing, listing the folder only when nothing has yet.
// An unreadable folder counts as empty and is listed again next time.
func (s *Service) newestScheduled() time.Time {
	s.mu.Lock()
	known, newest := s.newestKnown, s.newest
	s.mu.Unlock()
	if known {
		return newest
	}
	if _, err := s.List(); err != nil {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newest
}

// Start makes a backup in the background; ErrBusy when one is being made already.
// ctx bounds it: the server's lifetime, never a request's (the request ends first).
// It reads as running from the moment Start returns, so a status read right after
// (the request's answer) already shows it.
func (s *Service) Start(ctx context.Context, trigger string) error {
	if !s.begin(trigger) {
		return ErrBusy
	}
	go func() { _, _ = s.make(ctx, trigger) }()
	return nil
}

// Create makes a backup now (trigger is KindScheduled or KindManual) and, after a
// scheduled one, removes scheduled backups past the newest `keep`.
func (s *Service) Create(ctx context.Context, trigger string) (Backup, error) {
	if !s.begin(trigger) {
		return Backup{}, ErrBusy
	}
	return s.make(ctx, trigger)
}

// begin marks a backup as being made; false when one is already.
func (s *Service) begin(trigger string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	if trigger == KindScheduled {
		s.tried = s.now()
	}
	return true
}

// make writes the backup begin marked, records the outcome and clears running.
func (s *Service) make(ctx context.Context, trigger string) (Backup, error) {
	b, err := s.write(ctx, trigger, s.db.VacuumInto)
	res := Result{At: s.now(), OK: err == nil, Trigger: trigger, Name: b.Name}
	if err != nil {
		res.Error = reason(err)
		s.log.Warn("backup failed", "trigger", trigger, "err", err)
	} else {
		s.log.Info("backup made", "name", b.Name, "bytes", b.Size)
		if trigger == KindScheduled {
			s.prune()
		}
	}
	s.mu.Lock()
	s.running = false
	s.last = &res
	s.mu.Unlock()
	// A backup stopped because the server is stopping didn't fail: don't announce it.
	if err != nil && ctx.Err() == nil && s.OnFailure != nil {
		s.OnFailure(res)
	}
	return b, err
}

// write makes one backup file of the given kind with copyTo: into a hidden
// temporary name first, readable by the server's user only (it holds password and
// token hashes), then renamed into place, so a half-written file is never listed.
func (s *Service) write(ctx context.Context, kind string, copyTo func(context.Context, string) error) (Backup, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Backup{}, err
	}
	at := s.now().UTC().Truncate(time.Second)
	name := fmt.Sprintf("audiosilo-%s-%s.db", at.Format(timeLayout), kind)
	final := filepath.Join(s.dir, name)
	if _, err := os.Stat(final); err == nil {
		return Backup{}, fmt.Errorf("%s already exists", name) // two in one second
	}
	tmp := filepath.Join(s.dir, "."+name+".tmp")
	_ = os.Remove(tmp)
	if err := copyTo(ctx, tmp); err != nil {
		_ = os.Remove(tmp)
		return Backup{}, err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return Backup{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return Backup{}, err
	}
	fi, err := os.Stat(final)
	if err != nil {
		return Backup{}, err
	}
	return Backup{Name: name, Size: fi.Size(), CreatedAt: at, Kind: kind}, nil
}

// reason is a backup failure in a few plain words for the console.
func reason(err error) string {
	switch {
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case strings.Contains(err.Error(), "disk is full") || strings.Contains(err.Error(), "no space left"):
		return "disk_full"
	}
	return "failed"
}

// removeTemp deletes temporary files a stopped server left behind.
func (s *Service) removeTemp() {
	entries, err := s.readDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, ".audiosilo-") && strings.HasSuffix(n, ".tmp") {
			_ = os.Remove(filepath.Join(s.dir, n))
		}
	}
}

// prune removes scheduled backups past the newest `keep`, never the one a restore
// is waiting for (the next start would find it gone).
func (s *Service) prune() {
	s.mu.Lock()
	keep := s.keep
	s.mu.Unlock()
	list, err := s.List()
	if err != nil {
		return
	}
	pending, err := s.PendingRestore()
	if err != nil {
		s.log.Warn("reading the waiting restore failed; old backups are kept", "err", err)
		return
	}
	n := 0
	for _, b := range list { // newest first
		if b.Kind != KindScheduled {
			continue
		}
		if n++; n > keep && (pending == nil || pending.Name != b.Name) {
			if err := os.Remove(filepath.Join(s.dir, b.Name)); err != nil {
				s.log.Warn("removing an old backup failed", "name", b.Name, "err", err)
			}
		}
	}
}

// List returns the backups in the folder, newest first (none when the folder
// doesn't exist yet), and keeps the scheduler's newestScheduled current.
func (s *Service) List() ([]Backup, error) {
	entries, err := s.readDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		s.noteNewest(nil)
		return []Backup{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Backup{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !validName(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, describe(e.Name(), fi))
	}
	slices.SortFunc(out, func(a, b Backup) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.Name, a.Name)
	})
	s.noteNewest(out)
	return out, nil
}

// noteNewest records the newest scheduled backup in list (newest first).
func (s *Service) noteNewest(list []Backup) {
	var newest time.Time
	for _, b := range list {
		if b.Kind == KindScheduled {
			newest = b.CreatedAt
			break
		}
	}
	s.mu.Lock()
	s.newest, s.newestKnown = newest, true
	s.mu.Unlock()
}

// describe reads a backup's time and kind from its name, or its file for a name
// this server didn't make.
func describe(name string, fi os.FileInfo) Backup {
	b := Backup{Name: name, Size: fi.Size(), CreatedAt: fi.ModTime().UTC().Truncate(time.Second), Kind: KindManual}
	if m := madeRE.FindStringSubmatch(name); m != nil {
		if at, err := time.Parse(timeLayout, m[1]); err == nil {
			b.CreatedAt, b.Kind = at, m[2]
		}
	}
	return b
}

// path is a backup's file, after checking the name; ErrNotFound when there is none.
func (s *Service) path(name string) (string, os.FileInfo, error) {
	if !validName(name) {
		return "", nil, ErrNotFound
	}
	p := filepath.Join(s.dir, name)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", nil, ErrNotFound
	}
	return p, fi, nil
}

// Open opens a backup for download; the caller closes it.
func (s *Service) Open(name string) (*os.File, Backup, error) {
	p, fi, err := s.path(name)
	if err != nil {
		return nil, Backup{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, Backup{}, err
	}
	return f, describe(name, fi), nil
}

// Delete removes a backup. A restore waiting for it is cancelled.
func (s *Service) Delete(name string) error {
	p, _, err := s.path(name)
	if err != nil {
		return err
	}
	if pending, _ := s.PendingRestore(); pending != nil && pending.Name == name {
		if err := s.CancelRestore(); err != nil {
			return err
		}
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	// It may have been the newest scheduled one: the scheduler lists again.
	s.mu.Lock()
	s.newestKnown = false
	s.mu.Unlock()
	return nil
}

// PendingRestore is a restore waiting for the next start.
type PendingRestore struct {
	Name        string    `json:"name"`
	RequestedAt time.Time `json:"requested_at"`
	RequestedBy string    `json:"requested_by"`
	// Schema is the newest migration the backup has, for the console ("older
	// backups are brought up to date when the server starts").
	Schema string `json:"schema"`
}

// RestoreResult is how the last restore went, written at start.
type RestoreResult struct {
	Name        string    `json:"name"`
	AppliedAt   time.Time `json:"applied_at"`
	RequestedBy string    `json:"requested_by"`
	OK          bool      `json:"ok"`
	// Error is why it wasn't applied: "missing", "unusable", "newer" or "failed"
	// ("" when OK). The database was left as it was.
	Error string `json:"error,omitempty"`
	// SafetyCopy is the backup made of the database the restore replaced.
	SafetyCopy string `json:"safety_copy,omitempty"`
}

// RequestRestore checks the backup (it must be an AudioSilo database this server
// can open) and marks it to be restored at the next start, replacing any restore
// already waiting. by is who asked, for the record.
func (s *Service) RequestRestore(ctx context.Context, name, by string) (PendingRestore, error) {
	p, _, err := s.path(name)
	if err != nil {
		return PendingRestore{}, err
	}
	// The quick look (schema, readable) fits a request; the full check reads every
	// page, so it waits for the start, which reports a damaged backup once and
	// leaves the database as it is.
	info, err := store.Inspect(ctx, p, false)
	if err != nil {
		return PendingRestore{}, err
	}
	pr := PendingRestore{Name: name, RequestedAt: s.now().UTC(), RequestedBy: by, Schema: info.Schema}
	return pr, writeJSONFile(filepath.Join(s.dataDir, markerFile), pr)
}

// CancelRestore drops a restore waiting for the next start (nothing waiting is fine).
func (s *Service) CancelRestore() error {
	err := os.Remove(filepath.Join(s.dataDir, markerFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// PendingRestore returns the restore waiting for the next start, if any.
func (s *Service) PendingRestore() (*PendingRestore, error) {
	return readJSONFile[PendingRestore](filepath.Join(s.dataDir, markerFile))
}

// LastRestore returns how the last restore went, if one was applied or refused.
func (s *Service) LastRestore() (*RestoreResult, error) {
	return readJSONFile[RestoreResult](filepath.Join(s.dataDir, resultFile))
}

// ApplyPendingRestore applies a restore waiting in dataDir to the database at
// dbPath, before anything opens it (the launcher calls it first thing). It checks
// the backup again, copies the current database into the backups folder (dir, as
// for New), then puts the backup in its place. It returns nil when no restore was
// waiting. Whatever happens, the marker is removed and the outcome is written for
// the console, so a backup that can't be restored is reported once instead of
// stopping every start; a refused restore leaves the database as it was.
func ApplyPendingRestore(ctx context.Context, dataDir, dir, dbPath string, log *slog.Logger) (*RestoreResult, error) {
	marker := filepath.Join(dataDir, markerFile)
	pending, err := readJSONFile[PendingRestore](marker)
	if pending == nil && err == nil {
		return nil, nil
	}
	res := &RestoreResult{AppliedAt: time.Now().UTC()}
	if err != nil {
		// An unreadable marker is reported and dropped like any refused restore, not
		// left to stop every start.
		res.Error = "failed"
		log.Error("the waiting restore can't be read; the database was left as it was", "err", err)
	} else {
		res.Name, res.RequestedBy = pending.Name, pending.RequestedBy
		s := New(nil, dataDir, dir, log)
		if err := s.applyRestore(ctx, pending.Name, dbPath, res); err != nil {
			log.Error("restoring a backup failed; the database was left as it was", "name", pending.Name, "err", err)
		} else {
			res.OK = true
			log.Warn("restored the database from a backup", "name", pending.Name, "safety_copy", res.SafetyCopy)
		}
	}
	if err := writeJSONFile(filepath.Join(dataDir, resultFile), res); err != nil {
		log.Warn("recording the restore's outcome failed", "err", err)
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, fmt.Errorf("remove restore marker: %w", err)
	}
	return res, nil
}

func (s *Service) applyRestore(ctx context.Context, name, dbPath string, res *RestoreResult) error {
	src, _, err := s.path(name)
	if err != nil {
		res.Error = "missing"
		return err
	}
	if _, err := store.Inspect(ctx, src, true); err != nil {
		res.Error = "unusable"
		if errors.Is(err, store.ErrNewerDatabase) {
			res.Error = "newer"
		}
		return err
	}
	res.Error = "failed"
	// Stage the backup beside the database first, so the swap below is a rename.
	staged := dbPath + ".restoring"
	if err := copyFile(src, staged); err != nil {
		_ = os.Remove(staged)
		return err
	}
	if _, err := os.Stat(dbPath); err == nil {
		copied, err := s.write(ctx, KindBeforeRestore, func(ctx context.Context, to string) error {
			return store.VacuumFile(ctx, dbPath, to)
		})
		if err != nil {
			// The database can't be copied (perhaps why it is being restored): move it
			// and its -wal/-shm aside in the data folder instead, untouched.
			s.log.Warn("copying the current database before the restore failed; keeping its files instead", "err", err)
			kept, err := keepAside(dbPath)
			if err != nil {
				_ = os.Remove(staged)
				return fmt.Errorf("keep the current database: %w", err)
			}
			res.SafetyCopy = kept
		} else {
			res.SafetyCopy = copied.Name
		}
	}
	// The -wal and -shm files belong to the database being replaced.
	for _, side := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + side); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(staged)
			return err
		}
	}
	if err := os.Rename(staged, dbPath); err != nil {
		_ = os.Remove(staged)
		return err
	}
	res.Error = ""
	return nil
}

// keepAside renames the database at dbPath and its -wal/-shm to
// <name>.before-restore-<time> beside it, returning the new name of the main file.
// A failure puts back what was already moved, so the database is left as it was
// (never its main file without its -wal).
func keepAside(dbPath string) (string, error) {
	kept := fmt.Sprintf("%s.before-restore-%s", dbPath, time.Now().UTC().Format(timeLayout))
	var moved []string
	for _, side := range []string{"-wal", "-shm", ""} {
		err := os.Rename(dbPath+side, kept+side)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			for _, m := range moved {
				_ = os.Rename(kept+m, dbPath+m)
			}
			return "", err
		}
		moved = append(moved, side)
	}
	return filepath.Base(kept), nil
}

// copyFile copies src to dst (created, owner-only) and flushes it to disk.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func writeJSONFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readJSONFile reads a JSON file into a T; nil (no error) when there is none.
func readJSONFile[T any](path string) (*T, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return &v, nil
}

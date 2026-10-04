package backup

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// env is a data folder with a database holding one account named name.
type env struct {
	dataDir, dbPath string
	db              *store.DB
	svc             *Service
	clock           time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{dataDir: dir, dbPath: filepath.Join(dir, "audiosilo.db"), clock: time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)}
	db, err := store.Open(context.Background(), e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e.db = db
	e.svc = New(db, dir, "", nil)
	e.svc.now = func() time.Time { return e.clock }
	e.svc.loc = time.UTC
	addUser(t, db, "first")
	return e
}

func addUser(t *testing.T, db *store.DB, name string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users(username, password_hash, role, created_at, updated_at) VALUES(?, '', 'user', 'x', 'x')`, name); err != nil {
		t.Fatal(err)
	}
}

func users(t *testing.T, path string) []string {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(context.Background(), `SELECT username FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

func (e *env) tick(d time.Duration) { e.clock = e.clock.Add(d) }

func TestCreateListAndPrune(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc.SetSettings("daily:03:00", 2)

	m, err := e.svc.Create(ctx, KindManual)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "audiosilo-20261004-030000Z-manual.db" || m.Kind != KindManual || m.Size == 0 {
		t.Fatalf("manual backup = %+v", m)
	}
	fi, err := os.Stat(filepath.Join(e.svc.dir, m.Name))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, want owner-only", fi.Mode().Perm())
	}
	for range 3 {
		e.tick(24 * time.Hour)
		if _, err := e.svc.Create(ctx, KindScheduled); err != nil {
			t.Fatal(err)
		}
	}
	list, err := e.svc.List()
	if err != nil {
		t.Fatal(err)
	}
	// Two scheduled kept (newest first), the manual one untouched by retention.
	var names []string
	for _, b := range list {
		names = append(names, b.Name)
	}
	want := []string{
		"audiosilo-20261007-030000Z-scheduled.db",
		"audiosilo-20261006-030000Z-scheduled.db",
		"audiosilo-20261004-030000Z-manual.db",
	}
	if len(names) != len(want) {
		t.Fatalf("backups = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("backups = %v, want %v", names, want)
		}
	}
	if st := e.svc.Status(); st.Last == nil || !st.Last.OK || st.Last.Trigger != KindScheduled || st.Running ||
		st.Latest == nil || st.Latest.Name != "audiosilo-20261007-030000Z-scheduled.db" {
		t.Fatalf("status = %+v", st)
	}
}

func TestListIgnoresOtherFiles(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.svc.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"notes.txt", "audiosilo.db", ".audiosilo-x.db.tmp", "audiosilo-copied.db"} {
		_ = os.WriteFile(filepath.Join(e.svc.dir, n), []byte("x"), 0o600)
	}
	_ = os.Mkdir(filepath.Join(e.svc.dir, "audiosilo-dir.db"), 0o700)
	list, err := e.svc.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "audiosilo-copied.db" || list[0].Kind != KindManual {
		t.Fatalf("list = %+v", list)
	}
	// Leftover temporary files are removed when Run starts.
	e.svc.removeTemp()
	if _, err := os.Stat(filepath.Join(e.svc.dir, ".audiosilo-x.db.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary file left")
	}
}

func TestNamesNeverLeaveTheFolder(t *testing.T) {
	e := newEnv(t)
	secret := filepath.Join(e.dataDir, "audiosilo-secret.db")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"../audiosilo-secret.db", "audiosilo-../x.db", "/etc/passwd", "audiosilo-a/b.db",
		"audiosilo.db", "", "audiosilo-x.db\x00", "audiosilo-..db"} {
		if _, _, err := e.svc.Open(n); !errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) err = %v", n, err)
		}
		if err := e.svc.Delete(n); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete(%q) err = %v", n, err)
		}
		if _, err := e.svc.RequestRestore(context.Background(), n, "admin"); !errors.Is(err, ErrNotFound) {
			t.Errorf("RequestRestore(%q) err = %v", n, err)
		}
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatal("a file outside the folder was touched")
	}
	// A symlink in the folder isn't followed.
	_ = os.MkdirAll(e.svc.dir, 0o700)
	if err := os.Symlink(secret, filepath.Join(e.svc.dir, "audiosilo-link.db")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Open("audiosilo-link.db"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("symlink opened: %v", err)
	}
}

func TestCreateBusyAndFailure(t *testing.T) {
	e := newEnv(t)
	e.svc.mu.Lock()
	e.svc.running = true
	e.svc.mu.Unlock()
	if _, err := e.svc.Create(context.Background(), KindManual); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want busy", err)
	}
	if err := e.svc.Start(context.Background(), KindManual); !errors.Is(err, ErrBusy) {
		t.Fatalf("start err = %v, want busy", err)
	}
	e.svc.mu.Lock()
	e.svc.running = false
	e.svc.mu.Unlock()

	// The folder can't be created: a file is in the way.
	blocked := New(e.db, e.dataDir, filepath.Join(e.dataDir, "file"), nil)
	if err := os.WriteFile(filepath.Join(e.dataDir, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var heard *Result
	blocked.OnFailure = func(r Result) { heard = &r }
	if _, err := blocked.Create(context.Background(), KindScheduled); err == nil {
		t.Fatal("backup into a file succeeded")
	}
	if heard == nil || heard.OK || heard.Error == "" || heard.Trigger != KindScheduled {
		t.Fatalf("failure heard = %+v", heard)
	}
	if st := blocked.Status(); st.Last == nil || st.Last.OK {
		t.Fatalf("status = %+v", st)
	}
}

func TestScheduleDue(t *testing.T) {
	e := newEnv(t)
	e.clock = time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	e.svc.anchor = e.clock
	e.svc.SetSettings("", 7)
	if e.svc.due() || e.svc.Status().Next != nil {
		t.Fatal("due while off")
	}
	e.svc.SetSettings("daily:03:00", 7)
	if e.svc.due() {
		t.Fatal("due before its time")
	}
	if n := e.svc.Status().Next; n == nil || !n.Equal(time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("next = %v", n)
	}
	e.clock = time.Date(2026, 10, 4, 3, 0, 30, 0, time.UTC)
	if !e.svc.due() {
		t.Fatal("not due at its time")
	}
	if _, err := e.svc.Create(context.Background(), KindScheduled); err != nil {
		t.Fatal(err)
	}
	if e.svc.due() {
		t.Fatal("due again right after")
	}
	// A failed attempt waits for the next slot instead of retrying every minute.
	e.clock = time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)
	e.svc.mu.Lock()
	e.svc.tried = e.clock
	e.svc.mu.Unlock()
	e.tick(time.Minute)
	if e.svc.due() {
		t.Fatal("a failed slot is retried")
	}
	// A server that was off at its time makes the missed backup when it is back.
	e.clock = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	e.svc.mu.Lock()
	e.svc.tried = time.Time{}
	e.svc.mu.Unlock()
	if !e.svc.due() {
		t.Fatal("a missed backup isn't made")
	}
}

// After a restart (Run starting now), a slot missed while the server was off is
// counted from the newest scheduled backup, not from the restart.
func TestScheduleCatchesUpAfterRestart(t *testing.T) {
	e := newEnv(t)
	e.svc.SetSettings("daily:03:00", 7)
	if _, err := e.svc.Create(context.Background(), KindScheduled); err != nil {
		t.Fatal(err)
	}
	// The server was off over the next 03:00 and starts again at 09:00.
	e.clock = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	restarted := New(e.db, e.dataDir, "", nil)
	restarted.now, restarted.loc = func() time.Time { return e.clock }, time.UTC
	restarted.SetSettings("daily:03:00", 7)
	restarted.anchor = e.clock
	if !restarted.due() {
		t.Fatalf("the missed backup isn't made; next = %v", restarted.Status().Next)
	}
	// A fresh server with no scheduled backup waits for its first slot.
	empty := New(e.db, t.TempDir(), "", nil)
	empty.now, empty.loc = func() time.Time { return e.clock }, time.UTC
	empty.SetSettings("daily:03:00", 7)
	empty.anchor = e.clock
	if empty.due() {
		t.Fatal("a fresh server backs up before its slot")
	}
}

// Start reads as running as soon as it returns, so the request's answer says so.
func TestStartRunsAtOnce(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.Start(context.Background(), KindManual); err != nil {
		t.Fatal(err)
	}
	e.svc.mu.Lock()
	running, last := e.svc.running, e.svc.last
	e.svc.mu.Unlock()
	if !running && last == nil {
		t.Fatal("not running right after Start")
	}
	for range 200 {
		if st := e.svc.Status(); !st.Running && st.Last != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the backup never finished")
}

// Retention never removes the backup a restore is waiting for.
func TestPruneKeepsPendingRestore(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc.SetSettings("daily:03:00", 1)
	first, err := e.svc.Create(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RequestRestore(ctx, first.Name, "admin"); err != nil {
		t.Fatal(err)
	}
	e.tick(24 * time.Hour)
	if _, err := e.svc.Create(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.path(first.Name); err != nil {
		t.Fatal("retention removed the backup a restore is waiting for")
	}
	// Once the restore is cancelled, the next prune lets it go.
	if err := e.svc.CancelRestore(); err != nil {
		t.Fatal(err)
	}
	e.tick(24 * time.Hour)
	if _, err := e.svc.Create(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.path(first.Name); !errors.Is(err, ErrNotFound) {
		t.Fatal("an old backup outlived retention")
	}
}

// An unreadable restore marker is reported once and dropped, not left to stop
// every start.
func TestUnreadableMarkerIsDropped(t *testing.T) {
	e := newEnv(t)
	marker := filepath.Join(e.dataDir, markerFile)
	if err := os.WriteFile(marker, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ApplyPendingRestore(context.Background(), e.dataDir, "", e.dbPath, slog.Default())
	if err != nil || res == nil || res.OK || res.Error != "failed" {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the marker was left")
	}
	if last, _ := e.svc.LastRestore(); last == nil || last.OK {
		t.Fatalf("outcome = %+v", last)
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b, err := e.svc.Create(ctx, KindManual)
	if err != nil {
		t.Fatal(err)
	}
	addUser(t, e.db, "after-the-backup")

	pr, err := e.svc.RequestRestore(ctx, b.Name, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Name != b.Name || pr.RequestedBy != "admin" || pr.Schema == "" {
		t.Fatalf("pending = %+v", pr)
	}
	if got, _ := e.svc.PendingRestore(); got == nil || got.Name != b.Name {
		t.Fatalf("PendingRestore = %+v", got)
	}
	// The server stops; the next start applies it before opening the database.
	_ = e.db.Close()
	e.tick(time.Hour)
	res, err := ApplyPendingRestore(ctx, e.dataDir, "", e.dbPath, slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.OK || res.Name != b.Name || res.RequestedBy != "admin" || res.SafetyCopy == "" {
		t.Fatalf("result = %+v", res)
	}
	if got := users(t, e.dbPath); len(got) != 1 || got[0] != "first" {
		t.Fatalf("restored users = %v", got)
	}
	// The replaced database was kept, whole, as a backup.
	if got := users(t, filepath.Join(e.dataDir, dirName, res.SafetyCopy)); len(got) != 2 {
		t.Fatalf("safety copy users = %v", got)
	}
	if p, _ := e.svc.PendingRestore(); p != nil {
		t.Fatal("marker left behind")
	}
	if last, _ := e.svc.LastRestore(); last == nil || !last.OK {
		t.Fatalf("last restore = %+v", last)
	}
	// Nothing waiting: nothing happens.
	if res, err := ApplyPendingRestore(ctx, e.dataDir, "", e.dbPath, slogDiscard()); res != nil || err != nil {
		t.Fatalf("second apply = %+v, %v", res, err)
	}
}

func TestRestoreRefusedLeavesDatabase(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b, err := e.svc.Create(ctx, KindManual)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RequestRestore(ctx, b.Name, "admin"); err != nil {
		t.Fatal(err)
	}
	// The backup goes bad between the request and the restart.
	if err := os.WriteFile(filepath.Join(e.svc.dir, b.Name), []byte("garbage, not a database at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	addUser(t, e.db, "second")
	_ = e.db.Close()
	res, err := ApplyPendingRestore(ctx, e.dataDir, "", e.dbPath, slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Error != "unusable" || res.SafetyCopy != "" {
		t.Fatalf("result = %+v", res)
	}
	if got := users(t, e.dbPath); len(got) != 2 {
		t.Fatalf("database changed: %v", got)
	}
	if p, _ := e.svc.PendingRestore(); p != nil {
		t.Fatal("a refused restore stays pending (it would be retried at every start)")
	}

	// A backup that has since been deleted.
	if err := writeJSONFile(filepath.Join(e.dataDir, markerFile), PendingRestore{Name: "audiosilo-gone.db"}); err != nil {
		t.Fatal(err)
	}
	res, _ = ApplyPendingRestore(ctx, e.dataDir, "", e.dbPath, slogDiscard())
	if res.OK || res.Error != "missing" {
		t.Fatalf("result = %+v", res)
	}
}

func TestRequestRestoreChecksTheFile(t *testing.T) {
	e := newEnv(t)
	_ = os.MkdirAll(e.svc.dir, 0o700)
	if err := os.WriteFile(filepath.Join(e.svc.dir, "audiosilo-junk.db"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RequestRestore(context.Background(), "audiosilo-junk.db", "admin"); !errors.Is(err, store.ErrNotADatabase) {
		t.Fatalf("err = %v", err)
	}
	if p, _ := e.svc.PendingRestore(); p != nil {
		t.Fatal("an unusable backup was marked")
	}
}

func TestDeleteCancelsItsRestore(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b, err := e.svc.Create(ctx, KindManual)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RequestRestore(ctx, b.Name, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(b.Name); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.svc.PendingRestore(); p != nil {
		t.Fatal("deleting the backup left its restore pending")
	}
	if err := e.svc.CancelRestore(); err != nil {
		t.Fatalf("cancelling nothing: %v", err)
	}
}

func slogDiscard() *slog.Logger { return slog.New(slog.DiscardHandler) }

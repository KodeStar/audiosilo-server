package fsaccess

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// as runs the test as uid:gid, a folder owned by ouid:ogid (known), inside a
// container or not.
func as(t *testing.T, uid, gid, ouid, ogid int, known, container bool) {
	t.Helper()
	r, o, c := running, ownerOf, inCtr
	t.Cleanup(func() { running, ownerOf, inCtr = r, o, c })
	running = func() (int, int) { return uid, gid }
	ownerOf = func(string) (int, int, bool) { return ouid, ogid, known }
	inCtr = func() bool { return container }
}

func TestHint(t *testing.T) {
	cases := []struct {
		name                 string
		uid, gid, ouid, ogid int
		known, container     bool
		write                bool
		want                 string
	}{
		{"container, another owner", 1000, 1000, 1026, 100, true, true, true,
			"The server runs as user 1000:1000 and the folder belongs to 1026:100. Set PUID=1026 and PGID=100 (the folder's owner) on the container, or give user 1000:1000 read and write access to the folder."},
		{"container, owner unknown", 1000, 1000, 0, 0, false, true, false,
			"The server runs as user 1000:1000. Set PUID and PGID on the container to the folder's owner, or give user 1000:1000 read access to the folder."},
		{"the right owner, refused by the mode", 1026, 100, 1026, 100, true, true, true,
			"The server runs as user 1026:100 and the folder belongs to 1026:100. Give user 1026:100 read and write access to the folder (its permissions refuse it)."},
		{"owned by root: never PUID=0", 1000, 1000, 0, 0, true, true, true,
			"The server runs as user 1000:1000 and the folder belongs to 0:0. Give user 1000:1000 read and write access to the folder, for example by making 1000:1000 its owner on the host."},
		{"no container", 501, 20, 1026, 100, true, false, false,
			"The server runs as user 501:20 and the folder belongs to 1026:100. Run the server as the folder's owner, or give user 501:20 read access to the folder."},
		{"no uids (Windows)", -1, -1, 0, 0, false, false, true,
			"Give the account the server runs as read and write access to the folder."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			as(t, c.uid, c.gid, c.ouid, c.ogid, c.known, c.container)
			if got := Hint("/data", c.write); got != c.want {
				t.Fatalf("Hint =\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

func TestErrorWording(t *testing.T) {
	as(t, 1000, 1000, 1026, 100, true, true)
	err := Wrap("/data", true, &fs.PathError{Op: "open", Path: "/data/.x", Err: fs.ErrPermission})
	want := "the server can't write to /data: permission denied. The server runs as user 1000:1000 and the folder belongs to 1026:100. Set PUID=1026 and PGID=100 (the folder's owner) on the container, or give user 1000:1000 read and write access to the folder."
	if err.Error() != want {
		t.Fatalf("got\n%s\nwant\n%s", err, want)
	}
	if !IsPermission(err) {
		t.Fatal("a wrapped permission error is no longer one")
	}
	// Not a permission problem: the cause alone, no owner advice.
	ro := Wrap("/backups", true, errors.New("read-only file system"))
	if got := ro.Error(); got != "the server can't write to /backups: read-only file system" {
		t.Fatalf("got %q", got)
	}
	if Wrap("/x", false, nil) != nil {
		t.Fatal("nil wrapped as an error")
	}
	if again := Wrap("/y", false, err); again != err {
		t.Fatal("an *Error was wrapped twice")
	}
}

// skipUnlessModesBite skips where file modes don't refuse this process: as
// root, or on Windows.
func skipUnlessModesBite(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes don't refuse this user")
	}
}

func TestCheckWrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckWrite(dir, "config.yaml", "audiosilo.db"); err != nil {
		t.Fatalf("writable folder: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 1 {
		t.Fatalf("the check left files behind: %v", left)
	}
	if err := CheckWrite(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing folder passed")
	}
}

func TestCheckWriteRefused(t *testing.T) {
	skipUnlessModesBite(t)
	// A folder the user may not write to.
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	err := CheckWrite(ro)
	var e *Error
	if !errors.As(err, &e) || e.Path != ro || !e.Write || !IsPermission(err) {
		t.Fatalf("read-only folder: %v", err)
	}
	if !strings.Contains(err.Error(), "The server runs as user "+User()) {
		t.Fatalf("no user named: %v", err)
	}

	// A writable folder whose database belongs to someone else (here: read-only).
	dir := t.TempDir()
	db := filepath.Join(dir, "audiosilo.db")
	if err := os.WriteFile(db, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	err = CheckWrite(dir, "config.yaml", "audiosilo.db")
	if !errors.As(err, &e) || e.Path != db || !IsPermission(err) {
		t.Fatalf("read-only database: %v", err)
	}
	if !strings.Contains(err.Error(), "the file belongs to") {
		t.Fatalf("a file worded as a folder: %v", err)
	}
}

func TestCheckRead(t *testing.T) {
	empty := t.TempDir()
	if err := CheckRead(empty); err != nil {
		t.Fatalf("empty folder: %v", err)
	}
	if err := os.WriteFile(filepath.Join(empty, "a.m4b"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckRead(empty); err != nil {
		t.Fatalf("folder: %v", err)
	}
	if err := CheckRead(filepath.Join(empty, "missing")); err == nil {
		t.Fatal("a missing folder passed")
	}
}

func TestCheckReadRefused(t *testing.T) {
	skipUnlessModesBite(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o300); err != nil { // write and search, no listing
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := CheckRead(dir)
	var e *Error
	if !errors.As(err, &e) || e.Path != dir || e.Write || !IsPermission(err) {
		t.Fatalf("unlistable folder: %v", err)
	}
	if !strings.Contains(err.Error(), "the server can't read "+dir) {
		t.Fatalf("got %v", err)
	}
}

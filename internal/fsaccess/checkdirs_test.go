package fsaccess

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/mounts"
)

func TestCheckDirsBackups(t *testing.T) {
	skipUnlessModesBite(t)
	data := t.TempDir()
	backups := filepath.Join(data, "backups")
	if err := os.Mkdir(backups, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(backups, 0o700) })
	var out bytes.Buffer
	check := func(configured string, table []mounts.Mount) string {
		out.Reset()
		checkDirs(data, configured, false, true, table, &out, time.Second)
		return out.String()
	}
	for _, dir := range []string{"", backups + "/"} {
		got := check(dir, nil)
		if !strings.Contains(got, "can't write to "+backups) || !strings.Contains(got, User()) {
			t.Fatalf("backups %q: %q", dir, got)
		}
	}
	readOnly := []mounts.Mount{{Point: data, ReadOnly: true}, {Point: backups}}
	if got := check("", readOnly); strings.Contains(got, "without :ro") {
		t.Fatalf("a writable child mount: %s", got)
	}
	if got := check("", readOnly[:1]); !strings.Contains(got, "without :ro") || strings.Contains(got, "Changing its owner") {
		t.Fatalf("read-only ancestor: %s", got)
	}
	for _, dir := range []string{"relative", "/", data + "/../" + filepath.Base(data) + "/backups", filepath.Join(data, "not-created")} {
		if got := check(dir, nil); got != "" {
			t.Errorf("unsupported or missing backups %q: %q", dir, got)
		}
	}
	link := filepath.Join(data, "to-root")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	if got := check(link, nil); got != "" {
		t.Errorf("root alias: %q", got)
	}
}

func TestCheckDirsMountedFile(t *testing.T) {
	skipUnlessModesBite(t)
	data := t.TempDir()
	file := filepath.Join(data, "tab\t7\\040")
	if err := os.WriteFile(file, []byte("keep"), 0o000); err != nil {
		t.Fatal(err)
	}
	table := []mounts.Mount{{Point: file, FSType: "ext4", ReadOnly: true}}
	var out bytes.Buffer
	checkDirs(data, "", false, false, table, &out, time.Second)
	if got := out.String(); !strings.Contains(got, "can't read "+file) || !strings.Contains(got, "the file belongs to") || strings.Contains(got, "offline") || strings.Contains(got, "Changing its owner") {
		t.Fatalf("unreadable mounted file: %q", got)
	}
	if err := os.Chmod(file, 0o400); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	checkDirs(data, "", false, false, table, &out, time.Second)
	if out.Len() != 0 {
		t.Fatalf("readable read-only file: %s", out.String())
	}
}

// A shared folder can already contain a symlink with the write probe's prefix.
// The probe uses an exclusive temporary file and removes only its own file.
func TestCheckDirsWriteProbe(t *testing.T) {
	data := t.TempDir()
	victim := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(data, ".audiosilo-write-check-123456")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	var out bytes.Buffer
	checkDirs(data, "", false, false, nil, &out, time.Second)
	left, err := os.ReadDir(data)
	if err != nil || len(left) != 1 || left[0].Name() != filepath.Base(link) || left[0].Type()&os.ModeSymlink == 0 || out.Len() != 0 {
		t.Fatalf("after write check: %v %v %s", left, err, out.String())
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "untouched" {
		t.Fatalf("symlink target: %q %v", b, err)
	}
}

func TestFolderCheckTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var out bytes.Buffer
	checkFolder(&out, "/backups", time.Millisecond, func() string { <-release; return "late permission error" })
	if got := out.String(); !strings.Contains(got, "/backups didn't answer") || strings.Contains(got, "owner") {
		t.Errorf("hung backups check: %q", got)
	}
}

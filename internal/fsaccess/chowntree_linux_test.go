//go:build linux

package fsaccess

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/kodestar/audiosilo-server/internal/mounts"
)

// The walk's choices, with the changes recorded instead of made (a test runs
// without the right to give files away): links, mount points and snapshot
// folders are left alone, a read and write mounted file is changed.
func TestChownTreeStaysOnTheDataMount(t *testing.T) {
	data, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	for _, dir := range []string{"d", "mounted", ".snapshots/1"} {
		if err := os.MkdirAll(filepath.Join(data, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"a.db", "d/f", "mounted/theirs", ".snapshots/1/copy", "config.yaml", "ro.yaml"} {
		if err := os.WriteFile(filepath.Join(data, file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"link": filepath.Join(outside, "secret"), "dirlink": outside} {
		if err := os.Symlink(target, filepath.Join(data, name)); err != nil {
			t.Fatal(err)
		}
	}
	table := []mounts.Mount{
		{Point: "/"},
		{Point: data + "/mounted"},
		{Point: data + "/config.yaml"},
		{Point: data + "/ro.yaml", ReadOnly: true},
	}

	var changed []string
	at := func(fd int, name string) string {
		dir, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(data, filepath.Join(dir, name))
		return rel
	}
	w := walker{
		uid: os.Getuid() + 1, gid: os.Getgid() + 1, // everything is another's
		chownAt: func(fd int, name string) error { changed = append(changed, at(fd, name)); return nil },
		chownFd: func(fd int) error { changed = append(changed, at(fd, "")); return nil },
	}
	if err := w.run(data, table); err != nil {
		t.Fatal(err)
	}
	slices.Sort(changed)
	want := []string{"a.db", "config.yaml", "d", "d/f", "dirlink", "link"}
	if !slices.Equal(changed, want) {
		t.Fatalf("changed %q, want %q", changed, want)
	}

	// Entries already the owner's are not changed again.
	changed = nil
	w.uid, w.gid = os.Getuid(), os.Getgid()
	if err := w.run(data, table); err != nil || len(changed) != 0 {
		t.Fatalf("own files: changed %q (%v)", changed, err)
	}
}

// A mount the table does not list (one made after it was read, or one the walk
// itself set off) is told by the kernel and left alone. Needs root, to mount.
func TestChownTreeUnlistedMount(t *testing.T) {
	data, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(data, "late")
	if err := os.Mkdir(late, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", late, "tmpfs", 0, ""); err != nil {
		t.Skipf("can't mount here: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(late, unix.MNT_DETACH) })
	if err := os.WriteFile(filepath.Join(late, "theirs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var changed []string
	w := walker{
		uid: os.Getuid() + 1, gid: os.Getgid() + 1,
		chownAt: func(_ int, name string) error { changed = append(changed, name); return nil },
		chownFd: func(int) error { changed = append(changed, "a folder"); return nil },
	}
	if err := w.run(data, []mounts.Mount{{Point: "/"}}); err != nil || len(changed) != 0 {
		t.Fatalf("changed %q (%v)", changed, err)
	}
}

func TestChownTreeRefuses(t *testing.T) {
	data := t.TempDir()
	if err := chownTree(data, 0, 0, nil); err == nil {
		t.Fatal("walked with no mount table")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(data, link); err != nil {
		t.Fatal(err)
	}
	table := []mounts.Mount{{Point: "/"}}
	if err := chownTree(link, 0, 0, table); err == nil {
		t.Fatal("walked through a link at the data folder's name")
	}
	if err := chownTree("/", 0, 0, table); err == nil {
		t.Fatal("walked the whole system")
	}
}

// A folder whose stat says another mount is not entered or changed; the
// kernel's own answer is checked as well as the table's.
func TestChownTreeElsewhere(t *testing.T) {
	w := walker{known: true, mountID: 7, fsType: 0x58465342} // xfs
	parent := unix.Statx_t{Dev_major: 8, Dev_minor: 1}
	cases := map[string]struct {
		st   unix.Statx_t
		want bool
	}{
		"same mount":  {unix.Statx_t{Mask: unix.STATX_MNT_ID, Mnt_id: 7, Dev_major: 0, Dev_minor: 40}, false},
		"other mount": {unix.Statx_t{Mask: unix.STATX_MNT_ID, Mnt_id: 8, Dev_major: 8, Dev_minor: 1}, true},
		"mount root":  {unix.Statx_t{Mask: unix.STATX_MNT_ID, Mnt_id: 7, Attributes: unix.STATX_ATTR_MOUNT_ROOT, Attributes_mask: unix.STATX_ATTR_MOUNT_ROOT}, true},
		"automount":   {unix.Statx_t{Mask: unix.STATX_MNT_ID, Mnt_id: 7, Attributes: unix.STATX_ATTR_AUTOMOUNT, Attributes_mask: unix.STATX_ATTR_AUTOMOUNT}, true},
		// Before 5.8 the kernel set the bit but left it out of the mask.
		"automount, unmasked": {unix.Statx_t{Dev_major: 8, Dev_minor: 1, Attributes: unix.STATX_ATTR_AUTOMOUNT}, true},
		"old kernel dev":      {unix.Statx_t{Dev_major: 0, Dev_minor: 40}, true},
		"old kernel":          {unix.Statx_t{Dev_major: 8, Dev_minor: 1}, false},
	}
	for name, c := range cases {
		if got := w.elsewhere(&c.st, &parent); got != c.want {
			t.Errorf("%s: elsewhere %v, want %v", name, got, c.want)
		}
	}
	w.fsType = unix.BTRFS_SUPER_MAGIC // a subvolume's device number is its own
	old := unix.Statx_t{Dev_major: 0, Dev_minor: 40}
	if w.elsewhere(&old, &parent) {
		t.Error("btrfs subvolume on an old kernel: skipped")
	}
}

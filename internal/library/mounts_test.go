package library

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// A container's mount table: the books volumes are offered (sorted, an escaped
// space read back, a repeat once); the root, system paths, pseudo filesystems,
// a bound file, the data folder and anything not a folder are not; a folder
// whose name only begins like a system one (/snapshots) is offered.
func TestMountHints(t *testing.T) {
	t.Parallel()
	table := `
1 0 0:1 / / rw - overlay overlay rw
2 1 0:2 / /proc rw - proc proc rw
3 1 0:3 / /dev rw - tmpfs tmpfs rw
4 3 0:4 / /dev/shm rw - tmpfs shm rw
5 1 0:5 / /sys rw - sysfs sysfs ro
6 1 8:1 /srv/books /audiobooks rw,relatime - ext4 /dev/sda1 rw
7 1 8:1 /srv/data /config rw - ext4 /dev/sda1 rw
8 1 8:1 /srv/hosts /etc/hosts rw - ext4 /dev/sda1 rw
9 1 0:6 / /mnt/My\040Books rw shared:1 - fuse.shfs shfs rw
10 1 8:1 /srv/books /audiobooks rw - ext4 /dev/sda1 rw
11 1 8:1 /srv/file /media/a-file rw - ext4 /dev/sda1 rw
12 1 0:7 / /run/secrets rw - tmpfs tmpfs rw
13 1 8:2 / /data rw - xfs /dev/sdb1 rw
14 1 8:3 / /snapshots rw - btrfs /dev/sdc1 rw
15 1 8:3 / /boot/efi rw - vfat /dev/sdc2 rw
not a mount line
`
	notDir := map[string]bool{"/media/a-file": true}
	got := mountHints(strings.NewReader(table), []string{"/config/"}, func(p string) bool { return !notDir[p] })
	want := []string{"/audiobooks", "/data", "/mnt/My Books", "/snapshots"}
	if !slices.Equal(got, want) {
		t.Errorf("hints = %q, want %q", got, want)
	}

	var many strings.Builder
	for i := range maxMountHints + 5 {
		fmt.Fprintf(&many, "%d 1 8:1 / /books%02d rw - ext4 /dev/sda1 rw\n", i, i)
	}
	if got := mountHints(strings.NewReader(many.String()), nil, func(string) bool { return true }); len(got) != maxMountHints {
		t.Errorf("%d hints, want the cap %d", len(got), maxMountHints)
	}
	if got := mountHints(strings.NewReader(""), nil, func(string) bool { return true }); got == nil || len(got) != 0 {
		t.Errorf("no mounts = %#v, want []", got)
	}

	// Off Linux there is no mount table to read: always a list, never nil.
	if runtime.GOOS != "linux" {
		if got := MountHints(); got == nil || len(got) != 0 {
			t.Errorf("MountHints off Linux = %#v, want []", got)
		}
	}
}

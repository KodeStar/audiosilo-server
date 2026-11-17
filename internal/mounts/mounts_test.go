package mounts

import (
	"path/filepath"
	"runtime"
	"testing"
)

// A path is keyed by the longest mount point that holds it, by whole segments;
// with no mount holding it, by its volume root.
func TestLongest(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix paths")
	}
	points := []string{"/", "/mnt", "/mnt/nas", "/mnt/nas/deep", "/media/usb"}
	for path, want := range map[string]string{
		"/mnt/nas":             "/mnt/nas",
		"/mnt/nas/Author/Book": "/mnt/nas",
		"/mnt/nas2/Book":       "/mnt",
		"/mnt/nas/deep/x":      "/mnt/nas/deep",
		"/media/usb/a":         "/media/usb",
		"/media/usbx":          "/",
		"/srv/books":           "/",
	} {
		if got := longest(points, path); got != want {
			t.Errorf("longest(%q) = %q, want %q", path, got, want)
		}
	}
	if got := longest(nil, "/mnt/nas/x"); got != "/" {
		t.Errorf("no table: %q, want the volume root", got)
	}
}

// Of reads this machine's table: a path is keyed by a mount point at or above
// it, the same key for two paths on the same mount.
func TestOf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key := Of(filepath.Join(dir, "a", "b"))
	if !within(filepath.Join(dir, "a", "b"), key) {
		t.Errorf("Of = %q, not at or above the path", key)
	}
	if again := Of(filepath.Join(dir, "c")); again != key {
		t.Errorf("Of of a sibling = %q, want %q", again, key)
	}
}

// ParseLine reads the mount point (octal escapes undone) and the filesystem
// type; a line with no separator, or too few fields, is not a mount.
func TestParseLine(t *testing.T) {
	t.Parallel()
	point, fstype, ok := ParseLine(`9 1 0:6 / /mnt/My\040Books rw shared:1 - fuse.shfs shfs rw`)
	if !ok || point != "/mnt/My Books" || fstype != "fuse.shfs" {
		t.Errorf("ParseLine = %q %q %t", point, fstype, ok)
	}
	for _, bad := range []string{"not a mount line", "1 2 - ext4", "1 0 0:1 / / rw -"} {
		if _, _, ok := ParseLine(bad); ok {
			t.Errorf("ParseLine(%q) read a mount", bad)
		}
	}
}

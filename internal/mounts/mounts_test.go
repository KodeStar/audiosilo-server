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
	points := map[string]struct{}{"/": {}, "/mnt": {}, "/mnt/nas": {}, "/mnt/nas/deep": {}, "/media/usb": {}}
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
	if !Within(filepath.Join(dir, "a", "b"), key) {
		t.Errorf("Of = %q, not at or above the path", key)
	}
	if again := Of(filepath.Join(dir, "c")); again != key {
		t.Errorf("Of of a sibling = %q, want %q", again, key)
	}
}

// Within is by whole segments: a folder holds itself and what is under it,
// never a sibling whose name starts the same; the root holds everything.
func TestWithin(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix paths")
	}
	for _, c := range []struct {
		path, dir string
		want      bool
	}{
		{"/mnt/nas", "/mnt/nas", true},
		{"/mnt/nas/x", "/mnt/nas", true},
		{"/mnt/nas2", "/mnt/nas", false},
		{"/mnt", "/mnt/nas", false},
		{"/anything", "/", true},
	} {
		if got := Within(c.path, c.dir); got != c.want {
			t.Errorf("Within(%q, %q) = %t, want %t", c.path, c.dir, got, c.want)
		}
	}
}

// A PerMount makes one value per mount and hands it back for every path there.
func TestPerMount(t *testing.T) {
	t.Parallel()
	made := 0
	m := NewPerMount(func(mount string) *string { made++; return &mount })
	m.Mount = func(p string) string { return filepath.Dir(p) }
	a, b, c := m.For("/x/1"), m.For("/x/2"), m.For("/y/1")
	if a != b || *a != "/x" || c == a || *c != "/y" || made != 2 {
		t.Errorf("For = %q %q %q (%d made), want one per mount", *a, *b, *c, made)
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

package mounts

import (
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// A path is on the longest mount point that holds it, by whole segments; with
// no mount holding it, on its volume root.
func TestLongest(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix paths")
	}
	points := map[string]string{"/": "/", "/mnt": "/mnt", "/mnt/nas": "/mnt/nas", "/mnt/nas/deep": "/mnt/nas/deep", "/media/usb": "/media/usb"}
	for path, want := range map[string]string{
		"/mnt/nas":             "/mnt/nas",
		"/mnt/nas/Author/Book": "/mnt/nas",
		"/mnt/nas2/Book":       "/mnt",
		"/mnt/nas/deep/x":      "/mnt/nas/deep",
		"/media/usb/a":         "/media/usb",
		"/media/usbx":          "/",
		"/srv/books":           "/",
	} {
		if _, got := longest(points, path); got != want {
			t.Errorf("longest(%q) = %q, want %q", path, got, want)
		}
	}
	if _, got := longest(nil, "/mnt/nas/x"); got != "/" {
		t.Errorf("no table: %q, want the volume root", got)
	}
}

// A read of the table that fails keeps the last read's mount points, so the
// paths keep their mounts' keys (never one key for all of them until the next
// read); the first read has none to keep, and a later one that answers
// replaces them.
func TestNewSnapshotKeepsTheLastTableOnAFailedRead(t *testing.T) {
	t.Parallel()
	failed := func() ([]Mount, error) { return nil, errors.New("too many open files") }
	if first := newSnapshot(nil, failed); len(first.points) != 0 {
		t.Errorf("a failed first read holds %v, want no mount points", first.points)
	}
	good := newSnapshot(nil, func() ([]Mount, error) { return []Mount{{Point: "/"}, {Point: "/mnt/nas/"}}, nil })
	if _, ok := good.points["/mnt/nas"]; !ok || len(good.points) != 2 {
		t.Fatalf("a read = %v, want its two mount points, cleaned", good.points)
	}
	kept := newSnapshot(good, failed)
	if _, ok := kept.points["/mnt/nas"]; !ok || len(kept.points) != 2 || kept.read.Before(good.read) {
		t.Errorf("a failed read after it = %v, want the last table kept", kept.points)
	}
	next := newSnapshot(kept, func() ([]Mount, error) { return []Mount{{Point: "/"}}, nil })
	if _, ok := next.points["/mnt/nas"]; ok || len(next.points) != 1 {
		t.Errorf("a read that answers after it = %v, want the new table", next.points)
	}
}

// Of reads this machine's table: a path is on a mount point at or above it,
// with the same key and point for two paths on the same mount.
func TestOf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key, point := Of(filepath.Join(dir, "a", "b"))
	if key == "" || !Within(filepath.Join(dir, "a", "b"), point) {
		t.Errorf("Of = %q at %q, want a key and a point at or above the path", key, point)
	}
	if k, p := Of(filepath.Join(dir, "c")); k != key || p != point {
		t.Errorf("Of of a sibling = %q at %q, want %q at %q", k, p, key, point)
	}
}

// A mount is keyed by the filesystem it is of, not by where it is mounted: two
// bind mounts of one share have one key (one bound for the share), a share
// mounted anew at the same path has another (nothing stuck on it yet), and a
// mount with no device is keyed by its point. With no table every path has
// its volume root's.
func TestMountsAreKeyedByTheirFilesystem(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix paths")
	}
	read := func(list ...Mount) map[string]string {
		return newSnapshot(nil, func() ([]Mount, error) { return list, nil }).points
	}
	table := read(Mount{Point: "/", Device: "8:1"}, Mount{Point: "/books", Device: "0:52"},
		Mount{Point: "/more-books", Device: "0:52"}, Mount{Point: "/plain"})
	for _, c := range []struct{ path, key, point string }{
		{"/books/Author/Book/01.mp3", "0:52", "/books"},
		{"/more-books/Author", "0:52", "/more-books"},
		{"/var/lib", "8:1", "/"},
		{"/plain/x", "/plain", "/plain"},
	} {
		if key, point := longest(table, c.path); key != c.key || point != c.point {
			t.Errorf("%s is on %q at %q, want %q at %q", c.path, key, point, c.key, c.point)
		}
	}
	// Unmounted (lazily: its stuck reads still hold the old filesystem) and
	// mounted again at the same path, or mounted over.
	again := read(Mount{Point: "/", Device: "8:1"}, Mount{Point: "/books", Device: "0:52"}, Mount{Point: "/books", Device: "0:61"})
	if key, point := longest(again, "/books/Author"); key != "0:61" || point != "/books" {
		t.Errorf("a path mounted anew is on %q at %q, want the new filesystem's key", key, point)
	}
	if key, point := longest(read(), "/books/Author"); key != "/" || point != "/" {
		t.Errorf("with no table = %q at %q, want the volume root for both", key, point)
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

// A PerMount makes one value per mount (per key, whatever the mount point: two
// bind mounts of one share) and hands it back for every path there, made with
// the mount point of the path that first asked.
func TestPerMount(t *testing.T) {
	t.Parallel()
	made := 0
	m := NewPerMount(func(point string) *string { made++; return &point })
	m.SetMount(func(p string) (string, string) {
		point := filepath.Dir(p)
		if point == "/bind" {
			return "/x", point // another mount of /x's filesystem
		}
		return point, point
	})
	a, b, c, d := m.For("/x/1"), m.For("/x/2"), m.For("/y/1"), m.For("/bind/1")
	if a != b || *a != "/x" || c == a || *c != "/y" || d != a || made != 2 {
		t.Errorf("For = %q %q %q %q (%d made), want one per mount", *a, *b, *c, *d, made)
	}
}

// SetMount's stand-in names the mounts until its restore puts back the lookup
// before it, swapped while reads look the lookup up (run with -race: a read
// left under way when a test's cleanup restores the lookup is no data race).
func TestPerMountSetMount(t *testing.T) {
	t.Parallel()
	m := NewPerMount(func(point string) string { return point })
	outer := m.SetMount(func(string) (string, string) { return "outer", "/outer" })
	stop, reading := make(chan struct{}), make(chan struct{})
	var reads sync.WaitGroup
	reads.Go(func() {
		m.For("/x")
		close(reading)
		for {
			select {
			case <-stop:
				return
			default:
				m.For("/x")
			}
		}
	})
	<-reading // the swaps below overlap the reads
	for range 100 {
		m.SetMount(func(string) (string, string) { return "inner", "/inner" })()
	}
	if got := m.For("/x"); got != "/outer" {
		t.Errorf("For after the inner stand-in's restore = %q, want the outer stand-in's /outer", got)
	}
	close(stop)
	reads.Wait()
	outer()
	if _, point := Of("/x"); m.For("/x") != point {
		t.Errorf("For after the outer restore = %q, want Of's mount point %q", m.For("/x"), point)
	}
}

// ParseLine reads the mount point (octal escapes undone), the filesystem type
// and the device number; a line with no separator, or too few fields, is not a
// mount.
func TestParseLine(t *testing.T) {
	t.Parallel()
	m, ok := ParseLine(`9 1 0:6 / /mnt/My\040Books rw shared:1 - fuse.shfs shfs rw`)
	if want := (Mount{Point: "/mnt/My Books", FSType: "fuse.shfs", Device: "0:6"}); !ok || m != want {
		t.Errorf("ParseLine = %+v %t, want %+v", m, ok, want)
	}
	for _, bad := range []string{"not a mount line", "1 2 - ext4", "1 0 0:1 / / rw -"} {
		if _, ok := ParseLine(bad); ok {
			t.Errorf("ParseLine(%q) read a mount", bad)
		}
	}
}

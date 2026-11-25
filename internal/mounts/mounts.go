// Package mounts names the mount a path is on, from the process's mount table
// alone (no filesystem call), so it answers at once even for a path on a share
// that stopped answering. The reads that can hang on such a share (a scan's
// audio reads, the folder picker's listings, the setup preview) are bounded per
// mount with it (PerMount), so one hung share holds back only the reads on it,
// never every library's (server#360).
//
// A mount is told from another by the filesystem it is of (Mount.Device), not
// by where it is mounted: a share unmounted lazily and mounted again at the
// same path is a new filesystem with nothing stuck on it, and two bind mounts
// of one share (two libraries of one NAS in a container) are one filesystem
// with one bound.
package mounts

import (
	"bufio"
	"cmp"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tableTTL is how long a read of the mount table is used before it is read
// again, so a share mounted while the server runs is told apart soon after.
const tableTTL = 10 * time.Second

// Mount is one mount in the mount table: where it is mounted, its filesystem
// type (mountinfo alone) and the filesystem it is of, which is what tells one
// mount from another: mountinfo's device number (major:minor, the same for
// every mount of one filesystem and new for each filesystem mounted), macOS's
// filesystem ID. An empty Device is told by its Point.
type Mount struct {
	Point, FSType, Device string
	ReadOnly              bool // mount or underlying filesystem is read-only (Linux mountinfo)
}

// snapshot is one read of the mount table: each mount point's key (its
// Device, or the point itself with none), and when.
type snapshot struct {
	points map[string]string
	read   time.Time
}

// table is the last read of the mount table; refreshing is held while one
// goroutine reads it again (current), the callers keeping to the last read
// meanwhile.
var (
	table      atomic.Pointer[snapshot]
	refreshing atomic.Bool
)

// Of is the mount holding path, an absolute path (cleaned here): point is the
// longest mount point in the mount table that is path or a folder above it, by
// the path's text alone, so a path reached through a symlink into another
// mount is told by where the link sits; key is that mount's filesystem
// (Mount.Device), the same for every mount of it and another once the path is
// mounted anew. With no table to read (an OS this does not read it on, or one
// whose first read failed) both are the path's volume root ("/", "C:\"), one
// key for every path, as a single bound was before; a later read that fails
// keeps the last table (newSnapshot).
func Of(path string) (key, point string) {
	return longest(current(), filepath.Clean(path))
}

// current is the mount table's points, each with its key. The first read is the caller's; after
// that a read once tableTTL old is read again on a goroutine of its own, the
// callers keeping to the last read meanwhile, so no caller (a scan's reader, a
// picker request before its time-out starts) ever waits on the table again.
func current() map[string]string {
	s := table.Load()
	if s == nil {
		s = newSnapshot(nil, readTable)
		if !table.CompareAndSwap(nil, s) {
			return table.Load().points // another first read landed first
		}
		return s.points
	}
	if time.Since(s.read) > tableTTL && refreshing.CompareAndSwap(false, true) {
		go func() {
			defer refreshing.Store(false)
			table.Store(newSnapshot(s, readTable))
		}()
	}
	return s.points
}

// newSnapshot is a read of the mount table (read: readTable, or a test's). A
// read that fails keeps the last one's points (prev), read again a tableTTL
// later: a table that can't be read right now (no descriptor left, as a hung
// share's stuck reads can leave it) says nothing of the mounts, and one key for
// every path then would stop the other mounts' reads with the hung one's. With
// no read before, it holds what the failed read gave (none: the volume root).
func newSnapshot(prev *snapshot, read func() ([]Mount, error)) *snapshot {
	list, err := read()
	if err != nil && prev != nil {
		return &snapshot{points: prev.points, read: time.Now()}
	}
	// A mount over another at the same point is listed after it: the last one
	// is the one a path there is on.
	points := make(map[string]string, len(list))
	for _, m := range list {
		point := filepath.Clean(m.Point)
		points[point] = cmp.Or(m.Device, point)
	}
	return &snapshot{points: points, read: time.Now()}
}

// longest is the longest of points that is path or a folder above it, with its
// key, else path's volume root for both: path and its parents looked up in
// turn.
func longest(points map[string]string, path string) (key, point string) {
	// Each parent is a prefix of the clean path, cut at a separator: no
	// filepath.Dir, which cleans every parent again (a look at a library file
	// asks this once, and a no-change scan makes a look per audio file).
	vol := len(filepath.VolumeName(path))
	for p := path; len(p) > vol; {
		if key, ok := points[p]; ok {
			return key, p
		}
		i := strings.LastIndexByte(p[vol:], filepath.Separator) + vol
		switch {
		case i < vol:
			p = "" // a relative path: no parent left to look up
		case i == vol:
			p = p[:i+1] // the volume root, "/" or "C:\"
			if key, ok := points[p]; ok {
				return key, p
			}
			p = ""
		default:
			p = p[:i]
		}
	}
	root := filepath.VolumeName(path) + string(filepath.Separator)
	return root, root
}

// Within reports whether path is dir or under it, by whole segments (/mnt/nas
// holds /mnt/nas/x, never /mnt/nas2).
func Within(path, dir string) bool {
	if path == dir {
		return true
	}
	if !strings.HasPrefix(path, dir) {
		return false
	}
	return strings.HasSuffix(dir, string(filepath.Separator)) || path[len(dir)] == filepath.Separator
}

// PerMount is a value per mount (Of's key: per filesystem mounted), made by its
// newValue when a path on that mount first asks for it and kept: there are only
// as many as the filesystems the mount table has held, or one when it can't be
// read. A share mounted anew has a value of its own, the one before left to the
// reads still held on it.
type PerMount[T any] struct {
	// mount names a path's mount, its key and its mount point: Of, or a test's
	// stand-in (SetMount).
	mount atomic.Pointer[func(path string) (key, point string)]
	// newValue is handed the mount point of the path that first asked (the
	// mount's name, for a log).
	newValue func(point string) T
	by       sync.Map // key -> T
}

// NewPerMount is a PerMount making each mount's value with newValue.
func NewPerMount[T any](newValue func(point string) T) *PerMount[T] {
	m := &PerMount[T]{newValue: newValue}
	m.SetMount(Of)
	return m
}

// SetMount puts mount, a test's stand-in, in place of the lookup naming each
// path's mount (its key and mount point) until restore puts back the one
// before. Swapped atomically, so a read a test left held, still under way when
// the test's cleanup restores the lookup, is no data race: it looks up its
// mount with the stand-in or the restored lookup.
func (m *PerMount[T]) SetMount(mount func(path string) (key, point string)) (restore func()) {
	was := m.mount.Swap(&mount)
	return func() { m.mount.Store(was) }
}

// Mount is the lookup naming each path's mount: Of, or SetMount's stand-in (a
// test's, for one of its own to fall back on).
func (m *PerMount[T]) Mount() func(path string) (key, point string) {
	return *m.mount.Load()
}

// For is the value of the mount path is on.
func (m *PerMount[T]) For(path string) T {
	key, point := m.Mount()(path)
	if v, ok := m.by.Load(key); ok {
		return v.(T)
	}
	v, _ := m.by.LoadOrStore(key, m.newValue(point))
	return v.(T)
}

// Scan calls fn with each mount in r, a /proc/self/mountinfo table
// (ParseLine), skipping lines that aren't a mount.
func Scan(r io.Reader, fn func(Mount)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if m, ok := ParseLine(sc.Text()); ok {
			fn(m)
		}
	}
	return sc.Err()
}

// ParseLine reads one /proc/self/mountinfo line (proc(5)): the device number
// is the third field, the mount point the fifth, octal-escaped, and the
// filesystem type the first field after the " - " separator.
func ParseLine(line string) (m Mount, ok bool) {
	before, after, found := strings.Cut(line, " - ")
	if !found {
		return Mount{}, false
	}
	fields, tail := strings.Fields(before), strings.Fields(after)
	if len(fields) < 5 || len(tail) < 1 {
		return Mount{}, false
	}
	m = Mount{Point: filepath.Clean(unescape(fields[4])), FSType: tail[0], Device: fields[2]}
	m.ReadOnly = (len(fields) > 5 && hasRO(fields[5])) || (len(tail) > 2 && hasRO(tail[2]))
	return m, true
}

func hasRO(options string) bool {
	return strings.Contains(","+options+",", ",ro,")
}

// unescape undoes mountinfo's octal escapes ("\040" is a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Table reads the mount table once, uncached (readTable): nil off Linux and
// macOS, and nil with the error when it can't be read.
func Table() ([]Mount, error) { return readTable() }

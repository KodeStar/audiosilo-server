// Package mounts names the mount a path is on, from the process's mount table
// alone (no filesystem call), so it answers at once even for a path on a share
// that stopped answering. The reads that can hang on such a share (a scan's
// audio reads, the folder picker's listings, the setup preview) are bounded per
// mount with it (PerMount), so one hung share holds back only the reads on it,
// never every library's (server#360).
package mounts

import (
	"bufio"
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

// snapshot is one read of the mount table: its mount points, and when.
type snapshot struct {
	points map[string]struct{}
	read   time.Time
}

// table is the last read of the mount table; refreshing is held by the one
// caller reading it again, the others keeping to the last read meanwhile, so
// a scan's readers never wait on the table.
var (
	table      atomic.Pointer[snapshot]
	refreshing atomic.Bool
)

// Of is the mount point holding path, an absolute path (cleaned here): the
// longest mount point in the mount table that is path or a folder above it, by
// the path's text alone, so a path reached through a symlink into another
// mount is keyed by where the link sits. With no table to read (an OS this
// does not read it on, or a read that failed) it is the path's volume root
// ("/", "C:\"), one key for every path, as a single bound was before.
func Of(path string) string {
	return longest(current(), filepath.Clean(path))
}

// current is the mount table's points, read again once tableTTL old.
func current() map[string]struct{} {
	s := table.Load()
	if s != nil && time.Since(s.read) <= tableTTL {
		return s.points
	}
	if !refreshing.CompareAndSwap(false, true) {
		if s != nil {
			return s.points // another caller is reading it again
		}
		return readSnapshot().points // the first read, raced: read it here too
	}
	defer refreshing.Store(false)
	s = readSnapshot()
	table.Store(s)
	return s.points
}

// readSnapshot reads the mount table (none on a failed read: the volume root).
func readSnapshot() *snapshot {
	list, _ := readTable()
	points := make(map[string]struct{}, len(list))
	for _, p := range list {
		points[filepath.Clean(p)] = struct{}{}
	}
	return &snapshot{points: points, read: time.Now()}
}

// longest is the longest of points that is path or a folder above it, else
// path's volume root: path and its parents looked up in turn.
func longest(points map[string]struct{}, path string) string {
	for p := path; ; {
		if _, ok := points[p]; ok {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return filepath.VolumeName(path) + string(filepath.Separator)
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

// PerMount is a value per mount (Of), made by its newValue when a path on that
// mount first asks for it and kept: there are only as many as the mount table
// has, or one when it can't be read.
type PerMount[T any] struct {
	// Mount names a path's mount: Of, or a test's stand-in.
	Mount    func(path string) string
	newValue func(mount string) T
	by       sync.Map // mount -> T
}

// NewPerMount is a PerMount making each mount's value with newValue.
func NewPerMount[T any](newValue func(mount string) T) *PerMount[T] {
	return &PerMount[T]{Mount: Of, newValue: newValue}
}

// For is the value of the mount path is on.
func (m *PerMount[T]) For(path string) T {
	key := m.Mount(path)
	if v, ok := m.by.Load(key); ok {
		return v.(T)
	}
	v, _ := m.by.LoadOrStore(key, m.newValue(key))
	return v.(T)
}

// Scan calls fn with each mount point and filesystem type in r, a
// /proc/self/mountinfo table (ParseLine), skipping lines that aren't a mount.
func Scan(r io.Reader, fn func(point, fstype string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if point, fstype, ok := ParseLine(sc.Text()); ok {
			fn(point, fstype)
		}
	}
	return sc.Err()
}

// ParseLine reads one /proc/self/mountinfo line (proc(5)): the mount point is
// the fifth field, octal-escaped, and the filesystem type the first field after
// the " - " separator.
func ParseLine(line string) (point, fstype string, ok bool) {
	before, after, found := strings.Cut(line, " - ")
	if !found {
		return "", "", false
	}
	fields, tail := strings.Fields(before), strings.Fields(after)
	if len(fields) < 5 || len(tail) < 1 {
		return "", "", false
	}
	return filepath.Clean(unescape(fields[4])), tail[0], true
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

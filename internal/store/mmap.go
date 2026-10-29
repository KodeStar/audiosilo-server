package store

import (
	"errors"
	"strconv"
	"strings"
)

// The reader pool's memory-mapped I/O (PERF-SPIKE S6). SQLite's mmap_size lets a
// reader take its pages straight from the page cache instead of a pread system
// call per page. On Unraid's /mnt/user (shfs, a FUSE filesystem) each pread is a
// round trip through the FUSE daemon, and mapping the file cut the N305's
// authors list from 78.8 to 8.9 ms, Health from 157 to 60 ms and a 200-book page
// from 9.1 to 4.3 ms, with integrity intact under a concurrent scan
// (.perf-spike/s6-n305.md). On a local disk it gained little (search and a deep
// page were ~10% slower). An I/O error under a mapping is a SIGBUS, a crash in
// pure-Go modernc, where pread returns SQLITE_IOERR: a risk on a network
// filesystem (NFS, SMB/CIFS) and on a FUSE filesystem whose reads can fail
// (sshfs, rclone, s3fs, ntfs-3g). So by default it is on only for a database on
// shfs or mergerfs (a pooling layer over local disks, like shfs), found from the
// mount's filesystem type, and only on the readers: the writer never maps. The
// server never truncates the database in place (backups are VACUUM INTO, a
// restore swaps the file before it opens), which rules out mmap's other SIGBUS,
// a file shrunk under a live mapping.

// Reader mmap modes (WithReaderMmap; config database.mmap).
const (
	MmapAuto = "auto" // on when the database is on shfs or mergerfs (the default)
	MmapOn   = "on"   // always
	MmapOff  = "off"  // never
)

// MmapModes are the modes WithReaderMmap takes, in any case ("" is MmapAuto).
var MmapModes = []string{MmapAuto, MmapOn, MmapOff}

// readerMmapSize is the reader pool's mmap_size when it maps: a cap on address
// space, not memory (the pages are the kernel's file cache, reclaimable).
const readerMmapSize = 256 << 20

// fuseSuperMagic is statfs's f_type of a FUSE filesystem (FUSE_SUPER_MAGIC):
// Unraid's shfs, mergerfs, sshfs and the rest. It is the cheap first check;
// the mount's type in mountinfo says which FUSE filesystem it is.
const fuseSuperMagic = 0x65735546

// isFUSEMagic reports whether a statfs f_type is FUSE's.
func isFUSEMagic(fsType int64) bool { return fsType == fuseSuperMagic }

// WithReaderMmap sets the reader pool's mmap mode: MmapAuto (or ""), MmapOn or
// MmapOff, in any case. Anything else is MmapAuto. An in-memory database has no
// reader pool and never maps.
func WithReaderMmap(mode string) Option {
	return func(c *openConfig) { c.mmap = mode }
}

// withMmapDetector replaces onMappableFUSE (tests: the path can't be made on CI).
func withMmapDetector(f func(path string) bool) Option {
	return func(c *openConfig) { c.mappable = f }
}

// mmapMode is mode as WithReaderMmap reads it: lower case, "" as MmapAuto.
func mmapMode(mode string) string {
	if m := strings.ToLower(mode); m != "" {
		return m
	}
	return MmapAuto
}

// readerMmap is the reader pool's mmap_size for mode, asking mappable (the
// database file is on shfs or mergerfs) only in auto mode.
func readerMmap(mode string, mappable func() bool) int64 {
	switch mmapMode(mode) {
	case MmapOn:
		return readerMmapSize
	case MmapOff:
		return 0
	}
	if mappable() {
		return readerMmapSize
	}
	return 0
}

// mappableFSTypes are the mountinfo filesystem types the auto mode maps on:
// Unraid's shfs (measured) and mergerfs, the same kind of pooling layer over
// local disks.
var mappableFSTypes = map[string]bool{"fuse.shfs": true, "fuse.mergerfs": true}

// mountFSType is the filesystem type of the mount holding path (absolute, with
// symlinks resolved) in a /proc/self/mountinfo: the one whose mount point is the
// longest prefix of path on a segment boundary, the last such line when several
// mounts share the point (the one on top). ok is false for a line it can't read
// (the whole table is then untrusted) or no mount holding path.
func mountFSType(mountinfo, path string) (fsType string, ok bool) {
	best := -1
	for _, line := range strings.Split(mountinfo, "\n") {
		if line == "" {
			continue
		}
		// id parent major:minor root mount-point options [optional...] - type source super-options
		pre, post, found := strings.Cut(line, " - ")
		fields, tail := strings.Fields(pre), strings.Fields(post)
		if !found || len(fields) < 6 || len(tail) < 1 {
			return "", false
		}
		point, err := unescapeMount(fields[4])
		if err != nil || !strings.HasPrefix(point, "/") {
			return "", false
		}
		if !withinMount(path, point) || len(point) < best {
			continue
		}
		best, fsType = len(point), tail[0]
	}
	return fsType, best >= 0
}

// withinMount reports whether path is point or under it.
func withinMount(path, point string) bool {
	if point == "/" {
		return strings.HasPrefix(path, "/")
	}
	rest, ok := strings.CutPrefix(path, point)
	return ok && (rest == "" || rest[0] == '/')
}

// unescapeMount undoes mountinfo's octal escapes in a mount point (\040 for a
// space, \011 tab, \012 newline, \134 backslash).
func unescapeMount(s string) (string, error) {
	if !strings.Contains(s, "\\") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+4 > len(s) {
			return "", errBadMountinfo
		}
		n, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", errBadMountinfo
		}
		b.WriteByte(byte(n))
		i += 3
	}
	return b.String(), nil
}

var errBadMountinfo = errors.New("store: unreadable mountinfo")

// dsnFile is the database file a DSN names: the path without a "file:" scheme
// or query string.
func dsnFile(dsn string) string {
	p := strings.TrimPrefix(dsn, "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

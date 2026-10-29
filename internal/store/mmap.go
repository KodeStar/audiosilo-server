package store

import "strings"

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
// Unraid's shfs (a pooling layer over local disks), found from the
// mount's filesystem type, and only on the readers: the writer never maps. The
// server never truncates the database in place (backups are VACUUM INTO, a
// restore swaps the file before it opens), which rules out mmap's other SIGBUS,
// a file shrunk under a live mapping.

// Reader mmap modes (WithReaderMmap; config database.mmap).
const (
	MmapAuto = "auto" // on when the database is on Unraid's shfs (the default)
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
	return func(c *openConfig) { c.mmap = mmapMode(mode) }
}

// withMmapDetector replaces onMappableFUSE (tests: the path can't be made on CI).
func withMmapDetector(f func(path string) bool) Option {
	return func(c *openConfig) { c.mappable = f }
}

// mmapMode is mode as WithReaderMmap reads it: MmapOn or MmapOff in any case,
// anything else (and "") MmapAuto, so the start log names the mode it ran.
func mmapMode(mode string) string {
	if m := strings.ToLower(mode); m == MmapOn || m == MmapOff {
		return m
	}
	return MmapAuto
}

// readerMmap is the reader pool's mmap_size for mode (as mmapMode reads it),
// asking mappable (the database file is on shfs) only in auto mode.
func readerMmap(mode string, mappable func() bool) int64 {
	switch mode {
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
// Unraid's shfs, the one measured. Not mergerfs: with cache.files=off it runs
// direct_io, where a mapping fails or (on some kernels) can show readers stale
// pages after the writer's direct writes.
var mappableFSTypes = map[string]bool{"fuse.shfs": true}

// mountFSType is the filesystem type of the mount whose device (major:minor,
// mountinfo's third field) is dev in a /proc/self/mountinfo: the kernel's own
// identity for the mounted filesystem, the st_dev of every file on it, so a
// symlinked path, a bind mount, a stacked mount or an escaped mount point needs
// no path matching. Every line of one device names one type. ok is false for a
// line it can't read (the whole table is then untrusted), no line for dev, or
// two lines for dev that disagree.
func mountFSType(mountinfo, dev string) (fsType string, ok bool) {
	for line := range strings.Lines(mountinfo) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			continue
		}
		// id parent major:minor root mount-point options [optional...] - type source super-options
		pre, post, found := strings.Cut(line, " - ")
		fields, tail := strings.Fields(pre), strings.Fields(post)
		if !found || len(fields) < 6 || len(tail) < 1 {
			return "", false
		}
		if fields[2] != dev {
			continue
		}
		if fsType != "" && fsType != tail[0] {
			return "", false
		}
		fsType = tail[0]
	}
	return fsType, fsType != ""
}

// dsnFile is the database file a DSN names: the path without a "file:" scheme
// or query string.
func dsnFile(dsn string) string {
	p := strings.TrimPrefix(dsn, "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

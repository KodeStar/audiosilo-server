package store

import (
	"strings"
)

// The reader pool's memory-mapped I/O (PERF-SPIKE S6). SQLite's mmap_size lets a
// reader take its pages straight from the page cache instead of a pread system
// call per page. On a FUSE data folder (Unraid's /mnt/user shfs, mergerfs) each
// pread is a round trip through the FUSE daemon, and mapping the file cut the
// N305's authors list from 78.8 to 8.9 ms, Health from 157 to 60 ms and a
// 200-book page from 9.1 to 4.3 ms, with integrity intact under a concurrent
// scan (.perf-spike/s6-n305.md). On a local disk it gained little (search and a
// deep page were ~10% slower), and on a network filesystem (NFS, SMB/CIFS) an I/O
// error under a mapping is a SIGBUS, a crash in pure-Go modernc, where pread
// returns SQLITE_IOERR. So by default it is on for a FUSE data folder only, and
// only on the readers: the writer never maps. The server never truncates the
// database in place (backups are VACUUM INTO, a restore swaps the file before it
// opens), which rules out mmap's other SIGBUS, a file shrunk under a live mapping.

// Reader mmap modes (WithReaderMmap; config database.mmap).
const (
	MmapAuto = "auto" // on when the database is on a FUSE filesystem (the default)
	MmapOn   = "on"   // always
	MmapOff  = "off"  // never
)

// MmapModes are the modes WithReaderMmap takes ("" is MmapAuto).
var MmapModes = []string{MmapAuto, MmapOn, MmapOff}

// readerMmapSize is the reader pool's mmap_size when it maps: a cap on address
// space, not memory (the pages are the kernel's file cache, reclaimable).
const readerMmapSize = 256 << 20

// fuseSuperMagic is statfs's f_type of a FUSE filesystem (FUSE_SUPER_MAGIC):
// Unraid's shfs, mergerfs, sshfs and the rest.
const fuseSuperMagic = 0x65735546

// isFUSEMagic reports whether a statfs f_type is FUSE's.
func isFUSEMagic(fsType int64) bool { return fsType == fuseSuperMagic }

// WithReaderMmap sets the reader pool's mmap mode: MmapAuto (or ""), MmapOn or
// MmapOff. Anything else is MmapAuto. An in-memory database has no reader pool
// and never maps.
func WithReaderMmap(mode string) Option {
	return func(c *openConfig) { c.mmap = mode }
}

// withFUSEDetector replaces onFUSE (tests: the FUSE path can't be made on CI).
func withFUSEDetector(f func(path string) bool) Option {
	return func(c *openConfig) { c.onFUSE = f }
}

// readerMmap is the reader pool's mmap_size for mode, asking fuse (the
// database file's filesystem is FUSE) only in auto mode.
func readerMmap(mode string, fuse func() bool) int64 {
	switch mode {
	case MmapOn:
		return readerMmapSize
	case MmapOff:
		return 0
	}
	if fuse() {
		return readerMmapSize
	}
	return 0
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

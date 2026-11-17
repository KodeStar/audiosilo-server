// Package mounts names the mount a path is on, from the process's mount table
// alone (no filesystem call), so it answers at once even for a path on a share
// that stopped answering. The reads that can hang on such a share (a scan's
// audio reads, the folder picker's listings, the setup preview) are bounded per
// mount with it, so one hung share holds back only the reads on it, never every
// library's (server#360).
package mounts

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tableTTL is how long a read of the mount table is used before it is read
// again, so a share mounted while the server runs is told apart soon after.
const tableTTL = 10 * time.Second

// table is the last read of the mount table, read again once it is tableTTL
// old.
var table struct {
	sync.Mutex
	points []string
	read   time.Time
}

// Of is the mount point holding path, an absolute path (cleaned here): the
// longest mount point in the mount table that is path or a folder above it, by
// the path's text alone, so a path reached through a symlink into another
// mount is keyed by where the link sits. With no table to read (an OS this
// does not read it on, or a read that failed) it is the path's volume root
// ("/", "C:\"), one key for every path, as a single bound was before.
func Of(path string) string {
	path = filepath.Clean(path)
	return longest(points(), path)
}

// points is the mount table's mount points, read again when tableTTL old.
func points() []string {
	table.Lock()
	defer table.Unlock()
	if table.read.IsZero() || time.Since(table.read) > tableTTL {
		table.points, _ = readTable() // none on a failed read: the volume root
		table.read = time.Now()
	}
	return table.points
}

// longest is the longest of points that is path or a folder above it, else
// path's volume root.
func longest(points []string, path string) string {
	best := filepath.VolumeName(path) + string(filepath.Separator)
	for _, p := range points {
		if len(p) > len(best) && within(path, p) {
			best = p
		}
	}
	return best
}

// within reports whether path is dir or under it, by whole segments (/mnt/nas
// holds /mnt/nas/x, never /mnt/nas2).
func within(path, dir string) bool {
	if path == dir {
		return true
	}
	if !strings.HasPrefix(path, dir) {
		return false
	}
	return strings.HasSuffix(dir, string(filepath.Separator)) || path[len(dir)] == filepath.Separator
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

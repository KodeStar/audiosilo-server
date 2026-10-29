package store

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// onMappableFUSE reports whether path is on shfs (mappableFSTypes):
// statfs says FUSE first (cheap, and no for every local disk), then the line in
// /proc/self/mountinfo for the file's device (stat's st_dev, which a symlink is
// followed for) names which FUSE filesystem it is. Inside a container both see
// the bind mount's real filesystem (shfs for an Unraid /mnt/user share, btrfs or
// xfs for /mnt/cache). Any failure (statfs, stat, reading or parsing
// mountinfo) says no.
func onMappableFUSE(path string) bool {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return false
	}
	// f_type's type differs by architecture (int64 on amd64/arm64, int32 on arm).
	if !isFUSEMagic(int64(fs.Type)) {
		return false
	}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return false
	}
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	// st_dev's type differs by architecture too (uint32 on mips).
	dev := uint64(st.Dev)
	fsType, ok := mountFSType(string(info), fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev)))
	return ok && mappableFSTypes[fsType]
}

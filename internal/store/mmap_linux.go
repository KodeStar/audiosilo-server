package store

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// onMappableFUSE reports whether path is on shfs or mergerfs (mappableFSTypes):
// statfs says FUSE first (cheap, and no for every local disk), then the mount
// holding the resolved path in /proc/self/mountinfo names which FUSE filesystem
// it is. Inside a container both see the bind mount's real filesystem (shfs for
// an Unraid /mnt/user share, btrfs or xfs for /mnt/cache). Any failure (statfs,
// resolving the path, reading or parsing mountinfo) says no.
func onMappableFUSE(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	// f_type's type differs by architecture (int64 on amd64/arm64, int32 on arm).
	if !isFUSEMagic(int64(st.Type)) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	if resolved, err = filepath.Abs(resolved); err != nil {
		return false
	}
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	fsType, ok := mountFSType(string(info), resolved)
	return ok && mappableFSTypes[fsType]
}

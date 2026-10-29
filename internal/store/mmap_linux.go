package store

import "golang.org/x/sys/unix"

// onFUSE reports whether path is on a FUSE filesystem. Inside a container this
// sees the bind mount's real filesystem (FUSE for an Unraid /mnt/user share,
// btrfs or xfs for /mnt/cache). A failed statfs says no.
func onFUSE(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	// f_type's type differs by architecture (int64 on amd64/arm64, int32 on arm).
	return isFUSEMagic(int64(st.Type))
}

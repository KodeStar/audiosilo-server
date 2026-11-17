package mounts

import (
	"golang.org/x/sys/unix"
)

// readTable reads the mount points getfsstat(2) lists with MNT_NOWAIT, the
// kernel's last-known figures, so a mount that stopped answering is not asked.
func readTable() ([]string, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	buf := make([]unix.Statfs_t, n+4) // room for a mount added meanwhile
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for _, st := range buf[:n] {
		out = append(out, unix.ByteSliceToString(st.Mntonname[:]))
	}
	return out, nil
}

package mounts

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// readTable reads the mounts getfsstat(2) lists with MNT_NOWAIT, the kernel's
// last-known figures, so a mount that stopped answering is not asked; a
// mount's Device is its filesystem ID.
func readTable() ([]Mount, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	buf := make([]unix.Statfs_t, n+4) // room for a mount added meanwhile
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	out := make([]Mount, 0, n)
	for _, st := range buf[:n] {
		out = append(out, Mount{
			Point:  unix.ByteSliceToString(st.Mntonname[:]),
			Device: fmt.Sprintf("%d:%d", st.Fsid.Val[0], st.Fsid.Val[1]),
		})
	}
	return out, nil
}

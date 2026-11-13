//go:build unix

package fsaccess

import (
	"os"
	"syscall"
)

// owner is the uid and gid owning path (known false when it can't be read).
func owner(path string) (uid, gid int, known bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

//go:build unix

package media

import (
	"fmt"
	"os"
	"syscall"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// cacheIdentity is the source's inode number. The device number is left out:
// a btrfs subvolume, a ZFS dataset or a network mount is given a new one at
// each mount, which would make every copy and ETag new after a restart. The
// key's path already scopes the inode to one place.
func cacheIdentity(info os.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Ino != 0 {
		return fmt.Sprintf("%x", st.Ino)
	}
	return ""
}

func statCacheSource(root, abs string) (os.FileInfo, error) {
	return metadata.Below(root, abs, func(r *os.Root, rel string) (os.FileInfo, error) { return r.Stat(rel) })
}

func sameCacheSource(_ *os.File, before, now os.FileInfo) bool { return os.SameFile(before, now) }

//go:build unix

package media

import (
	"fmt"
	"os"
	"syscall"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

func cacheIdentity(info os.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Ino != 0 {
		return fmt.Sprintf("%x:%x", st.Dev, st.Ino)
	}
	return ""
}

func statCacheSource(root, abs string) (os.FileInfo, error) {
	return metadata.Below(root, abs, func(r *os.Root, rel string) (os.FileInfo, error) { return r.Stat(rel) })
}

func sameCacheSource(_ *os.File, before, now os.FileInfo) bool { return os.SameFile(before, now) }

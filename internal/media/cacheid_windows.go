package media

import (
	"fmt"
	"os"
	"syscall"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

type windowsCacheInfo struct {
	os.FileInfo
	id string
}

func cacheIdentity(info os.FileInfo) string {
	if fi, ok := info.(windowsCacheInfo); ok {
		return fi.id
	}
	return ""
}

func windowsFileID(f *os.File) string {
	var id syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &id); err != nil || id.FileIndexHigh == 0 && id.FileIndexLow == 0 {
		return ""
	}
	return fmt.Sprintf("%x:%x:%x", id.VolumeSerialNumber, id.FileIndexHigh, id.FileIndexLow)
}

func statCacheSource(root, abs string) (os.FileInfo, error) {
	f, info, err := metadata.OpenRegularBelow(root, abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return windowsCacheInfo{info, windowsFileID(f)}, nil
}

func sameCacheSource(f *os.File, before, now os.FileInfo) bool {
	if info, ok := before.(windowsCacheInfo); ok {
		if info.id != "" {
			return info.id == windowsFileID(f)
		}
		before = info.FileInfo
	}
	return os.SameFile(before, now)
}

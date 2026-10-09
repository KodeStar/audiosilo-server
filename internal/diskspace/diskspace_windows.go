//go:build windows

package diskspace

import "golang.org/x/sys/windows"

// Of reports the size of the volume holding path and the space free to the
// server on it.
func Of(path string) (total, free uint64, ok bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var avail, size, all uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &size, &all); err != nil {
		return 0, 0, false
	}
	return size, avail, true
}

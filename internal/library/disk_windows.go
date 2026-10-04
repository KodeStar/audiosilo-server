//go:build windows

package library

import "golang.org/x/sys/windows"

// diskSpace reports the size and the space free to the server of the volume
// holding path.
func diskSpace(path string) (total, free uint64, ok bool) {
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

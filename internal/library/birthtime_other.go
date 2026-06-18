//go:build !darwin && !linux

package library

import (
	"os"
	"time"
)

// birthTime has no portable source on other platforms; callers fall back to mtime.
func birthTime(_ string, _ os.FileInfo) (time.Time, bool) {
	return time.Time{}, false
}

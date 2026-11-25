//go:build !linux

package fsaccess

import (
	"errors"
	"time"
)

// ChownTree is the image's root ownership walk, for Linux only.
func ChownTree(string, int, int, time.Duration, func()) error {
	return errors.New("the ownership walk runs on Linux only")
}

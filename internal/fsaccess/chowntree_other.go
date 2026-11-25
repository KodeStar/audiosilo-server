//go:build !linux

package fsaccess

import "errors"

// ChownTree is the image's root ownership walk, for Linux only.
func ChownTree(string, int, int) error {
	return errors.New("the ownership walk runs on Linux only")
}

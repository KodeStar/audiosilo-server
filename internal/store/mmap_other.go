//go:build !linux

package store

// onFUSE is false off Linux: the auto mode maps only on Linux FUSE (measured on
// an Unraid box), so macOS and Windows data folders never map unless asked.
func onFUSE(string) bool { return false }

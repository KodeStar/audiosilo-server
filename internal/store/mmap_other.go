//go:build !linux

package store

// onMappableFUSE is false off Linux: the auto mode maps only on Linux shfs
// (measured on an Unraid box), so macOS and Windows data folders never
// map unless asked.
func onMappableFUSE(string) bool { return false }

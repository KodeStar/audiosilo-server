//go:build !linux && !darwin

package mounts

// readTable reads no table off Linux and macOS: every path is keyed by its
// volume root (Of).
func readTable() ([]Mount, error) { return nil, nil }

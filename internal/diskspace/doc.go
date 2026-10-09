// Package diskspace reads the size and free space of the filesystem holding a
// path, per OS (statfs on unix, GetDiskFreeSpaceEx on Windows): Health >
// System's library roots (internal/library) and the metadata mirror's disk guard
// (internal/metamirror) ask the same question.
package diskspace

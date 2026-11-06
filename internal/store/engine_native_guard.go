//go:build sqlite_native && !(cgo && sqlite_fts5 && sqlite_preupdate_hook)

package store

// A native build needs cgo and mattn's FTS5 and pre-update hook (see
// engine_native.go). Without the hook mattn's RegisterPreUpdateHook silently
// does nothing, and the user-state watch with it, so such a build is stopped
// here rather than run:
//
//	CGO_ENABLED=1 go build -tags "sqlite_native sqlite_fts5 sqlite_preupdate_hook" ./cmd/audiosilo
var _ = sqlite_native_needs_cgo_and_the_tags_sqlite_fts5_and_sqlite_preupdate_hook

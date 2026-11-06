//go:build !sqlite_native

package store

// chooseEngine is the default build's engine: modernc's (pure Go, no cgo).
func chooseEngine() *engine { return moderncEngine }

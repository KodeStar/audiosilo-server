package diskspace

import (
	"path/filepath"
	"testing"
)

func TestOf(t *testing.T) {
	total, free, ok := Of(t.TempDir())
	if !ok || total == 0 || free > total {
		t.Fatalf("Of(temp dir) = %d, %d, %v", total, free, ok)
	}
	if _, _, ok := Of(filepath.Join(t.TempDir(), "missing", "folder")); ok {
		t.Fatal("a path that isn't there must not report a filesystem")
	}
}

package jsonfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type doc struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func TestWriteRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if got, err := Read[doc](path); got != nil || err != nil {
		t.Fatalf("Read of no file = %v, %v; want nil, nil", got, err)
	}
	if err := Write(path, doc{Name: "a", N: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, doc{Name: "b", N: 2}); err != nil {
		t.Fatal(err)
	}
	got, err := Read[doc](path)
	if err != nil || got == nil || *got != (doc{Name: "b", N: 2}) {
		t.Fatalf("Read = %+v, %v", got, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want owner-only", fi.Mode().Perm())
	}
	if _, err := os.Stat(path + TempSuffix); !os.IsNotExist(err) {
		t.Fatalf("the temporary file must be gone: %v", err)
	}
}

// A write that fails leaves the old file as it was, and no temporary file.
func TestWriteFailureKeepsOld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Write(path, doc{Name: "kept"}); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("a value JSON can't encode must fail")
	}
	// A temporary name that can't be created (a folder in its place).
	if err := os.Mkdir(path+TempSuffix, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, doc{Name: "lost"}); err == nil {
		t.Fatal("a write whose temporary file can't be created must fail")
	}
	if err := os.Remove(path + TempSuffix); err != nil {
		t.Fatal(err)
	}
	if got, err := Read[doc](path); err != nil || got.Name != "kept" {
		t.Fatalf("Read = %+v, %v; want the old file", got, err)
	}
}

func TestReadBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read[doc](path); err == nil || !strings.Contains(err.Error(), "state.json") {
		t.Fatalf("Read = %v, want an error naming the file", err)
	}
}

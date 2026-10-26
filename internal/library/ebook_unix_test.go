//go:build unix

package library

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO where an ebook was (swapped in since the scan) is no ebook, answered at
// once: its type is checked before it is opened, as opening one would wait for a
// writer (on a route the request timeout doesn't bound, and in the scan).
func TestOpenEbookFIFO(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "Book.epub"), 0o644); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, _, err := OpenEbook(root, "Book.epub", false)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoEbook) {
			t.Fatalf("OpenEbook of a FIFO: %v, want ErrNoEbook", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenEbook waited on a FIFO")
	}
	if r, ok := checkEbook(root, ebookNamed(root, "", "Book.epub")); !ok || r.Path != "" || r.Sig == "" {
		t.Fatalf("checkEbook of a FIFO = %+v, %v; want none, recorded by its signature", r, ok)
	}
}

// A book's folder that can't be listed or looked into right now (a permission, a
// flaky mount) is no answer about its ebook: Unread, so an index keeps what is
// stored, never "none" (which would take the reader's ebook away).
func TestEbookOnDiskFolderUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any folder")
	}
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "Book")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Book.epub"), epubBytes(t, minimalEpub...), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	for _, c := range []struct {
		name   string
		mode   os.FileMode
		rel    string
		folder bool
	}{
		{"a folder book's folder that won't list", 0o300, "Book", true},
		{"a single-file book's folder that won't be looked into", 0o600, "Book/Book.m4b", false},
	} {
		if err := os.Chmod(dir, c.mode); err != nil {
			t.Fatal(err)
		}
		r := ebookOnDisk(root, c.rel, c.folder)
		if !r.Unread {
			t.Errorf("%s: ebookOnDisk = %+v, want Unread", c.name, r)
		}
		if _, ok := checkEbook(root, r); ok {
			t.Errorf("%s: checkEbook ok, want it kept for the next look", c.name)
		}
	}
}

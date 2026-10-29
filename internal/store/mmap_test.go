package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// migratedFile is a migrated database file, built once (storetest's trick:
// migrating under the race detector costs seconds; storetest can't be imported
// here).
var migratedFile = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "audiosilo-mmaptest")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "template.db")
	db, err := Open(context.Background(), path, WithReaderMmap(MmapOff))
	if err != nil {
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

// migratedPath writes a copy of migratedFile into t's temp dir.
func migratedPath(t *testing.T) string {
	t.Helper()
	b, err := migratedFile()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "audiosilo.db")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writerMmap reads PRAGMA mmap_size on the writer.
func writerMmap(t *testing.T, db *DB) (n int64) {
	t.Helper()
	if err := db.writer.QueryRowContext(context.Background(), "PRAGMA mmap_size").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestReaderMmap: the readers map 256 MiB when the detector says the database is
// on FUSE in auto mode (the default, "" too), or when forced on; never when
// forced off, nor on a disk the detector says isn't FUSE. The writer never maps.
// Every reader connection has it (the pool's DSN), and the detector is asked
// about the database file itself.
func TestReaderMmap(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		mode string
		fuse bool
		want int64
	}{
		{"auto on FUSE", MmapAuto, true, readerMmapSize},
		{"default on FUSE", "", true, readerMmapSize},
		{"auto on a local disk", MmapAuto, false, 0},
		{"default on a local disk", "", false, 0},
		{"on, local disk", MmapOn, false, readerMmapSize},
		{"off on FUSE", MmapOff, true, 0},
		{"unknown is auto", "sometimes", true, readerMmapSize},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := migratedPath(t)
			var asked []string
			detect := func(p string) bool { asked = append(asked, p); return c.fuse }
			db, err := Open(context.Background(), path+"?_txlock=immediate", WithReaderMmap(c.mode), withFUSEDetector(detect))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			// Hold several reader connections at once so more than one is checked.
			ctx := context.Background()
			var conns []int64
			for range 3 {
				conn, err := db.reader.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				var n int64
				if err := conn.QueryRowContext(ctx, "PRAGMA mmap_size").Scan(&n); err != nil {
					t.Fatal(err)
				}
				conns = append(conns, n)
			}
			for i, n := range conns {
				if n != c.want {
					t.Fatalf("reader connection %d mmap_size = %d, want %d", i, n, c.want)
				}
			}
			if w := writerMmap(t, db); w != 0 {
				t.Fatalf("writer mmap_size = %d", w)
			}
			wantAsked := 0
			if c.mode != MmapOn && c.mode != MmapOff {
				wantAsked = 1
			}
			if len(asked) != wantAsked || wantAsked == 1 && asked[0] != path {
				t.Fatalf("detector asked %q (mode %q)", asked, c.mode)
			}
		})
	}
}

// TestReaderMmapMemory: an in-memory database has no reader pool and never asks.
func TestReaderMmapMemory(t *testing.T) {
	t.Parallel()
	db, err := Open(context.Background(), ":memory:", WithReaderMmap(MmapOn),
		withFUSEDetector(func(string) bool { t.Error("detector asked for :memory:"); return true }))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.reader != db.writer {
		t.Fatal("an in-memory database opened a reader pool")
	}
}

// TestFUSEMagic: statfs's FUSE f_type is FUSE; the local and network ones the
// spike saw are not (btrfs, xfs, ext4, tmpfs, NFS, SMB2, CIFS, overlayfs).
func TestFUSEMagic(t *testing.T) {
	t.Parallel()
	if !isFUSEMagic(0x65735546) {
		t.Fatal("FUSE_SUPER_MAGIC not FUSE")
	}
	for _, m := range []int64{0x9123683e, 0x58465342, 0xef53, 0x01021994, 0x6969, 0xfe534d42, 0xff534d42, 0x794c7630, 0} {
		if isFUSEMagic(m) {
			t.Fatalf("%#x read as FUSE", m)
		}
	}
	for dsn, want := range map[string]string{
		"/data/audiosilo.db":                    "/data/audiosilo.db",
		"file:/data/audiosilo.db?mode=rwc":      "/data/audiosilo.db",
		"/data/audiosilo.db?_pragma=foo(1)&x=1": "/data/audiosilo.db",
	} {
		if got := dsnFile(dsn); got != want {
			t.Fatalf("dsnFile(%q) = %q", dsn, got)
		}
	}
}

// TestOnFUSEHere: the real detector says no for this test's temp folder (no CI
// runner keeps it on FUSE) and for a path that doesn't exist.
func TestOnFUSEHere(t *testing.T) {
	t.Parallel()
	if onFUSE(t.TempDir()) {
		t.Skip("the temp folder is on FUSE here")
	}
	if onFUSE(filepath.Join(t.TempDir(), "missing", "audiosilo.db")) {
		t.Fatal("a missing path read as FUSE")
	}
}

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
// on shfs or mergerfs in auto mode (the default, "" too), or when forced on;
// never when forced off, nor on a disk the detector says no for. A mode is read
// in any case. The writer never maps.
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
		{"Auto on FUSE", "Auto", true, readerMmapSize},
		{"AUTO on a local disk", "AUTO", false, 0},
		{"ON, local disk", "ON", false, readerMmapSize},
		{"OFF on FUSE", "OFF", true, 0},
		{"Off on FUSE", "Off", true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := migratedPath(t)
			var asked []string
			detect := func(p string) bool { asked = append(asked, p); return c.fuse }
			db, err := Open(context.Background(), path+"?_txlock=immediate", WithReaderMmap(c.mode), withMmapDetector(detect))
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
			if m := mmapMode(c.mode); m != MmapOn && m != MmapOff {
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
		withMmapDetector(func(string) bool { t.Error("detector asked for :memory:"); return true }))
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

// TestOnMappableFUSEHere: the real detector says no for this test's temp folder
// (no CI runner keeps it on shfs or mergerfs) and for a path that doesn't exist.
func TestOnMappableFUSEHere(t *testing.T) {
	t.Parallel()
	if onMappableFUSE(t.TempDir()) {
		t.Skip("the temp folder is on shfs or mergerfs here")
	}
	if onMappableFUSE(filepath.Join(t.TempDir(), "missing", "audiosilo.db")) {
		t.Fatal("a missing path read as FUSE")
	}
}

// unraidMountinfo is an Unraid container's /proc/self/mountinfo, cut down: the
// overlay root, the data folder bound from /mnt/user (shfs), a library from
// /mnt/cache (btrfs), a mergerfs pool, an sshfs mount, a tmpfs and a mount point
// with a space in it (escaped \040).
const unraidMountinfo = `581 470 0:52 / / rw,relatime master:278 - overlay overlay rw,lowerdir=/var/lib/docker/l
582 581 0:55 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
590 581 0:44 /appdata/audiosilo /config rw,noatime - fuse.shfs shfs rw,user_id=0,group_id=0,allow_other
591 581 0:31 /appdata/audiosilo-cache /cache rw,noatime - btrfs /dev/nvme0n1p1 rw,ssd,space_cache=v2
592 581 0:60 / /pool rw,relatime shared:12 - fuse.mergerfs pool rw,user_id=0,group_id=0
593 592 0:61 / /pool/remote rw,relatime - fuse.sshfs me@host:/srv rw,user_id=0,group_id=0
594 581 0:62 / /scratch rw,nosuid,nodev - tmpfs tmpfs rw,size=65536k
595 581 0:44 /appdata/my\040data /my\040data rw,noatime - fuse.shfs shfs rw
`

// TestMountFSType: the mount holding a path is the longest mount point that is a
// segment-boundary prefix of it, the later of two at one point (the one on top),
// and the auto mode maps on shfs and mergerfs only; a line it can't read makes
// the whole table untrusted (no).
func TestMountFSType(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		path, want string
		mappable   bool
	}{
		{"/config/audiosilo.db", "fuse.shfs", true},
		{"/config", "fuse.shfs", true},
		{"/configs/audiosilo.db", "overlay", false}, // not under /config
		{"/cache/audiosilo.db", "btrfs", false},
		{"/pool/data/audiosilo.db", "fuse.mergerfs", true},
		{"/pool/remote/audiosilo.db", "fuse.sshfs", false}, // nested in the mergerfs pool
		{"/scratch/audiosilo.db", "tmpfs", false},
		{"/data/audiosilo.db", "overlay", false},
		{"/my data/audiosilo.db", "fuse.shfs", true},
	} {
		got, ok := mountFSType(unraidMountinfo, c.path)
		if !ok || got != c.want || mappableFSTypes[got] != c.mappable {
			t.Errorf("%s: %q %v, want %q (mappable %v)", c.path, got, ok, c.want, c.mappable)
		}
	}
	// Two mounts at one point: the later is on top.
	stacked := unraidMountinfo + "596 590 0:70 / /config rw - fuse.sshfs me@host:/x rw\n"
	if got, ok := mountFSType(stacked, "/config/audiosilo.db"); !ok || got != "fuse.sshfs" {
		t.Fatalf("stacked mount: %q %v", got, ok)
	}

	for name, bad := range map[string]string{
		"no separator":       "590 581 0:44 / /config rw,noatime fuse.shfs shfs rw\n",
		"too few fields":     "590 581 / /config - fuse.shfs shfs rw\n",
		"no type":            "590 581 0:44 / /config rw - \n",
		"bad escape":         "590 581 0:44 / /my\\04 rw - fuse.shfs shfs rw\n",
		"relative point":     "590 581 0:44 / config rw - fuse.shfs shfs rw\n",
		"garbage":            "not a mountinfo at all\n",
		"one bad line among": unraidMountinfo + "garbage line\n",
	} {
		if got, ok := mountFSType(bad, "/config/audiosilo.db"); ok || mappableFSTypes[got] {
			t.Errorf("%s: %q %v, want no", name, got, ok)
		}
	}
	if _, ok := mountFSType("", "/config/audiosilo.db"); ok {
		t.Fatal("an empty mountinfo held the path")
	}
}

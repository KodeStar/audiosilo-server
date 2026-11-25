//go:build linux

package fsaccess

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/kodestar/audiosilo-server/internal/mounts"
)

// snapshotDirs are the folders filesystems and NAS systems keep snapshots in
// (snapper, Timeshift, Synology, NetApp, ZFS). They can be part of the data
// folder's own filesystem and mount, but they hold copies, never the data.
var snapshotDirs = map[string]bool{
	".snapshots": true, "@snapshots": true, "#snapshot": true, ".snapshot": true, ".zfs": true,
}

const statMask = unix.STATX_TYPE | unix.STATX_UID | unix.STATX_GID | unix.STATX_MNT_ID

// ChownTree gives uid:gid to everything under dir (not dir itself) that
// another owns. The image's entrypoint runs it as root on the data folder
// before it drops to the server's user, who can write in that folder, so it
// works from open folders, never from names: a link is changed itself and
// never followed, and a folder swapped for a link during the walk is not
// entered. It stays on dir's mount: it skips a mount point in table (except a
// read and write regular file, a mounted config.yaml), a mount that appears
// on the way (an NFS export's child filesystem), an automount point (never
// triggered), a folder on another filesystem type and the snapshot folders.
// A btrfs subvolume of the data folder is part of it and is walked. Without
// the mount table the walk does not start, as its edges would be unknown.
func ChownTree(dir string, uid, gid int) error {
	table, err := mounts.Table()
	if err != nil {
		return fmt.Errorf("read the mount table: %w", err)
	}
	return chownTree(dir, uid, gid, table)
}

func chownTree(dir string, uid, gid int, table []mounts.Mount) error {
	if len(table) == 0 {
		return errors.New("no mount table to bound the walk")
	}
	w := walker{
		uid: uid, gid: gid,
		chownAt: func(dirfd int, name string) error {
			return unix.Fchownat(dirfd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW)
		},
		chownFd: func(fd int) error { return unix.Fchown(fd, uid, gid) },
	}
	return w.run(dir, table)
}

type walker struct {
	uid, gid int
	chownAt  func(dirfd int, name string) error
	chownFd  func(fd int) error

	points  map[string]bool // mount points under the root: whether read-only
	fsType  int64           // the root's filesystem type
	mountID uint64          // the root's mount id, when the kernel tells (known)
	known   bool
	errs    []error
}

func (w *walker) run(dir string, table []mounts.Mount) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	// The physical path, as the mount table names it (--data /alias, /data/.).
	root, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil || root == "/" || !filepath.IsAbs(root) {
		_ = unix.Close(fd)
		return fmt.Errorf("resolve %s: not a folder to walk (%v)", dir, err)
	}
	var fsStat unix.Statfs_t
	var st unix.Statx_t
	if err := unix.Fstatfs(fd, &fsStat); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("statfs %s: %w", dir, err)
	}
	if err := statAt(fd, "", unix.AT_EMPTY_PATH, &st); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	w.fsType = int64(fsStat.Type)
	w.mountID, w.known = st.Mnt_id, st.Mask&unix.STATX_MNT_ID != 0
	// Later records at the same point describe the visible mount.
	w.points = map[string]bool{}
	for _, m := range table {
		if m.Point != root && mounts.Within(m.Point, root) {
			w.points[m.Point] = m.ReadOnly
		}
	}
	w.walk(fd, root, &st)
	return errors.Join(w.errs...)
}

// walk changes the owner of the entries in the open folder fd (at path, its
// stat st) and walks the folders among them. It closes fd.
func (w *walker) walk(fd int, path string, st *unix.Statx_t) {
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		w.errs = append(w.errs, err)
	}
	for _, name := range names {
		w.entry(fd, path, st, name)
	}
}

func (w *walker) entry(dirfd int, dir string, parent *unix.Statx_t, name string) {
	path := dir + "/" + name
	var st unix.Statx_t
	// AT_NO_AUTOMOUNT: looking at an automount point never mounts it.
	if err := statAt(dirfd, name, unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT, &st); err != nil {
		if !errors.Is(err, unix.ENOENT) { // gone since the listing
			w.errs = append(w.errs, fmt.Errorf("stat %s: %w", path, err))
		}
		return
	}
	kind := uint32(st.Mode) & unix.S_IFMT
	if ro, mounted := w.points[path]; mounted {
		if kind == unix.S_IFREG && !ro {
			w.chown(&st, path, func() error { return w.chownAt(dirfd, name) })
		}
		return
	}
	if w.elsewhere(&st, parent) {
		return
	}
	if kind != unix.S_IFDIR {
		w.chown(&st, path, func() error { return w.chownAt(dirfd, name) })
		return
	}
	if snapshotDirs[name] {
		return
	}
	// O_NOFOLLOW: a folder swapped for a link since the stat is not entered.
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		w.errs = append(w.errs, fmt.Errorf("open %s: %w", path, err))
		return
	}
	// What was opened, which may not be what was looked at.
	var fsStat unix.Statfs_t
	var opened unix.Statx_t
	if unix.Fstatfs(fd, &fsStat) != nil || int64(fsStat.Type) != w.fsType ||
		statAt(fd, "", unix.AT_EMPTY_PATH, &opened) != nil || w.elsewhere(&opened, parent) {
		_ = unix.Close(fd)
		return
	}
	w.chown(&opened, path, func() error { return w.chownFd(fd) })
	w.walk(fd, path, &opened)
}

// elsewhere reports an entry that is not on the root's mount: the root of a
// mount (one made after the table was read), an automount point, or another
// mount by its id. A btrfs subvolume has its own device number but the same
// mount, so it is walked. A kernel older than 5.8 tells no mount id: there a
// device number other than the parent folder's is another mount, unless the
// filesystem is btrfs.
func (w *walker) elsewhere(st, parent *unix.Statx_t) bool {
	attrs := st.Attributes & st.Attributes_mask
	if attrs&(unix.STATX_ATTR_MOUNT_ROOT|unix.STATX_ATTR_AUTOMOUNT) != 0 {
		return true
	}
	if w.known && st.Mask&unix.STATX_MNT_ID != 0 {
		return st.Mnt_id != w.mountID
	}
	otherDev := st.Dev_major != parent.Dev_major || st.Dev_minor != parent.Dev_minor
	return otherDev && w.fsType != unix.BTRFS_SUPER_MAGIC
}

func (w *walker) chown(st *unix.Statx_t, path string, change func() error) {
	if int(st.Uid) == w.uid && int(st.Gid) == w.gid {
		return
	}
	if err := change(); err != nil {
		w.errs = append(w.errs, fmt.Errorf("chown %s: %w", path, err))
	}
}

// statAt is statx, or fstatat on a kernel without it (before 4.11), whose
// answer carries no mount id and no attributes.
func statAt(dirfd int, name string, flags int, st *unix.Statx_t) error {
	err := unix.Statx(dirfd, name, flags, statMask, st)
	if !errors.Is(err, unix.ENOSYS) {
		return err
	}
	var s unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &s, flags); err != nil {
		return err
	}
	dev := uint64(s.Dev)
	*st = unix.Statx_t{
		Mask: unix.STATX_TYPE | unix.STATX_UID | unix.STATX_GID,
		Mode: uint16(s.Mode), Uid: s.Uid, Gid: s.Gid,
		Dev_major: unix.Major(dev), Dev_minor: unix.Minor(dev),
	}
	return nil
}

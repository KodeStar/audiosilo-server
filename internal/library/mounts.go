package library

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/mounts"
)

// The setup flow's folder hints: the folders mounted into the server's
// filesystem that may hold books (a Docker volume, a NAS share), offered as
// suggestions beside the typed path. Linux only, read from /proc/self/mountinfo,
// so in a container they are the container's paths, which is what a library
// root has to be.

// maxMountHints caps the hints.
const maxMountHints = 20

// pseudoFS are the filesystem types that never hold books.
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true, "ramfs": true,
	"cgroup": true, "cgroup2": true, "mqueue": true, "securityfs": true, "debugfs": true,
	"tracefs": true, "pstore": true, "bpf": true, "fusectl": true, "configfs": true,
	"hugetlbfs": true, "autofs": true, "binfmt_misc": true, "nsfs": true, "rpc_pipefs": true,
	"efivarfs": true, "selinuxfs": true, "squashfs": true, "overlay": true,
}

// MountHints are the mount points that may hold books (mountHints), [] off
// Linux or when the mount table can't be read. exclude are folders never to
// offer (the server's data folder).
func MountHints(exclude ...string) []string {
	if runtime.GOOS != "linux" {
		return []string{}
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return []string{}
	}
	defer f.Close()
	return mountHints(f, exclude, func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && info.IsDir()
	})
}

// mountHints reads a mountinfo table: the mount points that are folders
// (isDir), not a system path (systemPath), not a pseudo filesystem and not
// excluded, sorted, without repeats, at most maxMountHints.
func mountHints(r io.Reader, exclude []string, isDir func(string) bool) []string {
	out := []string{}
	_ = mounts.Scan(r, func(m mounts.Mount) { // a table cut short offers what it read
		point := m.Point
		if pseudoFS[m.FSType] || systemPath(point) || slices.Contains(out, point) {
			return
		}
		if slices.ContainsFunc(exclude, func(e string) bool { return e != "" && filepath.Clean(e) == point }) {
			return
		}
		if isDir(point) {
			out = append(out, point)
		}
	})
	slices.Sort(out)
	if len(out) > maxMountHints {
		out = out[:maxMountHints]
	}
	return out
}

// systemPath reports whether a mount point belongs to the system rather than to
// anyone's books: the root itself, /proc, /sys, /tmp, anything under /dev, /run,
// /etc, /boot or /snap (Docker binds /etc/hosts and friends). Under is by whole
// segments: /devices or /snapshots is someone's folder.
func systemPath(p string) bool {
	switch p {
	case "/", "/proc", "/sys", "/tmp", "/dev", "/run", "/boot", "/snap":
		return true
	}
	for _, prefix := range []string{"/proc/", "/sys/", "/dev/", "/run/", "/etc/", "/boot/", "/snap/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

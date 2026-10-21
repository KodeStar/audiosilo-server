package library

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
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
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		point, fstype, ok := parseMountLine(sc.Text())
		if !ok || pseudoFS[fstype] || systemPath(point) || slices.Contains(out, point) {
			continue
		}
		if slices.ContainsFunc(exclude, func(e string) bool { return e != "" && filepath.Clean(e) == point }) {
			continue
		}
		if isDir(point) {
			out = append(out, point)
		}
	}
	slices.Sort(out)
	if len(out) > maxMountHints {
		out = out[:maxMountHints]
	}
	return out
}

// parseMountLine reads one mountinfo line (proc(5)): the mount point is the fifth
// field, octal-escaped, and the filesystem type the first field after the " - "
// separator.
func parseMountLine(line string) (point, fstype string, ok bool) {
	before, after, found := strings.Cut(line, " - ")
	if !found {
		return "", "", false
	}
	fields, tail := strings.Fields(before), strings.Fields(after)
	if len(fields) < 5 || len(tail) < 1 {
		return "", "", false
	}
	return filepath.Clean(unescapeMount(fields[4])), tail[0], true
}

// unescapeMount undoes mountinfo's octal escapes ("\040" is a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// systemPath reports whether a mount point belongs to the system rather than to
// anyone's books: the root itself, /proc, /sys, /tmp, anything under /dev, /run,
// /etc, /boot or /snap (Docker binds /etc/hosts and friends).
func systemPath(p string) bool {
	switch p {
	case "/", "/proc", "/sys", "/tmp":
		return true
	}
	for _, prefix := range []string{"/proc/", "/sys/", "/dev", "/run", "/etc/", "/boot", "/snap"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

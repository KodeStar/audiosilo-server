package mounts

import "strings"

// pseudoFS are the filesystem types that never hold books.
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true, "ramfs": true,
	"cgroup": true, "cgroup2": true, "mqueue": true, "securityfs": true, "debugfs": true,
	"tracefs": true, "pstore": true, "bpf": true, "fusectl": true, "configfs": true,
	"hugetlbfs": true, "autofs": true, "binfmt_misc": true, "nsfs": true, "rpc_pipefs": true,
	"efivarfs": true, "selinuxfs": true, "squashfs": true, "overlay": true,
}

// ContentPath reports whether this mount could hold user content, such as a
// library or a mounted configuration file.
func (m Mount) ContentPath() bool {
	return !pseudoFS[m.FSType] && !SystemPath(m.Point)
}

// SystemPath reports whether a mount point belongs to the system rather than to
// anyone's books: the root itself, /proc, /sys, /tmp, anything under /dev, /run,
// /etc, /boot or /snap (Docker binds /etc/hosts and friends). Under is by whole
// segments: /devices or /snapshots is someone's folder.
func SystemPath(p string) bool {
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

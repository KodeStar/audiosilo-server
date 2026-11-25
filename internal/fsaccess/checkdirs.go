package fsaccess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/kodestar/audiosilo-server/internal/mounts"
)

// DirCheckTimeout bounds a startup lookup on a potentially unresponsive mount.
const DirCheckTimeout = 5 * time.Second

// CheckDirs performs the image's startup checks as the server user. A warning
// is advisory. The launcher decides whether it can start with the given data.
func CheckDirs(dataDir, backups string, dataChownFailed, backupChownFailed bool, out io.Writer) {
	// The image's mount table only: on a desktop OS every mounted volume (a
	// network share among them) would be opened by a dev run or a test.
	var table []mounts.Mount
	if runtime.GOOS == "linux" {
		table, _ = mounts.Table() // a table cut short checks what it read
	}
	checkDirs(dataDir, backups, dataChownFailed, backupChownFailed, table, out, DirCheckTimeout)
}

func checkDirs(dataDir, backups string, dataFailed, backupFailed bool, table []mounts.Mount, out io.Writer, limit time.Duration) {
	data, err := filepath.Abs(dataDir)
	if err != nil {
		_, _ = fmt.Fprintln(out, "audiosilo: WARNING:", err)
		return
	}
	if backups == "" {
		backups = filepath.Join(data, "backups")
	} else if !filepath.IsAbs(backups) || strings.Contains(filepath.ToSlash(backups)+"/", "/../") {
		backups = ""
	} else {
		backups = filepath.Clean(backups)
	}

	// Later records at the same point describe the visible mount.
	visible := make(map[string]mounts.Mount, len(table))
	for _, m := range table {
		visible[m.Point] = m
	}
	var checks []check
	write := func(path string, m mounts.Mount, chownFailed bool) {
		checks = append(checks, startCheck(path, func() string { return writeProblem(path, m.ReadOnly, chownFailed) }))
	}
	dataMount := containingMount(data, visible)
	if networkFSTypes[dataMount.FSType] {
		_, _ = fmt.Fprintf(out, "audiosilo: WARNING: The data folder %s is on a network share (%s). Keep it on a local disk if you can: the server's database needs file locking a share may not give (an SMB share needs the nobrl mount option), and the server can crash when the share stops answering.\n", data, dataMount.FSType)
	}
	write(data, dataMount, dataFailed)
	if backups != "" && backups != data {
		write(backups, containingMount(backups, visible), backupFailed)
	}
	for _, path := range slices.Sorted(maps.Keys(visible)) {
		if path == data || path == backups || mounts.SystemPath(path) {
			continue
		}
		// A nested directory is another app's mount, but an unreadable
		// mounted config file matters even inside /data or on tmpfs.
		filesOnly := mounts.Within(path, data) || !visible[path].ContentPath()
		checks = append(checks, startCheck(path, func() string { return readMount(path, filesOnly) }))
	}
	report(out, checks, limit)
}

// networkFSTypes are the mountinfo types of a network share, the data folder's
// notice. Not every FUSE type: Unraid's shfs and mergerfs pool local disks.
var networkFSTypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true, "ceph": true,
	"fuse.sshfs": true, "fuse.rclone": true, "fuse.s3fs": true, "fuse.gcsfuse": true, "fuse.glusterfs": true,
	"fuse.ceph-fuse": true, "fuse.juicefs": true,
}

// containingMount is the visible mount path is on, by its name: the nearest
// mount point at or above it (the zero Mount when none is).
func containingMount(path string, table map[string]mounts.Mount) mounts.Mount {
	for p := path; ; p = filepath.Dir(p) {
		if m, ok := table[p]; ok {
			return m
		}
		if p == filepath.Dir(p) {
			return mounts.Mount{}
		}
	}
}

func writeProblem(path string, readOnly, chownFailed bool) string {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "" // the server creates it on first use
	}
	if err == nil {
		root, statErr := os.Stat(filepath.VolumeName(path) + string(filepath.Separator))
		if statErr == nil && os.SameFile(info, root) {
			return "" // also a link to /
		}
		err = CheckWrite(path)
	}
	if err == nil {
		return ""
	}
	if IsPermission(err) && readOnly {
		err = syscall.EROFS // the mode can refuse the call before EROFS
	}
	msg := Wrap(path, true, err).Error()
	if chownFailed && IsPermission(err) && !isReadOnly(err) {
		msg += " Changing its owner to " + User() + " failed."
	}
	return msg
}

func readMount(path string, filesOnly bool) string {
	info, err := os.Stat(path)
	if err != nil {
		return Wrap(path, false, err).Error()
	}
	switch {
	case info.Mode().IsRegular():
		f, err := os.Open(path)
		if err != nil {
			return Wrap(path, false, err).Error()
		}
		_ = f.Close()
	case !filesOnly && info.IsDir():
		if err := CheckRead(path); err != nil {
			return err.Error() + " Until then, a library in that folder shows as offline."
		}
	}
	return ""
}

// check is one folder's probe, running on a goroutine of its own.
type check struct {
	path string
	done chan string // the warning ("" for none), sent once
}

func startCheck(path string, probe func() string) check {
	c := check{path: path, done: make(chan string, 1)}
	go func() { c.done <- probe() }()
	return c
}

// report prints the checks' warnings in order. A hard mount can hold any stat,
// open, read, write, removal or owner lookup, so the checks run at once and
// share one limit: several dead shares hold the start for limit, not limit
// each. Only report prints: a late result cannot print a second warning or hold
// the server start. The short-lived checkdirs process exits after the checks.
func report(out io.Writer, checks []check, limit time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	for _, c := range checks {
		message, answered := "", true
		select {
		case message = <-c.done: // an answer already in wins over a passed limit
		default:
			select {
			case message = <-c.done:
			case <-ctx.Done():
				answered = false
			}
		}
		switch {
		case !answered:
			_, _ = fmt.Fprintf(out, "audiosilo: WARNING: %s didn't answer within %v. Check the network share.\n", c.path, limit)
		case message != "":
			_, _ = fmt.Fprintln(out, "audiosilo: WARNING:", message)
		}
	}
}

package fsaccess

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
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
	var table []mounts.Mount
	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		_ = mounts.Scan(f, func(m mounts.Mount) { table = append(table, m) })
		_ = f.Close()
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
	write := func(path string, chownFailed bool) {
		ro := onReadOnlyMount(path, visible)
		checkFolder(out, path, limit, func() string { return writeProblem(path, ro, chownFailed) })
	}
	write(data, dataFailed)
	if backups != "" && backups != data {
		write(backups, backupFailed)
	}
	for _, path := range slices.Sorted(maps.Keys(visible)) {
		if path == data || path == backups || mounts.SystemPath(path) {
			continue
		}
		// A nested directory is another app's mount, but an unreadable
		// mounted config file matters even inside /data or on tmpfs.
		filesOnly := mounts.Within(path, data) || !visible[path].ContentPath()
		checkFolder(out, path, limit, func() string { return readMount(path, filesOnly) })
	}
}

func onReadOnlyMount(path string, table map[string]mounts.Mount) bool {
	for p := path; ; p = filepath.Dir(p) {
		if m, ok := table[p]; ok {
			return m.ReadOnly
		}
		if p == filepath.Dir(p) {
			return false
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

// A hard mount can hold any stat, open, read, write, removal or owner lookup.
// Only the caller prints: a late result cannot print a second warning or hold
// the server start. The short-lived checkdirs process exits after the checks.
func checkFolder(out io.Writer, path string, limit time.Duration, probe func() string) {
	done := make(chan string, 1)
	go func() { done <- probe() }()
	select {
	case message := <-done:
		if message != "" {
			_, _ = fmt.Fprintln(out, "audiosilo: WARNING:", message)
		}
	case <-time.After(limit):
		_, _ = fmt.Fprintf(out, "audiosilo: WARNING: %s didn't answer within %v. Check the network share.\n", path, limit)
	}
}

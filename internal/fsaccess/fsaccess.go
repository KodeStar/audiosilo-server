// Package fsaccess says why the server can't use a folder it needs: the path,
// the user the server runs as, who owns the folder and the fix (server#368). The
// data folder's start-up check, the backups folder and a library folder the
// server can't read share these words, as the Docker entrypoint's warnings do.
package fsaccess

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// EnvStartedAsUser is set to "1" by the Docker entrypoint when the container was
// started as a user of its own (docker --user, compose user:) rather than as root
// dropping to PUID:PGID, so the fix names that setting, not PUID and PGID.
const EnvStartedAsUser = "AUDIOSILO_STARTED_AS_USER"

// Test seams: who the server runs as, who owns a path and whether it runs in a
// container.
var (
	running = func() (uid, gid int) { return os.Getuid(), os.Getgid() }
	ownerOf = owner
	inCtr   = sync.OnceValue(func() bool {
		return exists("/.dockerenv") || exists("/run/.containerenv")
	})
)

// InContainer reports whether the server runs in a container (Docker, Podman),
// where the fix for an owner mismatch is the image's PUID and PGID.
func InContainer() bool { return inCtr() }

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Error is a folder (or a file in one) the server can't read or write. Its
// message names the path, the user the server runs as, the folder's owner and
// the fix when the system refused permission, or says to mount it read and
// write when it is on a read-only mount.
type Error struct {
	Path   string    // the folder or file refused
	Write  bool      // writing was refused (else reading)
	Err    error     // the system's error
	Access *Identity // for permission failures
	hint   string    // captured during the check, so formatting never stats a dead share
}

func (e *Error) Unwrap() error { return e.Err }

func (e *Error) Error() string {
	verb := "read"
	if e.Write {
		verb = "write to"
	}
	msg := fmt.Sprintf("the server can't %s %s: %s", verb, e.Path, Cause(e.Err))
	switch {
	case isReadOnly(e.Err):
		msg += ". " + readOnlyHint
	case errors.Is(e.Err, fs.ErrPermission):
		msg += ". " + e.hint
	}
	return msg
}

// Cause is the system's own words for err without the absolute paths a PathError
// or a LinkError (a rename) names ("permission denied", not "open
// /srv/books/x.mp3: permission denied"): the caller names the path its own way.
func Cause(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err.Error()
	}
	return err.Error()
}

// Wrap words err, a failure to read (write false) or write path, as an *Error;
// nil stays nil and an *Error is returned as it is.
func Wrap(path string, write bool, err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	failure := &Error{Path: path, Write: write, Err: err}
	if errors.Is(err, fs.ErrPermission) {
		info, _ := os.Stat(path)
		failure.Access = IdentityOf(info)
		failure.hint = hint(info, write, failure.Access)
	}
	return failure
}

// IsPermission reports whether err is the system refusing the server: permission
// denied, or a write to a read-only mount (docker's :ro), which no owner or mode
// can fix but is the same "the folder refuses the server" to the console.
func IsPermission(err error) bool { return errors.Is(err, fs.ErrPermission) || isReadOnly(err) }

// isReadOnly reports whether err is a write refused by a read-only mount.
func isReadOnly(err error) bool { return errors.Is(err, syscall.EROFS) }

// readOnlyHint is the fix for a folder on a read-only mount.
const readOnlyHint = "It is on a read-only mount: mount it read and write (in Docker, without :ro)."

// User is who the server runs as, "uid:gid" ("" where the system has no such
// ids: Windows).
func User() string {
	uid, gid := running()
	if uid < 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", uid, gid)
}

// Identity is the server account and the owner of the folder it checked, in UID:GID form.
type Identity struct {
	ServerUser string `json:"server_user"`
	Owner      string `json:"owner,omitempty"`
}

// IdentityOf uses the stat from the bounded folder check. When the folder
// could not be reached, the server's own identity is still known on Unix.
func IdentityOf(info fs.FileInfo) *Identity {
	user := User()
	if user == "" {
		return nil
	}
	id := &Identity{ServerUser: user}
	if uid, gid, ok := ownerOf(info); ok {
		id.Owner = fmt.Sprintf("%d:%d", uid, gid)
	}
	return id
}

// Description names the accounts for a folder or file, when the OS supplies them.
func (id *Identity) Description(noun string) string {
	if id == nil {
		return ""
	}
	text := "The server runs as user " + id.ServerUser
	if id.Owner != "" {
		text += " and the " + noun + " belongs to " + id.Owner
	}
	return text + "."
}

// Hint is the fix for a folder (or file) at path the server isn't allowed to
// read or write: who the server runs as, who owns it, and, in a container, PUID
// and PGID set to the owner (or, in one started with --user, the owner as that
// user, as the entrypoint says). The path itself is the caller's to name (Error
// does).
func Hint(path string, write bool) string {
	info, _ := os.Stat(path)
	return hint(info, write, IdentityOf(info))
}

func hint(info fs.FileInfo, write bool, id *Identity) string {
	access := "read"
	if write {
		access = "read and write"
	}
	noun := "folder"
	if info != nil && !info.IsDir() {
		noun = "file"
	}
	if id == nil {
		return fmt.Sprintf("Give the account the server runs as %s access to the %s.", access, noun)
	}
	user := id.ServerUser
	give := fmt.Sprintf("give user %s %s access to the %s", user, access, noun)
	giveFirst := "G" + give[1:] // the clause opening a sentence
	var b strings.Builder
	b.WriteString(id.Description(noun))
	b.WriteByte(' ')
	ouid, ogid, known := strings.Cut(id.Owner, ":")
	uid, _, _ := strings.Cut(user, ":")
	switch {
	case known && ouid == uid:
		// The right owner: the mode refuses it.
		fmt.Fprintf(&b, "%s (its permissions refuse it).", giveFirst)
	case known && ouid == "0":
		// Never advise running the server as root.
		fmt.Fprintf(&b, "%s, for example by making %s its owner on the host.", giveFirst, user)
	case InContainer():
		asUser := os.Getenv(EnvStartedAsUser) == "1"
		switch {
		case asUser && known:
			fmt.Fprintf(&b, "Run the container as the %s's owner (user: \"%s:%s\"), or %s.", noun, ouid, ogid, give)
		case asUser:
			fmt.Fprintf(&b, "Run the container as the %s's owner (user: in compose, --user with docker run), or %s.", noun, give)
		case known:
			fmt.Fprintf(&b, "Set PUID=%s and PGID=%s (the %s's owner) on the container, or %s.", ouid, ogid, noun, give)
		default:
			fmt.Fprintf(&b, "Set PUID and PGID on the container to the %s's owner, or %s.", noun, give)
		}
	default:
		fmt.Fprintf(&b, "Run the server as the %s's owner, or %s.", noun, give)
	}
	return b.String()
}

// writeCheck is the file CheckWrite creates and removes.
const writeCheck = ".audiosilo-write-check-*"

// CheckWrite reports whether the server can make a new file in dir (one is
// created and removed) and open each of files that exists there for reading and
// writing: nil, or an *Error naming the folder or the file refused. A folder
// that doesn't exist is reported as such (callers create it first).
func CheckWrite(dir string, files ...string) error {
	f, err := os.CreateTemp(dir, writeCheck)
	if err != nil {
		return Wrap(dir, true, err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return Wrap(dir, true, err)
	}
	for _, n := range files {
		p := filepath.Join(dir, n)
		if _, err := os.Lstat(p); err != nil {
			continue // not there yet: the folder's check covers making it
		}
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return Wrap(p, true, err)
		}
		_ = f.Close()
	}
	return nil
}

// CheckRead reports whether the server can list dir: nil, or an *Error.
func CheckRead(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return Wrap(dir, false, err)
	}
	defer func() { _ = f.Close() }() // read-only
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return Wrap(dir, false, err)
	}
	return nil
}

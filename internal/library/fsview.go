// Package library provides the filesystem view (instant directory browsing) and
// the background scanner that builds the computed/hybrid index from file
// metadata. The filesystem view requires no prior indexing, so a freshly
// connected client can browse immediately.
package library

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// ErrOutsideRoot is returned when a requested path escapes the library root.
var ErrOutsideRoot = errors.New("path escapes library root")

// ErrHidden is a path through a hidden folder, or naming a hidden file
// (ContentPath). It is fs.ErrNotExist and ErrNotIndexable both: such a path
// reads everywhere as one with nothing there.
var ErrHidden error = hiddenPathError{}

type hiddenPathError struct{}

func (hiddenPathError) Error() string { return "the path is in a hidden folder" }

func (hiddenPathError) Is(target error) bool {
	return target == fs.ErrNotExist || target == ErrNotIndexable
}

// ContentPath checks that a library-relative path may name content: none of its
// segments hidden (ErrHidden otherwise). The one statement of the rule: the scan
// never walks into a hidden folder and /fs never lists one, so no route reaches
// through one by name either, whether it lists a folder, indexes a book or
// serves a file (the Bridge's .audiosilo/ attachments, a share's .Trash). A
// request for "Book/.audiosilo/x.m4b" would otherwise index a phantom book the
// next full scan removes again, or stream a file no listing shows.
func ContentPath(relPath string) error {
	if hasHiddenPart(relPath) {
		return fmt.Errorf("%q: %w", relPath, ErrHidden)
	}
	return nil
}

// ContentJoin is SafeJoin for a path that names content: ContentPath's rule
// first, so a hidden path is refused before anything on disk is looked at.
func ContentJoin(root, relPath string) (string, error) {
	if err := ContentPath(relPath); err != nil {
		return "", err
	}
	return SafeJoin(root, relPath)
}

// ContentJoinBounded is ContentJoin as a metadata.Bounded call (ctx, at most
// timeout, on the joined path's mount), as SafeJoinBounded.
func ContentJoinBounded(ctx context.Context, root, relPath string, timeout time.Duration) (string, error) {
	if err := ContentPath(relPath); err != nil {
		return "", err
	}
	return SafeJoinBounded(ctx, root, relPath, timeout)
}

// SafeJoinBounded is SafeJoin as one metadata.Bounded call (ctx, at most
// timeout, counted on the joined path's mount): its Lstats of the names below
// the root, on a share that stopped answering, are given up on
// (metadata.GaveUp) rather than holding the caller for good.
func SafeJoinBounded(ctx context.Context, root, relPath string, timeout time.Duration) (string, error) {
	at := filepath.Join(root, filepath.FromSlash(relPath))
	return metadata.Bounded(ctx, at, timeout, func() (string, error) { return SafeJoin(root, relPath) }, nil)
}

// Entry is one item in a directory listing. The Book* fields are populated by
// the API layer (annotateWithBooks) when the entry resolves to an indexed book
// under the current per-folder detection (there is no library-wide layout). A
// browsing client acts on the entry by its Path (?path=), never a book id.
type Entry struct {
	Name    string `json:"name"`
	Path    string `json:"path"` // path relative to the library root, slash-separated
	IsDir   bool   `json:"is_dir"`
	IsAudio bool   `json:"is_audio"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`

	// Indexed-book annotations (the hybrid view). Act on a book by its Path.
	IsBook      bool    `json:"is_book,omitempty"`
	Title       string  `json:"title,omitempty"`
	Author      string  `json:"author,omitempty"`
	Series      string  `json:"series,omitempty"`
	SeriesIndex float64 `json:"series_index,omitempty"`
	// SeriesIndexEnd is the end of a range position (an omnibus of 1-3), as on a Book.
	SeriesIndexEnd float64 `json:"series_index_end,omitempty"`
	Duration       float64 `json:"duration,omitempty"`

	// Override is the explicit per-folder detection override set for this
	// directory ("book" or "collection"), empty when auto-detected. Surfaced so
	// the admin console can show and toggle it.
	Override string `json:"override,omitempty"`

	// SplitDiscs marks a folder whose audio is only in disc folders directly in it
	// (CD1, CD2...), each indexed as its own book: a `book` override joins them into
	// one (see discSets). Surfaced so the admin console offers that only here; set
	// for an admin's request only (annotateWithBooks), never on a player's listing.
	SplitDiscs bool `json:"split_discs,omitempty"`

	// PendingBook marks a folder the scanner would read as a book that the index
	// doesn't hold yet (MarkPendingBooks, #193), so a player opens it as a book
	// (indexed on demand) rather than as a folder. Never set with IsBook, nor in a
	// podcast library's listing; absent (not false) otherwise, and an older server
	// never sends it.
	PendingBook bool `json:"pending_book,omitempty"`
}

// Listing is a page of directory entries.
type Listing struct {
	Path       string  `json:"path"`
	Entries    []Entry `json:"entries"`
	Total      int     `json:"total"`
	Offset     int     `json:"offset"`
	NextOffset int     `json:"next_offset,omitempty"`
}

// SafeJoin resolves relPath against root, guaranteeing the result stays within
// root. It defends against ".." traversal, absolute-path injection, and symlinks
// inside the root that point outside it.
func SafeJoin(root, relPath string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	// Resolve symlinks in the root itself so containment is checked against the
	// real directory (the root is operator-configured and expected to exist).
	rootAs := rootAbs
	resolved, rerr := filepath.EvalSymlinks(rootAbs)
	if rerr == nil {
		rootAbs = resolved
	}
	full := filepath.Join(rootAbs, filepath.FromSlash(relPath))
	// Lexical containment first: reject ".." traversal. An absolute relPath is
	// treated as relative to the root by filepath.Join, so it stays contained;
	// only genuine escapes produce a ".." relative path. This also covers paths
	// that don't exist yet (which EvalSymlinks can't resolve).
	if !withinRoot(rootAbs, full) {
		return "", ErrOutsideRoot
	}
	// Symlink-aware containment: resolve symlinks in the longest existing prefix
	// of the target and re-check, so a symlink inside the root that points
	// outside it is rejected rather than followed. Below a resolved root (no
	// links in it) only the names under it need looking at (resolveBelow).
	var target string
	if rerr == nil {
		target, _, _ = resolveBelow(rootAbs, rootAs, full, plainLooks)
	} else {
		target = resolveExisting(full)
	}
	if !withinRoot(rootAbs, target) {
		return "", ErrOutsideRoot
	}
	return full, nil
}

// SafeJoinListed is SafeJoin for a route that opens the path only as the disk
// spells it (the stream, /fs, a book read on demand), each of its looks
// bounded (ctx, at most timeout in all, counted on the path's mount as
// metadata.Bounded's): there reports
// whether each name of relPath is in its folder's listing as given, byte for
// byte, the listings read through names (a metadata.DirNames' Names). Share
// rules match byte for byte, so a rule spelled in another case than the disk's
// names nothing there, as a rule naming a missing folder does; but a disk that
// ignores case or the Unicode form (macOS's, a Windows or SMB share) opens
// "émile zola/x.m4b" as "Émile Zola/x.m4b", which lists only as stored. Each
// name is looked for in the listing before it is looked at (resolveBelow), so
// a variant and a missing path take the same steps, up to the first name not
// listed, and none past it: no Lstat a variant would answer and a missing name
// would not. A link out of the root is ErrOutsideRoot as soon as it is
// reached, nothing below it listed or looked at and its target never looked
// at (a link is read, then its target walked one name at a time: followLink),
// so the answer never tells what the folder it points to holds, nor whether it
// is there, nor how its mount fails. err is that, or a look given up on
// (metadata.GaveUp: one that did not answer in time, or failed as a share does
// that a retry may get past, metadata.TransientFault's EIO, ESTALE, ENOTCONN, as the
// scanner reads them) or ctx's; a listing or look that fails otherwise (the
// name gone, a permission) is a name not there.
func SafeJoinListed(ctx context.Context, root, relPath string, timeout time.Duration,
	names func(ctx context.Context, dir string, timeout time.Duration) ([]string, error)) (abs string, there bool, err error) {
	deadline := time.Now().Add(timeout)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false, err
	}
	resolved, rerr := metadata.EvalSymlinksBounded(ctx, rootAbs, metadata.WaitLeft(deadline))
	if err := unansweredLook(ctx, rerr); err != nil {
		return "", false, err
	}
	if rerr != nil {
		resolved = rootAbs
	}
	full := filepath.Join(resolved, filepath.FromSlash(relPath))
	if !withinRoot(resolved, full) {
		return "", false, ErrOutsideRoot
	}
	if rerr != nil {
		return full, false, nil // no root to list: nothing there
	}
	target, there, err := resolveBelow(resolved, rootAbs, full, boundedLooks(ctx, deadline, names, metadataLooks))
	switch {
	case err != nil:
		return "", false, err
	case !withinRoot(resolved, target):
		return "", false, ErrOutsideRoot
	}
	return full, there, nil
}

// belowLooks are resolveBelow's looks at a name under the root: SafeJoin's
// plain ones (plainLooks, under one Bounded call), or SafeJoinListed's, each
// bounded and a name first looked for in its folder's listing. No look is ever
// made at a name out of the root: a link is read (readlink), never followed,
// and its target walked name by name from the root down.
type belowLooks struct {
	// lstat is the name's Lstat, nil when it is not there (or the look
	// failed); err only for a look that went unanswered (with boundedLooks,
	// one failing as a share does that a retry may get past too).
	lstat func(path string) (fs.FileInfo, error)
	// readlink is a link's own target as stored (relative or absolute), ""
	// when it can't be read; err as lstat's.
	readlink func(path string) (string, error)
	// listed, when not nil, reports whether name is in dir's listing as given;
	// err as lstat's. Not nil: a name not listed ends the walk unlooked at.
	listed func(dir, name string) (bool, error)
}

// unansweredLook is err, a bounded look's (SafeJoinListed's, coverLook's), when it
// is no answer about the name, else nil (a name not there): a look given up
// on, ctx ended (its error ctx's), or a look failing as a share does that a
// retry may get past (metadata.TransientFault: EIO, ESTALE, a mount that dropped, made
// one given up on, transientGaveUp), as the scanner's cover look reads it
// (coverLook.find), so a book on a share that stopped answering is a 503
// share_not_answering, never a 404. A lasting fault (a permission, the device
// gone, a link loop: lastingFault) stays a name not there, the lasting answer
// it had, as does the name gone; so does a fault of the server's own (too
// many open files: neither), as it always was.
func unansweredLook(ctx context.Context, err error) error {
	if err = transientGaveUp(err); metadata.Unanswered(ctx, err) {
		return cmp.Or(err, ctx.Err())
	}
	return nil
}

// rawLooks are boundedLooks' calls at the disk, each a metadata.Bounded one:
// metadata's (metadataLooks), a test's to inject a share's errors.
type rawLooks struct {
	lstat    func(ctx context.Context, path string, timeout time.Duration) (fs.FileInfo, error)
	readlink func(ctx context.Context, path string, timeout time.Duration) (string, error)
}

// metadataLooks are SafeJoinListed's rawLooks.
var metadataLooks = rawLooks{lstat: metadata.LstatBounded, readlink: metadata.ReadlinkBounded}

// boundedLooks are SafeJoinListed's: each a metadata.Bounded look of its own
// (raw), within deadline, a name first looked for in its folder's listing
// (names), each failure read by unansweredLook. Every look is at a name under
// the root (resolveBelow never looks out of it), so every fault is the
// library's own share's.
func boundedLooks(ctx context.Context, deadline time.Time,
	names func(ctx context.Context, dir string, timeout time.Duration) ([]string, error), raw rawLooks) belowLooks {
	return belowLooks{
		lstat: func(p string) (fs.FileInfo, error) {
			fi, err := raw.lstat(ctx, p, metadata.WaitLeft(deadline))
			if err != nil {
				return nil, unansweredLook(ctx, err)
			}
			return fi, nil
		},
		readlink: func(p string) (string, error) {
			to, err := raw.readlink(ctx, p, metadata.WaitLeft(deadline))
			if err != nil {
				return "", unansweredLook(ctx, err)
			}
			return to, nil
		},
		listed: func(dir, name string) (bool, error) {
			ns, err := names(ctx, dir, metadata.WaitLeft(deadline))
			if err != nil {
				return false, unansweredLook(ctx, err)
			}
			return slices.Contains(ns, name), nil
		},
	}
}

// plainLooks are SafeJoin's: os.Lstat and os.Readlink, no listing.
var plainLooks = belowLooks{
	lstat: func(p string) (fs.FileInfo, error) {
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, nil //nolint:nilerr // a name not there ends the walk, as any failed look does
		}
		return fi, nil
	},
	readlink: func(p string) (string, error) {
		to, err := os.Readlink(p)
		if err != nil {
			return "", nil //nolint:nilerr // a link that can't be read resolves to nothing
		}
		return to, nil
	},
}

// maxLinkHops is how many links one path's resolve follows (resolveBelow), as
// filepath.EvalSymlinks allows: past it, a loop, which resolves to nothing.
const maxLinkHops = 255

// resolveBelow is resolveExisting(p) for a path p under rootAbs, a root with no
// symlinks in it (a path inside the root is; one out of it is some path out of
// it), rootAs the same folder as configured (absolute, its links unresolved:
// a link's absolute target may name the root so): the names below the root
// are looked at one at a time (lk), a
// link resolved where it is met (followLink: one out of the root ends the walk there, a
// path out of the root returned, nothing below it looked at:
// every path through such a link is refused, one that would come back into
// the root through a folder above it too) and the walk going on from its
// target, down to the first name that isn't there (it and the names after it
// kept as they are; there false). With lk.listed, a name is looked at only
// once its folder lists it as given. EvalSymlinks would Lstat every name from
// the filesystem's root down, the root's own included, which on a network
// share or a FUSE pool (Unraid's /mnt/user) is a round trip each, on every
// cover and stream request, and would follow a link out of the root to its
// target, whose mount's faults (a hang, EIO) would then tell what it points
// to. err is a look's that went unanswered.
func resolveBelow(rootAbs, rootAs, p string, lk belowLooks) (target string, there bool, err error) {
	rel, err := filepath.Rel(rootAbs, p)
	if err != nil || rel == "." {
		return p, true, nil //nolint:nilerr // p is the root, or not under it: checked by the caller
	}
	names := strings.Split(rel, string(filepath.Separator))
	cur := rootAbs
	w := &linkWalk{rootAbs: rootAbs, rootAs: rootAs, lk: lk}
	for i, name := range names {
		rest := func() string { return filepath.Join(append([]string{cur}, names[i:]...)...) }
		if lk.listed != nil {
			if in, err := lk.listed(cur, name); err != nil || !in {
				return rest(), false, err
			}
		}
		next := filepath.Join(cur, name)
		info, err := lk.lstat(next)
		if err != nil || info == nil {
			return rest(), false, err
		}
		if link, maybe := linkKind(info); link {
			resolved, err := followLink(w, cur, next, maybe)
			if err != nil || resolved == "" {
				return rest(), false, err
			}
			if !withinRoot(rootAbs, resolved) {
				// The target alone, never joined with the names after it: below
				// a link to a folder above the root ("up/<root>/Book/link/x")
				// they would read as a path back inside it, its links not
				// looked at.
				return resolved, true, nil
			}
			next = resolved
		}
		cur = next
	}
	return cur, true, nil
}

// linkWalk is one resolve's (resolveBelow's) state for followLink: the root,
// resolved (rootAbs: no link in it or above it) and as configured (rootAs,
// which the OS resolves to rootAbs), its looks, and the links followed so far.
type linkWalk struct {
	rootAbs, rootAs string
	lk              belowLooks
	hops            int
}

// linkKind is how resolveBelow and followLink read a name by its Lstat: a
// symbolic link (link), or a name that may be one (link and maybe): Lstat's
// irregular entry that is no folder, which on Windows is a junction (a mount
// point: since Go 1.23 neither ModeSymlink nor ModeDir, so walking through it
// as a folder would read its target's names, out of the root as it may be)
// or another reparse point (a cloud file's placeholder). Such a name is read
// (os.Readlink reads a junction's target) and followed as a link when it
// reads as one, else walked as a plain name, as it always was.
func linkKind(info fs.FileInfo) (link, maybe bool) {
	switch m := info.Mode(); {
	case m&fs.ModeSymlink != 0:
		return true, false
	case m&fs.ModeIrregular != 0 && !m.IsDir():
		return true, true
	}
	return false, false
}

// isPathSep reports whether r separates the names of a path (a link's target).
func isPathSep(r rune) bool { return r < 0x80 && os.IsPathSeparator(uint8(r)) }

// followLink resolves the link at link, in the folder dir (resolved, under
// w.rootAbs), one hop at a time: its target is read (lk.readlink) and walked
// name by name from dir (a relative one) or the filesystem's root (an
// absolute one; from the root itself when it names the root as configured,
// w.rootAs, which the OS resolves there: throughRootAs), ".." taking the
// folder above, each name under the root
// looked at (lk.lstat) and a link among them followed the same way. dir and
// the folders above it, the root's among them, are walked through unlooked at
// (both are resolved: no link in any of them). The first name out of the root ends it,
// that path returned, never looked at: a link out of the root answers the
// same whatever its target is, there or not, on a mount that answers, hangs
// or fails. A target that resolves to a folder above the root is returned as
// it is (out of the root too). "" for a link that resolves to nothing (a name
// along its target not there, a link that can't be read, more than
// maxLinkHops links: a loop); link itself for a name that may be a link
// (maybe: linkKind) and does not read as one. err is a look's that went
// unanswered.
func followLink(w *linkWalk, dir, link string, maybe bool) (string, error) {
	if w.hops++; w.hops > maxLinkHops {
		return "", nil
	}
	to, err := w.lk.readlink(link)
	switch {
	case err != nil:
		return "", err
	case to == "" && maybe:
		return link, nil // no link after all: the name itself, a plain one
	case to == "":
		return "", nil
	}
	cur, names := dir, strings.FieldsFunc(to, isPathSep)
	if vol := filepath.VolumeName(to); vol != "" || os.IsPathSeparator(to[0]) {
		vol = cmp.Or(vol, filepath.VolumeName(w.rootAbs))
		cur, names = vol+string(filepath.Separator), strings.FieldsFunc(to[len(filepath.VolumeName(to)):], isPathSep)
		if below, ok := w.throughRootAs(vol, names); ok {
			cur, names = w.rootAbs, below
		}
	}
	for _, name := range names {
		switch name {
		case ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		switch {
		case withinRoot(next, dir):
			cur = next // dir or a folder above it (the root's too): resolved, no link there
			continue
		case !withinRoot(w.rootAbs, next):
			return next, nil // out of the root: never looked at
		}
		info, err := w.lk.lstat(next)
		if err != nil || info == nil {
			return "", err
		}
		if link, maybe := linkKind(info); link {
			resolved, err := followLink(w, cur, next, maybe)
			if err != nil || resolved == "" || !withinRoot(w.rootAbs, resolved) {
				return resolved, err
			}
			next = resolved
		}
		cur = next
	}
	return cur, nil
}

// throughRootAs is names, an absolute link target's on volume vol, less the
// root's names as configured (w.rootAs) when they lead it: the OS resolves
// that spelling to w.rootAbs, as this resolve's EvalSymlinks of the root did,
// so the names after it are walked from w.rootAbs (a root that is itself a
// link, /data/books to /mnt/books, or macOS's /var to /private/var, whose
// links name its files through the configured path). ok false when they do
// not, or the root has no other spelling: a target through any other link
// above the root is out of it, never followed.
func (w *linkWalk) throughRootAs(vol string, names []string) (below []string, ok bool) {
	if w.rootAs == "" || w.rootAs == w.rootAbs || vol != filepath.VolumeName(w.rootAs) {
		return nil, false
	}
	as := strings.FieldsFunc(w.rootAs[len(vol):], isPathSep)
	if len(as) == 0 || len(names) < len(as) || !slices.Equal(names[:len(as)], as) {
		return nil, false
	}
	return names[len(as):], true
}

// withinRoot reports whether p is the root itself or nested under it.
func withinRoot(rootAbs, p string) bool {
	rel, err := filepath.Rel(rootAbs, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolveExisting resolves symlinks in the longest existing prefix of p and
// re-appends the (not-yet-existing) remainder, so containment can be checked even
// for a path that has not been created yet.
func resolveExisting(p string) string {
	rest := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // nothing along the path exists to resolve
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// BrowseFS lists a directory within a library root with offset pagination: a
// page of ListDir, its files with their Size and ModTime. relPath "" (or "/")
// lists the root. Its looks are ListDir's, within timeout in all, the page's
// file info among them (one Bounded call: given up on, the page lists without
// sizes).
func BrowseFS(ctx context.Context, root, relPath string, timeout time.Duration, offset, limit int, allow func(relPath string) bool, ignore *Ignore) (*Listing, error) {
	deadline := time.Now().Add(timeout)
	entries, dirEntries, err := listDir(ctx, root, relPath, timeout, allow, ignore)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	total := len(entries)
	offset = min(max(offset, 0), total)
	end := min(offset+limit, total)
	page := entries[offset:end]
	// Only files need their info (for Size, used to compute bitrate), and only
	// the page's: one round-trip per entry is the difference between a snappy and
	// a multi-second listing on a network mount. The directory read's own entry
	// answers it (a stat on Linux and macOS, nothing at all on Windows, which
	// lists it with the name).
	type info struct{ size, mod int64 }
	infos, _ := metadata.LookBounded(ctx, filepath.Join(root, filepath.FromSlash(relPath)), metadata.WaitLeft(deadline), func() ([]info, error) {
		out := make([]info, len(page))
		for i := range page {
			if page[i].IsDir {
				continue
			}
			if fi, err := dirEntries[offset+i].Info(); err == nil {
				out[i] = info{fi.Size(), fi.ModTime().Unix()}
			}
		}
		return out, nil
	})
	for i := range infos {
		page[i].Size, page[i].ModTime = infos[i].size, infos[i].mod
	}
	out := &Listing{Path: catalog.CleanRelPath(relPath), Entries: page, Total: total, Offset: offset}
	if end < total {
		out.NextOffset = end
	}
	return out, nil
}

// ListDir is a directory within a library root as the player browses it, whole
// and without Size or ModTime: hidden and non-audio files left out, and so is
// whatever allow refuses (a share's path rules; nil allows everything) or the
// library's ignore rules skip, as the scanner skips it, and an .mka a book of
// the folder leaves out (shadowedMKA: beside a file of its name in another
// format). Directories sort before files, each in natural name order
// (naturalLess), so the files list in the order a book of the folder plays them
// (sortFileOrder) and the order is stable for paging. The join and the
// listing are bounded looks (ctx, timeout in all, on the folder's mount): on a
// share that stopped answering the error is a look given up on
// (metadata.GaveUp).
func ListDir(ctx context.Context, root, relPath string, timeout time.Duration, allow func(relPath string) bool, ignore *Ignore) ([]Entry, error) {
	entries, _, err := listDir(ctx, root, relPath, timeout, allow, ignore)
	return entries, err
}

// listDir is ListDir, with each entry's fs.DirEntry from the directory read,
// in the same order.
func listDir(ctx context.Context, root, relPath string, timeout time.Duration, allow func(relPath string) bool, ignore *Ignore) ([]Entry, []fs.DirEntry, error) {
	deadline := time.Now().Add(timeout)
	// A hidden folder isn't listed, so it can't be opened by name either.
	full, err := ContentJoinBounded(ctx, root, relPath, timeout)
	if err != nil {
		return nil, nil, err
	}
	dirEntries, err := metadata.ReadDirBounded(ctx, full, metadata.WaitLeft(deadline))
	if err != nil {
		return nil, nil, err
	}
	type listed struct {
		Entry
		lower string // Name case folded once, for the sort
		de    fs.DirEntry
	}
	entries := make([]listed, 0, len(dirEntries))
	// The canonical rel path prefixes each entry's Path; scope checks and
	// persisted path keys rely on this same form (see catalog.CleanRelPath).
	cleanRel := catalog.CleanRelPath(relPath)
	// The audio the scanner sees here (not hidden, not ignored: isAudioEntry's
	// rule), whatever the caller's scope: what decides the .mka a book of this
	// folder leaves out (shadowedMKA, below).
	var audioNames []string
	for _, de := range dirEntries {
		name := de.Name()
		if isHidden(name) {
			continue // hidden here AND skipped by the scanner (see isHidden)
		}
		childRel := name
		if cleanRel != "" {
			childRel = cleanRel + "/" + name
		}
		isDir := de.IsDir()
		if !isDir && !metadata.IsAudio(name) {
			continue // hide non-audio files: clicking one can't open a book
		}
		if ignore.Covers(childRel, isDir) {
			continue // skipped by the library's ignore rules, here and by the scanner
		}
		if !isDir {
			audioNames = append(audioNames, name)
		}
		if allow != nil && !allow(childRel) {
			continue // outside the caller's share scope
		}
		entries = append(entries, listed{Entry{Name: name, Path: childRel, IsDir: isDir, IsAudio: !isDir}, strings.ToLower(name), de})
	}
	if shadowed := shadowedMKA(audioNames); shadowed != nil {
		entries = slices.DeleteFunc(entries, func(e listed) bool { return !e.IsDir && shadowed[e.Name] })
	}
	slices.SortStableFunc(entries, func(a, b listed) int {
		if a.IsDir != b.IsDir {
			if a.IsDir {
				return -1 // dirs first
			}
			return 1
		}
		return naturalCmpFolded(a.lower, b.lower, a.Name, b.Name)
	})
	out := make([]Entry, len(entries))
	des := make([]fs.DirEntry, len(entries))
	for i, e := range entries {
		out[i], des[i] = e.Entry, e.de
	}
	return out, des, nil
}

// MarkBooks fills in the entries the index holds as books (books by path, as
// catalog.BooksByPaths returns them): IsBook and the book's metadata.
func MarkBooks(entries []Entry, books map[string]catalog.Book) {
	for i := range entries {
		e := &entries[i]
		if b, ok := books[e.Path]; ok {
			e.IsBook = true
			e.Title = b.Title
			e.Author = b.Author
			e.Series = b.Series
			e.SeriesIndex, e.SeriesIndexEnd = b.SeriesIndex, b.SeriesIndexEnd
			e.Duration = b.Duration
		}
	}
}

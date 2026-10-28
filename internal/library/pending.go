package library

import (
	"context"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"slices"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The bounds of the pending pass (MarkPendingBooks). pendingTime is one
// listing's whole pass, walks included: on a slow network mount a folder read
// costs a round trip or more, so a page lists at most about that much later, and
// a folder the pass doesn't reach in time stays unmarked (listed as it was
// before #193). pendingWalkDirs bounds the walk that tells whether a folder with
// a `book` override is a disc set: a CD rip is a handful of folders, so a folder
// past it (a stale override on an author's folder) is read as none.
const (
	pendingWalkDirs = 200
	pendingTime     = 2 * time.Second
)

// MarkPendingBooks marks the folders among entries (a page of one folder's
// listing in a books library, MarkBooks already run) that the scanner would read
// as a book but the index doesn't hold yet (Entry.PendingBook, #193): the player
// opens one as a book, which indexes it on demand, rather than as a folder with
// nothing in it to play. The scanner's own rules decide (pendingBook), from
// directory reads alone (no audio is opened), within pendingTime, and only for a
// folder allow accepts (the caller's scope, nil for all: a folder listed only as
// the way down to a grant is no book of theirs) with no indexed book at or below
// it (holding, catalog.FoldersHoldingBooks: such a folder is the index's to
// describe and costs no disk read, so one with audio of its own above an indexed
// book waits for a scan). overrides are the library's folder overrides, ignore
// its ignore rules (ParseIgnore, as the listing was read with).
func MarkPendingBooks(ctx context.Context, lib catalog.Library, entries []Entry, overrides map[string]string, holding map[string]bool, allow func(string) bool, ignore *Ignore) {
	ctx, cancel := context.WithTimeout(ctx, pendingTime)
	defer cancel()
	until, _ := ctx.Deadline()
	// Whether a folder is a disc set, each walked once: the listed folder, which
	// every disc folder on the page asks about, among them.
	sets := map[string]bool{}
	isDiscSet := func(p string) bool {
		set, ok := sets[p]
		if !ok {
			limit := &walkCap{dirs: pendingWalkDirs, until: until}
			dirs, _, _, err := audioDirs(ctx, lib, absOf(lib, p), p, ignore, slog.New(slog.DiscardHandler), nil, limit, walkIO)
			set = err == nil && discSets(maps.Keys(dirs))[p]
			sets[p] = set
		}
		return set
	}
	for i := range entries {
		if ctx.Err() != nil {
			return // out of time, or the caller is gone: the rest stay unmarked
		}
		e := &entries[i]
		if !e.IsDir || e.IsBook || holding[e.Path] || (allow != nil && !allow(e.Path)) {
			continue
		}
		e.PendingBook = pendingBook(lib, e.Path, overrides, ignore, isDiscSet)
	}
}

// pendingBook reports whether a scan would make a book of the folder at rel
// (below the root) itself, as resolveBook works it out. A `collection` override
// makes each file a book of its own, the folder none. A folder joined into a book
// (joinRoot: a `book` override on a disc set, rel's own or the folder holding it)
// is that book: rel is one when it is the joined folder, none when it is a disc
// of the folder above. Otherwise rel is a book when it holds audio of its own
// (booksInDir's folder book). isDiscSet walks a folder (bounded), asked only as
// resolveBook asks it: of rel when it holds no audio, of the folder above when
// rel is a disc folder holding some.
func pendingBook(lib catalog.Library, rel string, overrides map[string]string, ignore *Ignore, isDiscSet func(string) bool) bool {
	if overrides[rel] == catalog.OverrideCollection {
		return false
	}
	own := hasAudio(absOf(lib, rel), rel, ignore)
	if root, ok := joinRoot(overrides, rel, func(p string) bool {
		if p == rel {
			return !own && isDiscSet(p)
		}
		return own && isDiscFolder(path.Base(rel)) && isDiscSet(p)
	}); ok {
		return root == rel
	}
	return own
}

// hasAudio reports whether the folder abs (at library-relative rel) directly
// holds an audio file discovery reads (isAudioEntry): audioEntries' question
// without building, filtering or sorting the list (a shadowed .mka has its
// shadowing file beside it, so any audio entry answers it).
func hasAudio(abs, rel string, ignore *Ignore) bool {
	entries, err := os.ReadDir(abs)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(entries, func(de fs.DirEntry) bool {
		return isAudioEntry(de, path.Join(rel, de.Name()), ignore)
	})
}

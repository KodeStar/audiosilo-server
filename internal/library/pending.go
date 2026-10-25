package library

import (
	"context"
	"log/slog"
	"maps"
	"path"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The bounds of the walk that tells a folder with a `book` override and no audio
// of its own is a disc set (pendingBook): a CD rip is a handful of folders, so a
// folder past these (a stale override on an author's folder) is read as none.
const (
	pendingWalkDirs = 200
	pendingWalkTime = 2 * time.Second
)

// MarkPendingBooks marks the folders among entries (a page of one folder's
// listing in a books library, MarkBooks already run) that the scanner would read
// as a book but the index doesn't hold yet (Entry.PendingBook, #193): the player
// opens one as a book, which indexes it on demand, rather than as a folder with
// nothing in it to play. The scanner's own rules decide (pendingBook), from
// directory reads alone (no audio is opened), and only for a folder allow accepts
// (the caller's scope, nil for all: a folder listed only as the way down to a
// grant is no book of theirs) with no indexed book at or below it (holding,
// catalog.FoldersHoldingBooks: such a folder is the index's to describe, and
// costs no disk read). overrides are the library's folder overrides.
func MarkPendingBooks(ctx context.Context, lib catalog.Library, entries []Entry, overrides map[string]string, holding map[string]bool, allow func(string) bool) {
	ignore := ParseIgnore(lib.IgnorePatterns)
	for i := range entries {
		e := &entries[i]
		if !e.IsDir || e.IsBook || holding[e.Path] || (allow != nil && !allow(e.Path)) {
			continue
		}
		e.PendingBook = pendingBook(ctx, lib, e.Path, overrides, ignore)
	}
}

// pendingBook reports whether a scan would make one book of the folder at rel
// (below the root): it holds audio of its own (booksInDir's folder book), unless
// a `collection` override makes each file a book of its own or it is a disc
// folder in a folder with a `book` override (a disc of a joined book, joinRoot);
// or, holding none, it has a `book` override and is a disc set (joinedBook),
// walked only then.
func pendingBook(ctx context.Context, lib catalog.Library, rel string, overrides map[string]string, ignore *Ignore) bool {
	if overrides[rel] == catalog.OverrideCollection {
		return false
	}
	if parent := dirOf(rel); parent != "" && overrides[parent] == catalog.OverrideBook && isDiscFolder(path.Base(rel)) {
		return false
	}
	if len(audioEntries(lib.Root, absOf(lib, rel), ignore)) > 0 {
		return true
	}
	if overrides[rel] != catalog.OverrideBook {
		return false
	}
	limit := &walkCap{dirs: pendingWalkDirs, until: time.Now().Add(pendingWalkTime)}
	dirs, _, _, err := audioDirs(ctx, lib, absOf(lib, rel), rel, ignore, slog.New(slog.DiscardHandler), nil, limit)
	return err == nil && discSets(maps.Keys(dirs))[rel]
}

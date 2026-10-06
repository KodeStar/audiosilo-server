package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Scanner walks library roots and keeps the catalog index up to date. The
// filesystem view does not depend on it, so scanning can run in the background
// after startup without blocking client browsing. Scans normally go through its
// job queue (jobs.go), which runs one at a time and records each in scan_runs.
type Scanner struct {
	cat         *catalog.Catalog
	ffprobePath string
	log         *slog.Logger

	mu       sync.Mutex
	progress map[int64]ScanProgress // latest progress per library (for the admin UI)
	jobs     jobQueue               // guarded by mu

	roots *rootProber // bounded, cached checks that a library root is reachable

	// OnRunFinished, when set before Start, hears how each scan job ended (the
	// notifications). ctx outlives the server's, like the run's own record.
	OnRunFinished func(ctx context.Context, r RunReport)
}

// ScanProgress reports how far a (possibly running) library scan has gotten, so
// the admin UI can show a counter instead of guessing.
type ScanProgress struct {
	Running bool `json:"running"`
	// Queued: a scan of the library waits in the job queue (behind another
	// library's, or to run again after the current one).
	Queued  bool `json:"queued,omitempty"`
	Total   int  `json:"total"`
	Done    int  `json:"done"`
	Indexed int  `json:"indexed"`
	// What the scan has changed so far (see catalog.ScanCounts).
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Moved   int `json:"moved"`
	Removed int `json:"removed"`
	// Unavailable is set when the last finished scan stopped at the
	// unavailable-root guard (ErrLibraryUnavailable): nothing was pruned.
	Unavailable bool `json:"unavailable,omitempty"`
}

// NewScanner returns a Scanner. ffprobePath may be "" to skip ffprobe.
func NewScanner(cat *catalog.Catalog, ffprobePath string, log *slog.Logger) *Scanner {
	if log == nil {
		log = slog.Default()
	}
	return &Scanner{
		cat:         cat,
		ffprobePath: ffprobePath,
		log:         log,
		progress:    map[int64]ScanProgress{},
		jobs:        newJobQueue(),
		roots:       newRootProber(),
	}
}

// Progress returns the latest scan progress for a library (the zero value if it
// has never been scanned this process), with whether a scan of it waits in the
// queue.
func (s *Scanner) Progress(libID int64) ScanProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progressLocked(libID)
}

// progressLocked is Progress with mu held.
func (s *Scanner) progressLocked(libID int64) ScanProgress {
	p := s.progress[libID]
	p.Queued = s.jobs.queuedFor(libID)
	return p
}

// updateProgress applies fn to a library's progress under the lock.
func (s *Scanner) updateProgress(libID int64, fn func(*ScanProgress)) {
	s.mu.Lock()
	p := s.progress[libID]
	fn(&p)
	s.progress[libID] = p
	s.mu.Unlock()
}

// ScanResult summarizes a scan: what it found and changed, whether it skipped
// pruning because part of the tree couldn't be read, and its log.
type ScanResult struct {
	catalog.ScanCounts
	Partial bool
	Log     []catalog.RunEvent
	// AddedTitles are the first few added books' titles (for a notification).
	AddedTitles []string
}

// maxAddedTitles bounds ScanResult.AddedTitles.
const maxAddedTitles = 5

// ErrLibraryUnavailable means the library root could not be read, or it
// returned no audio files while the index still has books - a strong signal
// that a network share (e.g. SMB/NFS) is unmounted. The scanner refuses to
// prune in this case so a dropped mount never wipes the rebuildable index.
// (Durable user state - progress/bookmarks/notes - is path-keyed with no FK to
// books since migration 0003, so it would survive a prune anyway; protecting the
// index still avoids a needless full re-scan and a transient empty catalog.)
var ErrLibraryUnavailable = errors.New("library root unavailable; skipping scan to protect the index")

// ErrNotIndexable means a resolved path is not a book (e.g. a directory that
// holds no audio directly and isn't joined into one, or a directory the detector
// treats as a collection of separate books rather than one book), or one the
// library's ignore rules skip.
var ErrNotIndexable = errors.New("path is not an indexable book")

// ErrNotAllowed means the book a path resolves to lies outside what the caller may
// reach (IndexPathWithin): a disc folder of a joined book, say, for a caller
// granted only the disc. Nothing was indexed.
var ErrNotAllowed = errors.New("the book at that path is outside the caller's scope")

// coverNames are sibling image files treated as a book's cover.
var coverNames = []string{"cover.jpg", "cover.jpeg", "cover.png", "folder.jpg", "folder.png"}

// isHidden reports whether a file/directory name is hidden (dot-prefixed).
// Discovery and the /fs browse view must agree on this - anything the scanner
// skips must also be invisible when browsing (and vice versa), or it would
// index books a client can't reach - so every hidden-name check in the package
// goes through here.
func isHidden(name string) bool { return strings.HasPrefix(name, ".") }

// primaryPath returns the file whose bytes represent a book (used for tag
// extraction and content fingerprinting): the book itself for a single-file
// book, else its first part.
func primaryPath(b *catalog.Book) string {
	if b.IsFolder && len(b.Files) > 0 {
		return b.Files[0].RelPath
	}
	return b.RelPath
}

// Scan indexes a single library. The job queue (jobs.go) calls it one scan at a
// time and closes its log with how it ended (closingEvent); tests call it
// directly. The result is never nil: on an error it holds what the scan had done
// and logged by then.
func (s *Scanner) Scan(ctx context.Context, lib catalog.Library) (_ *ScanResult, err error) {
	res := &ScanResult{}
	s.mu.Lock()
	s.progress[lib.ID] = ScanProgress{Running: true}
	s.mu.Unlock()
	rl := &runLog{}
	start := time.Now()
	defer func() {
		res.Log = rl.finish()
		s.mu.Lock()
		p := s.progress[lib.ID]
		// A queue job reads as running until its run is recorded (work clears it), so
		// a poll that sees it finished finds its history row finished too.
		if r := s.jobs.running; r == nil || r.LibraryID != lib.ID {
			p.Running = false
		}
		p.Unavailable = errors.Is(err, ErrLibraryUnavailable)
		s.progress[lib.ID] = p
		s.mu.Unlock()
		// The scan just looked at the root; let the next availability check look again.
		s.roots.forget(lib.Root)
	}()
	rl.add("info", "started", func(e *catalog.RunEvent) { e.Path = lib.Root })

	sigs, err := s.cat.Signatures(ctx, lib.ID)
	if err != nil {
		return res, err
	}

	// Guard against an unavailable root (e.g. an unmounted network share):
	// WalkDir would otherwise silently yield zero files and the prune step
	// would wipe the index. Fail fast before discovery. A hard-mounted share whose
	// server is gone can block a stat for minutes, holding the one scan worker and
	// every scan queued behind it, so the bounded root probe answers first.
	if _, answered := s.roots.check(lib.Root); !answered {
		return res, fmt.Errorf("%w: %q is not responding", ErrLibraryUnavailable, lib.Root)
	}
	if info, statErr := os.Stat(lib.Root); statErr != nil || !info.IsDir() {
		return res, fmt.Errorf("%w: %q: %v", ErrLibraryUnavailable, lib.Root, statErr)
	}

	overrides, err := s.cat.FolderOverrides(ctx, lib.ID)
	if err != nil {
		return res, err
	}
	// Discovery walks the whole tree (slow on a large network share) and emits no
	// per-file output, so bookend it with logs - otherwise a long scan looks hung.
	s.log.Info("scan started: discovering books", "library", lib.Name, "root", lib.Root)
	ignore := ParseIgnore(lib.IgnorePatterns)
	books, partialDiscovery, err := discoverAuto(ctx, lib, overrides, ignore, s.log, rl)
	if err != nil {
		return res, err
	}
	res.Books = len(books)
	rl.add("info", "discovered", func(e *catalog.RunEvent) { e.Count = len(books) })
	s.log.Info("discovery complete; indexing", "library", lib.Name, "books", len(books))

	// A root that exists but now contains no audio files, while the index still
	// has books, almost always means the mount dropped to an empty directory.
	// Refuse to prune so the index (and cascaded progress/bookmarks) survive -
	// unless the library's ignore rules skip every indexed book, which explains
	// the empty discovery (an admin just ignored all of it).
	if len(books) == 0 && len(sigs) > 0 && !ignoresAll(ignore, sigs) {
		return res, fmt.Errorf("%w: %q returned 0 audio files but %d are indexed",
			ErrLibraryUnavailable, lib.Root, len(sigs))
	}

	// The paths discovery found, built once: what detectMoves and splitFrom compare
	// the index with, and the prune's keep (every book found stays; carryJoinedState
	// adds the books it couldn't carry from, after both have read it).
	keep := make(map[string]bool, len(books))
	for _, b := range books {
		keep[b.RelPath] = true
	}

	// Carry user state across moved/renamed files before indexing. A moved book is
	// counted once, as moved: not as new at its new path, nor as removed at its old.
	moves := s.detectMoves(ctx, lib, sigs, books, keep, rl)
	res.Moved = len(moves)
	movedTo := make(map[string]bool, len(moves))
	for _, to := range moves {
		movedTo[to] = true
	}

	// Books a folder's joined book splits back into (its `book` override removed);
	// the folder's book went, and they are no new content.
	split := s.splitFrom(ctx, lib, books, keep, sigs, moves)
	splitRoots := make(map[string]bool, len(split))
	for _, root := range split {
		splitRoots[root] = true
	}

	coverBackfill := map[string]bool{}
	suspectBackfill := map[string]int{}
	splitBackfill := map[string]string{}
	lastLog := time.Now()
	report := func(done int) {
		s.updateProgress(lib.ID, func(p *ScanProgress) {
			p.Total, p.Done, p.Indexed = len(books), done, res.Added+res.Updated
			p.Added, p.Updated, p.Moved, p.Removed = res.Added, res.Updated, res.Moved, res.Removed
		})
	}
	for i, b := range books {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		report(i)
		// Heartbeat so a long indexing pass (ffprobe per file over a network share)
		// shows progress in the logs, not just in the admin UI counter.
		if time.Since(lastLog) >= 15*time.Second {
			s.log.Info("scan progress", "library", lib.Name, "done", i, "total", len(books), "indexed", res.Added+res.Updated)
			lastLog = time.Now()
		}
		old, existed := sigs[b.RelPath]
		if existed && old.MTime == b.MTime && old.Size == b.Size &&
			(s.ffprobePath == "" || (old.Duration > 0 && old.Codec != "")) &&
			!problemCleared(lib, old, s.ffprobePath) {
			// Unchanged since last scan; only skip the probe when ffprobe is
			// disabled, or a prior probe already stored both duration and codec
			// (so books indexed before the codec column get it backfilled).
			// Whether it is a disc of a split book turns on its siblings, so it can
			// change while the book doesn't (and starts unset before migration 0022).
			if old.SplitParent != b.SplitParent {
				splitBackfill[b.RelPath] = b.SplitParent
			}
			if old.ContentHash != "" {
				// Rows indexed before migration 0016 have no cover flag; fill it with a
				// tag read (no ffprobe) rather than re-indexing the book.
				// A file that can't be opened right now (a flaky mount) is left unknown
				// for the next scan rather than recorded as having no cover.
				primary := absOf(lib, primaryPath(b))
				if old.HasCover == nil && readable(primary) {
					_, _, coverBackfill[b.RelPath] = media.EmbeddedCover(primary)
				}
				// Rows indexed before 0017 haven't been checked for holding several
				// books; the same kind of tag read does it.
				if old.SuspectUnchecked && b.IsFolder {
					if n, ok := suspectFromTags(lib, b); ok {
						suspectBackfill[b.RelPath] = n
					}
				}
				continue
			}
			// The fingerprint never landed (a one-off read error). Without one a
			// later move of this book couldn't be detected and its progress would
			// be stranded, so re-enrich to backfill it - but probe the fingerprint
			// first: if the file still can't be read, the full re-enrich would fail
			// the same way, so a persistently unreadable book costs one cheap
			// failed open per scan instead of an ffprobe sweep forever.
			b.ContentHash = fingerprintFile(absOf(lib, primaryPath(b)))
			if b.ContentHash == "" {
				continue
			}
		}
		s.enrich(lib, b)
		if _, err := s.cat.UpsertBook(ctx, b); err != nil {
			s.log.Warn("index book failed", "library", lib.Name, "path", b.RelPath, "err", err)
			res.Errors++
			rl.add("error", "error", func(e *catalog.RunEvent) { e.Path, e.Detail = b.RelPath, err.Error() })
			continue
		}
		if b.ScanError != "" {
			res.Errors++
			rl.add("warn", "problem", func(e *catalog.RunEvent) {
				e.Path, e.Code, e.Detail = b.ScanErrorFile, b.ScanError, b.ScanErrorDetail
			})
		}
		switch {
		case existed:
			res.Updated++
		case !movedTo[b.RelPath] && !joinsIndexed(b, sigs) && split[b.RelPath] == "":
			// New content. A moved book is counted as moved, a book joined from disc
			// books already indexed is a reshape logged as joined (carryJoinedState),
			// and a disc split back out of a joined book one logged as split (below):
			// none is new, nor announced as added.
			res.Added++
			if len(res.AddedTitles) < maxAddedTitles {
				res.AddedTitles = append(res.AddedTitles, b.Title)
			}
		}
	}
	report(len(books))
	if err := s.cat.SetHasCover(ctx, lib.ID, coverBackfill); err != nil {
		s.log.Warn("record cover flags failed", "library", lib.Name, "err", err)
	}
	if err := s.cat.SetSuspectParts(ctx, lib.ID, suspectBackfill); err != nil {
		s.log.Warn("record suspect folders failed", "library", lib.Name, "err", err)
	}
	if err := s.cat.SetSplitParent(ctx, lib.ID, splitBackfill); err != nil {
		s.log.Warn("record split discs failed", "library", lib.Name, "err", err)
	}

	// Only prune when discovery saw the whole tree. If a subtree was unreadable
	// (partialDiscovery), its books are missing from `keep` through a mount/permission
	// fault, not a real deletion - pruning would drop still-present books and their
	// cascaded index rows. Skip it; a later clean scan reconciles genuine deletions.
	// Removed books leave nothing behind but their paths in this scan's log: the
	// listeners' progress is path-keyed and comes back if the files do.
	if partialDiscovery {
		res.Partial = true
		rl.add("warn", "partial", nil)
		s.log.Warn("skipping prune after partial discovery to protect the index",
			"library", lib.Name)
	} else {
		// A Stop (or the time limit) stops a scan before its prune, never inside it:
		// once begun, the prune finishes (one transaction), so a stopped scan has
		// removed nothing and one that pruned records what it removed.
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// Books a folder now joins into one hand their state to it first (logged as
		// joined, not removed); like the prune, this runs to completion once begun.
		joinedFrom := s.carryJoinedState(context.WithoutCancel(ctx), lib, sigs, overrides, keep, moves, rl)
		removed, err := s.cat.DeleteBooksNotIn(context.WithoutCancel(ctx), lib.ID, keep)
		if err != nil {
			return res, err
		}
		for _, p := range removed {
			if _, moved := moves[p]; moved || joinedFrom[p] {
				continue // the book moved or was joined (logged as such), it wasn't removed
			}
			if splitRoots[p] {
				// A joined book split back into its discs: a reshape, not a removal.
				s.log.Info("split into its discs", "library", lib.Name, "path", p)
				rl.add("info", "split", func(e *catalog.RunEvent) { e.Path = p })
				continue
			}
			res.Removed++
			rl.add("info", "removed", func(e *catalog.RunEvent) { e.Path = p })
		}
		report(len(books))
	}
	s.log.Info("library scanned", "library", lib.Name,
		"added", res.Added, "updated", res.Updated, "moved", res.Moved, "removed", res.Removed,
		"elapsed", time.Since(start))
	return res, nil
}

// maxRunEvents bounds one scan's log; past it, events are counted, not kept.
const maxRunEvents = 300

// maxProblemEvents bounds the problems (unreadable paths, read problems, errors)
// one log keeps, so a library full of unreadable files can't crowd out the moves
// and removed paths logged after them: the problems are also on the books, the
// removed paths nowhere else.
const maxProblemEvents = maxRunEvents / 2

// runLog collects a scan's events for its scan_runs row.
type runLog struct {
	events   []catalog.RunEvent
	problems int
	dropped  int
}

// add records an event (fill sets its facts), or counts it once the log, or its
// share for problems, is full. The cap leaves room for the truncation note and
// the closing event.
func (l *runLog) add(level, kind string, fill func(*catalog.RunEvent)) {
	problem := kind == "unreadable" || kind == "problem" || kind == "error"
	if len(l.events) >= maxRunEvents-2 || (problem && l.problems >= maxProblemEvents) {
		l.dropped++
		return
	}
	if problem {
		l.problems++
	}
	l.events = append(l.events, newEvent(level, kind, fill))
}

func newEvent(level, kind string, fill func(*catalog.RunEvent)) catalog.RunEvent {
	e := catalog.RunEvent{At: now(), Level: level, Kind: kind}
	if fill != nil {
		fill(&e)
	}
	return e
}

// finish returns the events, with a note of how many didn't fit.
func (l *runLog) finish() []catalog.RunEvent {
	if l.dropped > 0 {
		l.events = append(l.events, newEvent("info", "truncated", func(e *catalog.RunEvent) { e.Count = l.dropped }))
	}
	return l.events
}

// enrich fills metadata for a book from its primary file (tags + ffprobe) and
// folder context (path heuristics, sibling cover art).
func (s *Scanner) enrich(lib catalog.Library, b *catalog.Book) {
	primary := primaryPath(b)
	// Baseline from the path, then overlay embedded tags/probe which are
	// authoritative when present.
	base := metadata.DeriveFromPath(b.RelPath, b.IsFolder)
	b.Title, b.Author, b.Series, b.SeriesIndex = base.Title, base.Author, base.Series, base.SeriesIndex

	abs := absOf(lib, primary)
	md, _ := metadata.Extract(abs, s.ffprobePath)
	b.ScanError, b.ScanErrorFile, b.ScanErrorDetail = "", "", ""
	if !b.IsFolder {
		noteProblem(b, primary, b.Size, md)
	}
	if md != nil {
		b.Title = chooseTitle(md.Title, b.Title)
		if strings.TrimSpace(md.Author) != "" {
			b.Author = md.Author
		}
		if strings.TrimSpace(md.Series) != "" {
			b.Series = md.Series
		}
		if md.SeriesIndex != 0 {
			b.SeriesIndex = md.SeriesIndex
		}
		b.Narrator = md.Narrator
		b.Codec = md.Codec
	}

	// Move-detection fingerprint (reused content_hash column); skip if already
	// computed during move detection.
	if b.ContentHash == "" {
		b.ContentHash = fingerprintFile(abs)
	}

	// Normalize chapters so single-file and multi-file books look the same to a
	// client (see metadata.Chapter).
	if b.IsFolder {
		s.buildMultiFileChapters(lib, b)
	} else {
		none := 0
		b.SuspectParts = &none
		b.Chapters = singleFileChapters(md, b.RelPath)
		if md != nil && md.Duration > 0 {
			b.Duration = md.Duration
		} else if n := len(b.Chapters); n > 0 {
			// No format duration (some m4b); fall back to the last chapter's end.
			b.Duration = b.Chapters[n-1].End
		}
	}
	// Sibling cover art takes precedence; otherwise the cover handler falls back
	// to embedded art from the primary file. A folder book's art lives INSIDE the
	// book folder; a loose single-file book's art sits in the same directory.
	bookAbs := absOf(lib, b.RelPath)
	if b.IsFolder {
		b.CoverPath = findCover(lib.Root, bookAbs, true)
		// A multi-CD book's art often sits in the parent (e.g. ".../Book/CD1" with
		// the cover in ".../Book"); fall back there for disc-part subfolders.
		if b.CoverPath == "" && isDiscFolder(filepath.Base(bookAbs)) {
			b.CoverPath = findCover(lib.Root, filepath.Dir(bookAbs), true)
		}
		// A folder joined from its disc folders: its own art first (above), then the
		// first disc's.
		if first := dirOf(primary); b.CoverPath == "" && first != b.RelPath {
			b.CoverPath = findCover(lib.Root, absOf(lib, first), true)
		}
	} else {
		b.CoverPath = findCover(lib.Root, filepath.Dir(bookAbs), false)
	}
	// Embedded art; UpsertBook adds a sibling cover (CoverPath) to the flag itself.
	hasCover := md != nil && md.HasCover
	b.HasCover = &hasCover
}

// chooseTitle prefers a meaningful embedded title, falling back to the
// path-derived title when the embedded one is missing or generic ("Track 01").
func chooseTitle(embedded, pathDerived string) string {
	if embedded != "" && !metadata.IsGenericTitle(embedded) {
		return embedded
	}
	return pathDerived
}

// isDiscFolder reports whether a folder name is a disc/part subfolder like "CD1",
// "CD 2" or "Disc 3" - used to look one level up for a multi-CD book's cover.
func isDiscFolder(name string) bool {
	n := strings.NewReplacer(" ", "", "-", "", ".", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(name)))
	for _, p := range []string{"cd", "disc", "disk"} {
		if !strings.HasPrefix(n, p) {
			continue
		}
		rest := n[len(p):]
		if rest == "" {
			return true
		}
		allDigits := true
		for _, r := range rest {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return true
		}
	}
	return false
}

// imageExts are image types accepted as a fallback cover when no conventionally
// named file is present.
var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}

// findCover returns a library-relative path to a book's cover image in dir: a
// conventionally-named file (cover.jpg, folder.png, …) if present, else - only
// when anyImage is set (a book's own folder, where a stray image is almost
// certainly its cover) - the first image file alphabetically. Returns "" if none,
// so the cover handler falls back to embedded art.
func findCover(root, dir string, anyImage bool) string {
	for _, name := range coverNames {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			rel, _ := filepath.Rel(root, filepath.Join(dir, name))
			return filepath.ToSlash(rel)
		}
	}
	if !anyImage {
		return ""
	}
	entries, err := os.ReadDir(dir) // sorted by name
	if err != nil {
		return ""
	}
	var firstImage, coverish string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !imageExts[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		if firstImage == "" {
			firstImage = name
		}
		// Prefer a "cover"-named image (e.g. "<book> - Cover.jpg") over an arbitrary
		// one like an Amazon thumbnail ("71AbC...._SL500_.jpg").
		if coverish == "" && strings.Contains(strings.ToLower(name), "cover") {
			coverish = name
		}
	}
	pick := coverish
	if pick == "" {
		pick = firstImage
	}
	if pick == "" {
		return ""
	}
	rel, _ := filepath.Rel(root, filepath.Join(dir, pick))
	return filepath.ToSlash(rel)
}

// singleFileChapters takes embedded chapters from a single-file book and marks
// them as living in file 0 (path relPath), with the in-file start doubling as
// the book offset.
func singleFileChapters(md *metadata.Metadata, relPath string) []metadata.Chapter {
	if md == nil {
		return nil
	}
	out := make([]metadata.Chapter, len(md.Chapters))
	for i, ch := range md.Chapters {
		ch.FileIndex = 0
		ch.FilePath = relPath
		ch.BookOffset = ch.Start
		out[i] = ch
	}
	return out
}

// buildMultiFileChapters builds a normalized chapter list for a folder book.
// For each part it measures the duration and, crucially, if that file has its
// own embedded chapters (e.g. a single m4b living in its own book folder) it
// expands them; otherwise the whole file becomes one chapter. Book offsets are
// cumulative across parts, and the book's total duration is the sum. This makes
// a single chaptered m4b and a folder of split mp3s render identically.
func (s *Scanner) buildMultiFileChapters(lib catalog.Library, b *catalog.Book) {
	var cum float64
	idx := 0
	parts := make([]partFacts, 0, len(b.Files))
	for i := range b.Files {
		f := &b.Files[i]
		abs := absOf(lib, f.RelPath)
		md, _ := metadata.Extract(abs, s.ffprobePath)
		noteProblem(b, f.RelPath, f.Size, md)
		var dur float64
		if md != nil {
			dur = md.Duration
		}
		if md != nil && len(md.Chapters) > 0 {
			for _, ch := range md.Chapters {
				b.Chapters = append(b.Chapters, metadata.Chapter{
					Index:      idx,
					Title:      ch.Title,
					FileIndex:  f.Seq,
					FilePath:   f.RelPath,
					Start:      ch.Start,
					End:        ch.End,
					BookOffset: cum + ch.Start,
				})
				idx++
			}
			// ffprobe can report no format duration for a chaptered m4b; fall
			// back to the last chapter's end so durations/offsets stay correct.
			if dur <= 0 {
				dur = md.Chapters[len(md.Chapters)-1].End
			}
		} else {
			b.Chapters = append(b.Chapters, metadata.Chapter{
				Index:      idx,
				Title:      joinedPartTitle(b.RelPath, f.RelPath),
				FileIndex:  f.Seq,
				FilePath:   f.RelPath,
				Start:      0,
				End:        dur,
				BookOffset: cum,
			})
			idx++
		}
		f.Duration = dur
		if md != nil {
			f.Codec = md.Codec
		}
		parts = append(parts, partFacts{title: partName(f.RelPath, md), duration: dur})
		cum += dur
	}
	b.Duration = cum
	n := suspectParts(parts)
	b.SuspectParts = &n
}

// partTitle derives a chapter title from a part's filename, stripping the
// extension and any leading track number ("03 - Chapter Three" -> "Chapter Three").
func partTitle(relPath string) string {
	name := filepath.Base(relPath)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if _, title := metadata.SplitSeriesIndex(name); title != "" {
		return title
	}
	return name
}

// discoverAuto walks a library and classifies each directory that directly
// contains audio files on its own - so a mixed library (some folders are one
// multi-file book, others hold several single-file books) is handled without any
// layout setting. overrides forces a folder's interpretation when the heuristic
// gets it wrong (see booksInDir); a `book` override on a folder whose audio lives
// only in its disc folders joins them into one book (see discSets, joinedBook).
// The bool return reports whether discovery hit any per-entry error (an
// unreadable/permission-denied subtree on a partially-mounted share). When true,
// the caller must NOT prune: the books under the failed subtree are absent from the
// result through no fault of the filesystem-of-truth, and pruning them would drop a
// still-present book and its cascaded index rows until the mount recovers.
// Files and folders the library's ignore rules match are skipped (a folder with
// everything under it); rl, when set, logs unreadable entries. The walk stops with
// ctx (a Stop, the scan's time limit, shutdown): on a large network share it can
// take minutes.
//
// The root is resolved first and the walk starts at its target: filepath.WalkDir
// Lstat's its root, so a root that is itself a symlink (a NAS mount linked into
// place) was one non-directory entry and indexed nothing. Links below the
// root are still not followed. Rel paths are taken against the folder walked and
// folders are named under lib.Root, so both are the same whether or not the root
// is a link. (Not os.DirFS: it refuses folder names that aren't valid UTF-8,
// which a Linux share can hold, and every scan would then read as partial.)
func discoverAuto(ctx context.Context, lib catalog.Library, overrides map[string]string, ignore *Ignore, log *slog.Logger, rl *runLog) (books []*catalog.Book, hadErrors bool, err error) {
	walkRoot := lib.Root
	if resolved, rerr := filepath.EvalSymlinks(lib.Root); rerr == nil {
		walkRoot = resolved
	}
	dirs, hadErrors, err := audioDirs(ctx, lib, walkRoot, "", ignore, log, rl)
	if err != nil {
		return nil, hadErrors, err
	}
	return booksOf(lib, dirs, overrides), hadErrors, nil
}

// booksOf turns the folders that directly hold audio (dirs: library-relative, with
// their audio files, as audioDirs found them for the whole library) into books:
// each folder's own (booksInDir), except that the disc folders of a disc set with a
// `book` override (discSets, joinRoot) make one book of their folder (joinedBook).
// The discs of a disc set not joined are marked as such (markSplitDiscs).
func booksOf(lib catalog.Library, dirs map[string][]audioFile, overrides map[string]string) []*catalog.Book {
	sets := discSets(maps.Keys(dirs))
	isDiscSet := func(p string) bool { return sets[p] }
	var books []*catalog.Book
	joined := map[string]map[string][]audioFile{}
	for rel, audio := range dirs {
		if root, ok := joinRoot(overrides, rel, isDiscSet); ok {
			if joined[root] == nil {
				joined[root] = map[string][]audioFile{}
			}
			joined[root][rel] = audio
			continue
		}
		books = append(books, booksInDir(lib, rel, audio, overrides)...)
	}
	for root, discs := range joined {
		if b := joinedBook(lib, root, discs); b != nil {
			books = append(books, b)
		}
	}
	markSplitDiscs(books, sets)
	return books
}

// audioDirs walks walkFrom (the folder at library-relative fromRel, "" for the
// root) and returns the folders under it that directly hold audio, library-relative,
// each with its audio files in name order: the files audioEntries would list, read
// in the same walk rather than again. Hidden and ignored entries are skipped as
// discovery does; see discoverAuto for hadErrors and rl. walkFrom itself is never
// skipped, even when its own name begins with a dot.
func audioDirs(ctx context.Context, lib catalog.Library, walkFrom, fromRel string, ignore *Ignore, log *slog.Logger, rl *runLog) (dirs map[string][]audioFile, hadErrors bool, err error) {
	dirs = map[string][]audioFile{}
	err = filepath.WalkDir(walkFrom, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := path.Join(fromRel, relPathOf(walkFrom, p))
		if walkErr != nil {
			// Warn and skip just the unreadable entry rather than aborting the whole
			// scan, but record that discovery was partial so the caller skips pruning.
			hadErrors = true
			log.Warn("skipping unreadable path during discovery",
				"library", lib.Name, "path", absOf(lib, rel), "err", walkErr)
			if rl != nil {
				rl.add("warn", "unreadable", func(e *catalog.RunEvent) {
					e.Path, e.Detail = rel, pathErrText(walkErr)
				})
			}
			return nil
		}
		if d.IsDir() {
			// Skip hidden directories (.Trash-1000, Syncthing .stversions, .git) so
			// their audio isn't indexed into books unreachable via the fs browse view
			// (which also hides them).
			if rel != fromRel && (isHidden(d.Name()) || (!ignore.Empty() && ignore.Match(rel, true))) {
				return fs.SkipDir
			}
			return nil
		}
		if !isAudioEntry(d, rel, ignore) {
			return nil
		}
		dir := dirOf(rel)
		dirs[dir] = append(dirs[dir], audioFile{abs: absOf(lib, rel), de: d})
		return nil
	})
	return dirs, hadErrors, err
}

// isAudioEntry reports whether a directory entry (at library-relative rel) is an
// audio file discovery reads: not a folder, not hidden, an audio extension, and
// not ignored. The walk and audioEntries share it, so both see the same files.
func isAudioEntry(de fs.DirEntry, rel string, ignore *Ignore) bool {
	return !de.IsDir() && !isHidden(de.Name()) && metadata.IsAudio(de.Name()) &&
		(ignore.Empty() || !ignore.Match(rel, false))
}

// absOf is the folder or file at a library-relative path, named under lib.Root.
func absOf(lib catalog.Library, rel string) string {
	return filepath.Join(lib.Root, filepath.FromSlash(rel))
}

// booksInDir turns the audio files directly inside absDir into books. The model
// matches the dominant "folder per book" convention (and Audiobookshelf): a
// directory that directly contains audio is ONE book, with all those files as its
// tracks/chapters - whether it holds a single m4b or fifty mp3 chapters. The two
// exceptions: audio sitting directly in the library root has no enclosing book
// folder, so each such file is its own single-file book ("flat"); and a folder of
// loose single-file books (one book per file) is expressed with the `collection`
// override. `book` forces the folder-is-one-book reading (e.g. at the root); on a
// disc set (a folder whose audio is only in its disc folders) it joins them instead
// (joinedBook), which never reaches here. audio is the folder's (rel's) own audio files
// (audioEntries, or the discovery walk's).
func booksInDir(lib catalog.Library, rel string, audio []audioFile, overrides map[string]string) []*catalog.Book {
	if len(audio) == 0 {
		return nil
	}
	asBook := func() []*catalog.Book {
		if b := folderBook(lib, absOf(lib, rel), audio); b != nil {
			return []*catalog.Book{b}
		}
		return nil
	}
	switch overrides[rel] {
	case catalog.OverrideBook:
		return asBook()
	case catalog.OverrideCollection:
		return fileBooksIn(lib, audio)
	}
	if rel == "" {
		return fileBooksIn(lib, audio)
	}
	return asBook()
}

// audioFile is one audio file found for a book: its path under lib.Root and its
// directory entry.
type audioFile struct {
	abs string
	de  fs.DirEntry
}

// audioEntries returns the non-hidden, non-ignored audio files directly inside
// absDir, in the stable name order os.ReadDir provides.
func audioEntries(root, absDir string, ignore *Ignore) []audioFile {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil
	}
	var audio []audioFile
	for _, de := range entries {
		abs := filepath.Join(absDir, de.Name())
		if isAudioEntry(de, relPathOf(root, abs), ignore) {
			audio = append(audio, audioFile{abs: abs, de: de})
		}
	}
	return audio
}

// fileBooksIn builds one single-file book per audio file.
func fileBooksIn(lib catalog.Library, audio []audioFile) []*catalog.Book {
	var books []*catalog.Book
	for _, f := range audio {
		info, err := f.de.Info()
		if err != nil {
			continue
		}
		books = append(books, fileBook(lib, f.abs, info))
	}
	return books
}

// relPathOf returns absDir as a slash-separated path relative to root ("" for
// the root itself), matching the rel_path keys used elsewhere.
func relPathOf(root, absDir string) string {
	rel, err := filepath.Rel(root, absDir)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// addedAt is when a book first appeared on disk: the file's birth (creation)
// time where the OS/filesystem records it, otherwise its mtime. Formatted as
// RFC3339 UTC to match the other timestamp columns and sort lexicographically.
func addedAt(absPath string, info os.FileInfo) string {
	t := info.ModTime()
	if bt, ok := birthTime(absPath, info); ok && !bt.IsZero() {
		t = bt
	}
	return t.UTC().Format(time.RFC3339)
}

// fileBook builds a single-file book for the audio file at absPath.
func fileBook(lib catalog.Library, absPath string, info os.FileInfo) *catalog.Book {
	rel, _ := filepath.Rel(lib.Root, absPath)
	return &catalog.Book{
		LibraryID: lib.ID,
		RelPath:   filepath.ToSlash(rel),
		Format:    ext(filepath.Base(absPath)),
		Size:      info.Size(),
		MTime:     info.ModTime().Unix(),
		AddedAt:   addedAt(absPath, info),
	}
}

// folderBook builds a (possibly multi-file) book at absDir from audio, its
// files in play order (audioEntries' name order, or joinedBook's), or returns nil
// if there are none.
func folderBook(lib catalog.Library, absDir string, audio []audioFile) *catalog.Book {
	var files []catalog.BookFile
	var totalSize, maxMTime int64
	var added string // earliest file added time = when the book first appeared
	for _, f := range audio {
		info, ierr := f.de.Info()
		if ierr != nil {
			continue
		}
		frel, _ := filepath.Rel(lib.Root, f.abs)
		files = append(files, catalog.BookFile{
			RelPath: filepath.ToSlash(frel), Seq: len(files), Format: ext(f.de.Name()), Size: info.Size(),
		})
		totalSize += info.Size()
		if m := info.ModTime().Unix(); m > maxMTime {
			maxMTime = m
		}
		if a := addedAt(f.abs, info); added == "" || a < added {
			added = a // RFC3339 UTC sorts lexicographically, so min string = earliest
		}
	}
	if len(files) == 0 {
		return nil
	}
	rel, _ := filepath.Rel(lib.Root, absDir)
	return &catalog.Book{
		LibraryID: lib.ID,
		RelPath:   filepath.ToSlash(rel),
		IsFolder:  true,
		Format:    files[0].Format,
		Size:      totalSize,
		MTime:     maxMTime,
		AddedAt:   added,
		Files:     files,
	}
}

// IndexPath indexes a single browsed path on demand and returns the resulting
// book (with chapters). It lets a client act on something it found in the
// filesystem view before the background scan has reached it. When a folder is a
// single (multi-file) book, resolving either the book folder or a file inside it
// yields that same folder book.
func (s *Scanner) IndexPath(ctx context.Context, lib catalog.Library, relPath string) (*catalog.Book, error) {
	return s.IndexPathWithin(ctx, lib, relPath, nil)
}

// IndexPathWithin is IndexPath for a caller who may reach only the paths allow
// accepts (nil: every path). The book relPath resolves to can lie above it (the
// folder book of a part, the joined book of a disc folder); when allow refuses the
// book's path, it returns ErrNotAllowed before probing or indexing anything.
func (s *Scanner) IndexPathWithin(ctx context.Context, lib catalog.Library, relPath string, allow func(string) bool) (*catalog.Book, error) {
	// SafeJoin is the security gate (rejects traversal and symlink escapes). It
	// returns a symlink-RESOLVED path, but we derive the working path from an
	// unresolved join so the rel_path computed by fileBook/folderBook matches the
	// full scan, which names folders under lib.Root unresolved. Using SafeJoin's
	// resolved path here would yield a "../"-laden rel_path whenever any component
	// of lib.Root is a symlink (e.g. macOS /tmp -> /private/tmp, or a NAS /data ->
	// /mnt/...).
	if _, err := SafeJoin(lib.Root, relPath); err != nil {
		return nil, err
	}
	abs := absOf(lib, relPath)
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotIndexable, err)
	}

	// What the ignore rules keep out of a scan isn't in the library on demand either.
	ignore := ParseIgnore(lib.IgnorePatterns)
	if ignore.Covers(relPathOf(lib.Root, abs), info.IsDir()) {
		return nil, fmt.Errorf("%w: %q is ignored", ErrNotIndexable, relPath)
	}
	overrides, err := s.cat.FolderOverrides(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	// Classify the containing directory exactly as a full scan would, then pick
	// the book the requested path resolves to: the folder book itself, a
	// single-file book, or the folder book a clicked part belongs to. Inside a
	// folder joined into one book, that is the joined book, for a disc folder too.
	dirRel := relPathOf(lib.Root, abs)
	if !info.IsDir() {
		dirRel = dirOf(dirRel)
	}
	own := audioEntries(lib.Root, absOf(lib, dirRel), ignore)
	// Whether a folder with a `book` override is a disc set turns on its whole
	// subtree, walked only then; the walk is the joined book's too.
	var discs map[string][]audioFile
	var walkErr error
	isDiscSet := func(p string) bool {
		// The folder itself holding audio, or (for the folder holding it) a folder
		// that is no disc with audio of its own: no disc set, no walk (an author's or
		// a series' stale override).
		if p == dirRel {
			if len(own) > 0 {
				return false
			}
		} else if len(own) == 0 || !isDiscFolder(path.Base(dirRel)) {
			return false
		}
		dirs, _, err := audioDirs(ctx, lib, absOf(lib, p), p, ignore, s.log, nil)
		if err != nil {
			walkErr = err
			return false
		}
		discs = dirs
		return discSets(maps.Keys(dirs))[p]
	}
	var candidates []*catalog.Book
	if root, ok := joinRoot(overrides, dirRel, isDiscSet); ok {
		if b := joinedBook(lib, root, discs); b != nil {
			candidates = []*catalog.Book{b}
		}
	} else if walkErr != nil {
		return nil, walkErr
	} else {
		candidates = booksInDir(lib, dirRel, own, overrides)
	}
	book := pickBook(candidates, relPathOf(lib.Root, abs))
	if book == nil {
		return nil, fmt.Errorf("%w: no book at %q", ErrNotIndexable, relPath)
	}
	if allow != nil && !allow(book.RelPath) {
		return nil, ErrNotAllowed
	}
	if book.SplitParent, err = s.splitParentOf(ctx, lib, book, ignore); err != nil {
		return nil, err
	}

	s.enrich(lib, book)
	// UpsertBook layers any path-keyed enrichment and metadata overrides onto the
	// scanned values in the same transaction.
	id, err := s.cat.UpsertBook(ctx, book)
	if err != nil {
		return nil, err
	}
	return s.cat.GetBook(ctx, id)
}

// splitParentOf is books.split_parent for a book IndexPath indexes, worked out as a
// full scan would (markSplitDiscs): the folder holding it when that is a disc set
// (discSets, from the folder's subtree), read only for a folder book with a disc
// name. A disc set with a `book` override would have made the joined book instead.
func (s *Scanner) splitParentOf(ctx context.Context, lib catalog.Library, b *catalog.Book, ignore *Ignore) (string, error) {
	parent := dirOf(b.RelPath)
	if !b.IsFolder || parent == "" || !isDiscFolder(path.Base(b.RelPath)) {
		return "", nil
	}
	dirs, _, err := audioDirs(ctx, lib, absOf(lib, parent), parent, ignore, s.log, nil)
	if err != nil {
		return "", err
	}
	if discSets(maps.Keys(dirs))[parent] {
		return parent, nil
	}
	return "", nil
}

// pickBook returns the book from candidates whose path matches want - the book's
// own rel_path (single-file or folder book) or, for a part the client clicked
// inside a folder book, one of that book's files, or a folder holding some of
// them (a disc folder of a joined book).
func pickBook(candidates []*catalog.Book, want string) *catalog.Book {
	for _, b := range candidates {
		if b.RelPath == want {
			return b
		}
		for _, f := range b.Files {
			// A folder book's subfolder is a disc folder of a joined book; a folder of
			// single-file books (collection) is no book.
			if f.RelPath == want || (b.IsFolder && want != "" && strings.HasPrefix(f.RelPath, want+"/")) {
				return b
			}
		}
	}
	return nil
}

// readable reports whether the file at absPath can be opened for reading.
func readable(absPath string) bool {
	f, err := os.Open(absPath)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func ext(name string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
}

const fingerprintChunk = 64 * 1024 // bytes hashed from the head and tail

// fingerprintFile returns a cheap content fingerprint - sha256 of
// (size, first 64KB, last 64KB) - used only to detect "same content, new path"
// moves. It is not a full hash (large files stay cheap) and intentionally not a
// durable identity (that is the path). Returns "" on error.
func fingerprintFile(absPath string) string {
	f, err := os.Open(absPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d:", info.Size())
	head := make([]byte, fingerprintChunk)
	n, _ := io.ReadFull(f, head)
	h.Write(head[:n])
	if info.Size() > int64(fingerprintChunk) {
		tail := make([]byte, fingerprintChunk)
		if m, err := f.ReadAt(tail, info.Size()-int64(fingerprintChunk)); err == nil || m > 0 {
			h.Write(tail[:m])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// detectMoves migrates durable user state (progress/bookmarks/notes/history)
// from a vanished path to a new path with matching content, so a moved/renamed
// file keeps its state, then carries the favourites of the folders those moves
// say were renamed (renamedFolders). It only does work when something both
// disappeared and appeared, keeping fingerprinting off the hot path of normal scans.
// found is the paths of books (discovery's). It returns the moves it carried
// state across (old path -> new path), logging each to rl.
func (s *Scanner) detectMoves(ctx context.Context, lib catalog.Library, sigs map[string]catalog.Signature,
	books []*catalog.Book, found map[string]bool, rl *runLog) (moved map[string]string) {
	var disappeared []string
	for relPath := range sigs {
		if !found[relPath] {
			disappeared = append(disappeared, relPath)
		}
	}
	var newBooks []*catalog.Book
	for _, b := range books {
		if _, existed := sigs[b.RelPath]; !existed {
			newBooks = append(newBooks, b)
		}
	}
	if len(disappeared) == 0 || len(newBooks) == 0 {
		return nil
	}
	moved = map[string]string{}
	// The stored fingerprints for the disappeared paths are already in sigs
	// (Signatures selects content_hash), so no extra query is needed.
	oldFP := make(map[string]string, len(disappeared))
	for _, p := range disappeared {
		if h := sigs[p].ContentHash; h != "" {
			oldFP[p] = h
		}
	}
	for _, nb := range newBooks {
		fp := fingerprintFile(absOf(lib, primaryPath(nb)))
		nb.ContentHash = fp // reused by enrich, avoiding a second read
		if fp == "" {
			continue
		}
		for oldPath, ofp := range oldFP {
			if ofp != fp || reclassified(oldPath, sigs[oldPath], nb) {
				continue
			}
			if err := s.cat.MoveDurableState(ctx, lib.ID, oldPath, nb.RelPath); err != nil {
				s.log.Warn("move state failed", "from", oldPath, "to", nb.RelPath, "err", err)
				rl.add("error", "error", func(e *catalog.RunEvent) { e.Path, e.To, e.Detail = oldPath, nb.RelPath, err.Error() })
			} else {
				s.log.Info("detected move", "library", lib.Name, "from", oldPath, "to", nb.RelPath)
				rl.add("info", "moved", func(e *catalog.RunEvent) { e.Path, e.To = oldPath, nb.RelPath })
				moved[oldPath] = nb.RelPath
			}
			delete(oldFP, oldPath)
			break
		}
	}
	// A navigation folder (an author, a series) has no book of its own to move, so
	// a favourite on it follows the folder the moved books say was renamed.
	for from, to := range renamedFolders(lib, moved) {
		if err := s.cat.MoveFolderFavourites(ctx, lib.ID, from, to); err != nil {
			s.log.Warn("move folder favourites failed", "library", lib.Name, "from", from, "to", to, "err", err)
			rl.add("error", "error", func(e *catalog.RunEvent) { e.Path, e.To, e.Detail = from, to, err.Error() })
		}
	}
	return moved
}

// renamedFolders works out the folders a scan's moves (old path -> new path) say
// were renamed or moved: the ancestors of a moved book, paired up from the end -
// the folders holding the two paths, then on up while the names of the pair match
// ("Series A/Book" -> "Series B/Book" pairs "Series A" with "Series B"; a book
// renamed as it went still left its folder for the new one). A pair counts only
// when every move agrees on where the folder went and the old folder is gone from
// disk (a book moved out of a folder that is still there says nothing about it).
func renamedFolders(lib catalog.Library, moved map[string]string) map[string]string {
	out := map[string]string{} // "" = the moves disagree
	for o, n := range moved {
		for {
			o, n = path.Dir(o), path.Dir(n)
			if o == "." || n == "." || o == n {
				break // the root, or the ancestors both paths share
			}
			to := n
			if prev, ok := out[o]; ok && prev != n {
				to = ""
			}
			out[o] = to
			if path.Base(o) != path.Base(n) {
				break
			}
		}
	}
	listed := map[string]map[string]bool{} // each folder's entry names, read once
	for from, to := range out {
		if to == "" || dirPresent(lib, from, listed) {
			delete(out, from)
		}
	}
	return out
}

// dirPresent reports whether a folder at exactly this path is on disk. A
// case-insensitive filesystem still answers to the old name of a folder renamed
// only in case ("WIth" -> "With"), or of any folder under one so renamed, so when
// the path still answers, each name along it must be in its parent's listing
// (listed caches the listings). It errs toward present: a folder it can't check
// keeps its favourites.
func dirPresent(lib catalog.Library, rel string, listed map[string]map[string]bool) bool {
	if _, err := os.Lstat(absOf(lib, rel)); err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	for dir := rel; dir != "."; dir = path.Dir(dir) {
		parent := path.Dir(dir)
		names, ok := listed[parent]
		if !ok {
			entries, err := os.ReadDir(absOf(lib, parent))
			if err != nil {
				return true
			}
			names = make(map[string]bool, len(entries))
			for _, e := range entries {
				names[e.Name()] = true
			}
			listed[parent] = names
		}
		if !names[path.Base(dir)] {
			return false
		}
	}
	return true
}

// reclassified reports whether a vanished path and a new path with the same
// fingerprint are one folder seen two ways rather than a moved book. Turning a
// folder book into a collection (or back) leaves a book at a path nested in the
// other, fingerprinted by the folder's first part, but it is a different book: only
// an equal size (a single-part folder) makes the two the same.
func reclassified(oldPath string, old catalog.Signature, nb *catalog.Book) bool {
	nested := strings.HasPrefix(nb.RelPath, oldPath+"/") || strings.HasPrefix(oldPath, nb.RelPath+"/")
	if nested && old.Size != nb.Size {
		return true
	}
	// A joined book shares its first disc's fingerprint: renamed away from its
	// `book` override (which stays on the old path), it reads again as one book per
	// disc, and its first disc is not the joined book moved (the joined timeline's
	// positions and finish don't fit one disc).
	return old.IsFolder && nb.IsFolder && isDiscFolder(path.Base(nb.RelPath)) && old.Size > nb.Size
}

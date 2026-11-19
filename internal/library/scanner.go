package library

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/fsaccess"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/mounts"
	"github.com/kodestar/audiosilo-server/internal/pool"
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
	// previews are the setup previews under way per mount (Preview), a hung one
	// holding its slot until it returns.
	previews *mounts.PerMount[chan struct{}]

	// The reads of a book under way, by book: an on-demand index's
	// (IndexPathWithin), IndexMoved's or a scan's. A concurrent request for the
	// same book waits on the one read rather than reading the book again (a book
	// page asks for the item and its chapters at once), and no two run side by
	// side (indexShared, startFlight).
	flightMu sync.Mutex
	flights  map[bookKey]*indexFlight
	// base, once set (SetBaseContext), ends the reads leadIndex runs past their
	// requests with the server: none reads on or writes once it has stopped.
	base atomic.Pointer[context.Context]

	// OnRunFinished, when set before Start, hears how each scan job ended (the
	// notifications). ctx outlives the server's, like the run's own record.
	OnRunFinished func(ctx context.Context, r RunReport)

	// concurrency is how many audio files a scan reads at once (SetConcurrency).
	concurrency int
	// probeTimeout is each ffprobe run's, and each bounded look at a file beside
	// the audio (metadata.Bounded: a cover, an ebook, a metadata.json, an
	// attachment) a scan or an on-demand index makes (metadata.ProbeTimeout;
	// tests shorten it).
	probeTimeout time.Duration
	// readsLeft is how many book reads stopped scans left under way (stopped),
	// not ended yet.
	readsLeft atomic.Int64
	// batched, when set (tests), hears each book a scan takes into its write
	// batch, with the batch's length then.
	batched func(n int)
	// readFile, when set (tests), stands in for each scan's and on-demand
	// index's metadata.ExtractTimeout (readSlots.readFile).
	readFile func(ctx context.Context, absPath, ffprobePath string, timeout time.Duration) *metadata.Metadata

	// The files this server has seen ffprobe reject (problemCleared), by
	// absolute path.
	rejectedMu sync.Mutex
	rejected   map[string]fileState

	// With no ffprobe: the books with no stored duration this server has read
	// again since it started (rereadUnprobed), in the state read.
	unprobedMu sync.Mutex
	unprobed   map[bookKey]fileState
}

// rereadUnprobed reports whether a scan with no ffprobe should read again a
// book it finds unchanged with no stored duration or codec (the caller's
// test): once per
// server run and book state (noteUnprobed). A server that ran without ffprobe
// before the in-process probe ran without it (SCAN-SPIKE decision 2) gets its
// existing books' durations from its first scan since, while a file the probe
// is unsure of (no ffprobe to ask) is not re-indexed on every scan. A restart
// reads such books once more.
func (s *Scanner) rereadUnprobed(libraryID int64, b *catalog.Book) bool {
	s.unprobedMu.Lock()
	defer s.unprobedMu.Unlock()
	was, ok := s.unprobed[bookKey{libraryID, b.RelPath}]
	return !ok || was != (fileState{mtime: b.MTime, size: b.Size})
}

// noteUnprobed remembers a book a scan with no ffprobe has just written with
// no duration or codec, in the state it read (rereadUnprobed).
func (s *Scanner) noteUnprobed(libraryID int64, b *catalog.Book) {
	if s.ffprobePath != "" || (b.Duration > 0 && b.Codec != "") {
		return
	}
	s.unprobedMu.Lock()
	defer s.unprobedMu.Unlock()
	if s.unprobed == nil {
		s.unprobed = map[bookKey]fileState{}
	}
	s.unprobed[bookKey{libraryID, b.RelPath}] = fileState{mtime: b.MTime, size: b.Size}
}

// SetConcurrency sets how many audio files a scan reads at once (tags and
// ffprobe; config scan.concurrency): n < 1 means the default, one per CPU up to
// DefaultScanReaders. Set below walkIO, it also caps discovery's folder listings
// and file lookups (walkWidth). Set it before Start; a scan reads it when it
// begins.
// However many files are read at once, the index is written by the scan alone,
// in discovery order, so a scan's result is the same as one that reads a file at
// a time.
func (s *Scanner) SetConcurrency(n int) { s.concurrency = n }

// DefaultScanReaders caps the default number of files a scan reads at once. Past
// about eight the scan gains little (a disk or share is the limit, and ffprobe's
// start-up dominates), while each ffprobe run costs more CPU as they contend:
// on a 20-core machine, 20 at once took a third more CPU than 8 for the same
// scan, and the server keeps cores free for listeners. scan.concurrency sets more.
const DefaultScanReaders = 8

// readers is how many audio files a scan reads at once. One per CPU is
// GOMAXPROCS, not NumCPU: in a container it is the CPU limit (cgroup quota), not
// the host's cores, and the ffprobe runs count against that limit too.
func (s *Scanner) readers() int {
	if s.concurrency >= 1 {
		return s.concurrency
	}
	return min(runtime.GOMAXPROCS(0), DefaultScanReaders)
}

// walkWidth is how many filesystem calls discovery makes at once: walkIO, or
// scan.concurrency when that is set lower, which asks a slow disk or a busy
// share for fewer at a time (1: one at a time, each folder listed as the walk
// reaches it). Not readers(): those calls wait on round trips, not on CPUs.
func (s *Scanner) walkWidth() int {
	if s.concurrency >= 1 {
		return min(walkIO, s.concurrency)
	}
	return walkIO
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
		flights:     map[bookKey]*indexFlight{},
		jobs:        newJobQueue(),
		roots:       newRootProber(),
		previews:    newMountSlots(previewsPerMount),

		probeTimeout: metadata.ProbeTimeout,
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

// ErrUnknownKind is a scan of a library whose kind this server doesn't know (a
// newer AudioSilo's, run on this database before a rollback): it is left alone,
// never scanned as books, which would prune what the newer server indexed
// there (server#182).
var ErrUnknownKind = errors.New("this server doesn't know the library's kind; left alone")

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

// hasHiddenPart reports whether any segment of a library-relative path is hidden
// (isHidden): ContentPath's rule.
func hasHiddenPart(relPath string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(relPath), "/") {
		if seg != "." && seg != ".." && isHidden(seg) {
			return true
		}
	}
	return false
}

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
	if !catalog.KnownKind(lib.Kind) {
		return res, fmt.Errorf("%w: %q", ErrUnknownKind, lib.Kind)
	}
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
	// A root the server may reach but not list (a folder another user owns, as on
	// a NAS share) would read as an empty library: stop, naming the folder, the
	// server's user and the fix (server#368).
	if err := fsaccess.CheckRead(lib.Root); err != nil {
		return res, fmt.Errorf("%w: %w", ErrLibraryUnavailable, err)
	}

	if lib.Kind == catalog.KindPodcasts {
		return s.scanPodcasts(ctx, lib, res)
	}
	overrides, err := s.cat.FolderOverrides(ctx, lib.ID)
	if err != nil {
		return res, err
	}
	// Discovery walks the whole tree (slow on a large network share) and emits no
	// per-file output, so bookend it with logs - otherwise a long scan looks hung.
	s.log.Info("scan started: discovering books", "library", lib.Name, "root", lib.Root)
	ignore := ParseIgnore(lib.IgnorePatterns)
	books, epubs, partialDiscovery, err := discoverAuto(ctx, lib, overrides, ignore, s.log, rl, nil, s.walkWidth(), s.probeTimeout)
	if err != nil {
		return res, err
	}
	ebooksOfWalk(lib, books, epubs)
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
	// After a partial discovery a book not found is not known gone (its folder
	// unreadable, or on a share that stopped answering), so only one gone from
	// disk moves.
	moves := s.detectMoves(ctx, lib, sigs, books, keep, partialDiscovery, rl)
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
	// Unchanged books with a sidecar image, whose art version is read after the
	// loop, walkWidth at a time (one stat each: a round trip on a network share).
	var artChecks []artCheck
	suspectBackfill := map[string]int{}
	splitBackfill := map[string]string{}
	releasedBackfill := map[string]string{}
	hintsBackfill := map[string]catalog.TagHints{}
	// Books indexed before 0049 whose genre tag was never read: the genres a read
	// of the primary file finds (SetGenres), flushed like the path readings since
	// each one that has any re-resolves its book.
	genresBackfill := map[string][]string{}
	pathBackfill := map[string]*metadata.Metadata{}
	// Folder books whose metadata.json is new, changed, gone or never read, while
	// their audio is unchanged: read here and recorded without a re-index
	// (SetABSFiles), flushed like the path readings.
	absBackfill := map[string]catalog.ABSReading{}
	// Books whose .audiosilo/ attachments are new, changed, gone or never read,
	// while their audio is unchanged: likewise (SetAttachments).
	attachBackfill := map[string]catalog.AttachmentsReading{}
	// Folder books found unchanged whose file order key isn't recorded (indexed
	// before 0055, or since by an older server; Signatures read it from their
	// files this once): recorded without a re-index (SetFileOrders), so the next
	// scan reads none of their files.
	orderBackfill := map[string]uint64{}
	// Books found unchanged whose ebook is new, changed or gone (or never looked
	// for: indexed before 0056): recorded without a re-index (SetEbooks).
	ebookBackfill := map[string]catalog.EbookReading{}
	// Books found unchanged whose revision 1 to 4 snapshot needs no read
	// (Signature.RevStamp): stamped with today's revision (StampScannedRev), so
	// later scans don't test them again.
	var revBackfill []string
	slots := s.readSlots(s.readers())
	// The path, genre, tag hint, ABS, attachments, file order and ebook backfills are
	// written as they go: a large one (most of a library laid out Author/Title, or
	// every folder book of a library ABS has managed, read once after an upgrade)
	// can take a while, and a scan stopped part way keeps what it read (written past
	// the Stop: at most pathFlushEvery books).
	//
	// The unchanged books whose primary file a backfill reads (dates, genres,
	// tag hints, names): read a batch at a time, as many at once as the scan's
	// slots allow (one an upgrade asks of most of a library is not a file at a
	// time), then recorded in the backfills above.
	var tagReads []*tagRead
	readTags := func() {
		if len(tagReads) == 0 {
			return
		}
		pool.Each(ctx, slots.size(), tagReads, func() bool { return ctx.Err() == nil }, func(r **tagRead) {
			md := slots.extract(ctx, (*r).primary, s.ffprobePath)
			if ctx.Err() != nil {
				return // one a Stop may have cut short is dropped, for the next scan
			}
			(*r).md = md
			// A book of several files whose title turns on another file's tags
			// (partNamed): its last file's, read beside the first.
			if b := (*r).book; (*r).names && partNamed(b, md) {
				if last := slots.extract(ctx, absOf(lib, lastPath(b)), s.ffprobePath); ctx.Err() == nil {
					(*r).last = last
				}
			}
		})
		for _, r := range tagReads {
			md, b, old := r.md, r.book, r.old
			if md == nil || md.OpenErr != nil || md.TagsUnread() {
				// Stopped, unopenable now, or its tag read cut short (recording it would
				// record no date, genres, hints or names): the next scan reads it.
				continue
			}
			if old.ReleasedUnchecked {
				releasedBackfill[b.RelPath] = md.Released
			}
			if old.HintsUnchecked && (md.ProbeErr == nil || old.ScanError == problemProbe) {
				hintsBackfill[b.RelPath] = catalog.TagHints{ASIN: md.TagASIN, ISBN: md.TagISBN, Language: md.TagLanguage}
			}
			if old.GenresUnchecked {
				genresBackfill[b.RelPath] = md.Genres
			}
			// A title that turns on the last file's tags waits for them too:
			// unread now (stopped, unopenable, cut short), the next scan reads both.
			if r.names && (md.ProbeErr == nil || old.ScanError == problemProbe) && (!partNamed(b, md) || tagsRead(r.last)) {
				pathBackfill[b.RelPath] = readNames(b, md, r.last)
			}
		}
		clear(tagReads)
		tagReads = tagReads[:0]
	}
	flushBackfills := func() {
		readTags()
		if err := s.cat.SetPathReading(context.WithoutCancel(ctx), lib.ID, pathBackfill); err != nil {
			s.log.Warn("record path readings failed", "library", lib.Name, "err", err)
		}
		clear(pathBackfill)
		if err := s.cat.SetGenres(context.WithoutCancel(ctx), lib.ID, genresBackfill); err != nil {
			s.log.Warn("record genres failed", "library", lib.Name, "err", err)
		}
		clear(genresBackfill)
		if err := s.cat.SetTagHints(context.WithoutCancel(ctx), lib.ID, hintsBackfill); err != nil {
			s.log.Warn("record tag hints failed", "library", lib.Name, "err", err)
		}
		clear(hintsBackfill)
		if err := s.cat.SetABSFiles(context.WithoutCancel(ctx), lib.ID, absBackfill); err != nil {
			s.log.Warn("record Audiobookshelf metadata failed", "library", lib.Name, "err", err)
		}
		clear(absBackfill)
		if err := s.cat.SetAttachments(context.WithoutCancel(ctx), lib.ID, attachBackfill); err != nil {
			s.log.Warn("record .audiosilo attachments failed", "library", lib.Name, "err", err)
		}
		clear(attachBackfill)
		if err := s.cat.SetFileOrders(context.WithoutCancel(ctx), lib.ID, orderBackfill); err != nil {
			s.log.Warn("record file orders failed", "library", lib.Name, "err", err)
		}
		clear(orderBackfill)
		if err := s.cat.SetEbooks(context.WithoutCancel(ctx), lib.ID, ebookBackfill); err != nil {
			s.log.Warn("record ebooks failed", "library", lib.Name, "err", err)
		}
		clear(ebookBackfill)
		if err := s.cat.StampScannedRev(context.WithoutCancel(ctx), lib.ID, revBackfill); err != nil {
			s.log.Warn("record scanned revisions failed", "library", lib.Name, "err", err)
		}
		revBackfill = revBackfill[:0]
	}
	// The books to read are read a few files at a time (readFiles), each book on
	// its own goroutine, up to a window of books ahead of the one written next; the
	// index is written here alone, in discovery order (writeNext), so the rows,
	// their ids and the log come out as a scan reading one file at a time writes them.
	window := 2 * slots.size()
	var pending []*bookRead
	// The books read and waiting to be written: a few share a transaction
	// (writeBatch), written once there are writeBatch of them, they have waited
	// writeBatchAge, the writer would wait on a read, or the scan ends or stops.
	var batch []*bookRead
	var batchSince time.Time
	// The folders of books split across disc folders whose discs this scan has
	// counted as added: the discs of one are one book added, not one each
	// (PLAYER-08).
	addedSplit := map[string]bool{}
	// recordWrite counts a book written (err nil) or not, and logs its problem
	// or the failure.
	recordWrite := func(r *bookRead, err error) {
		b := r.book
		r.endFlight(s, err)
		if err != nil {
			s.log.Warn("index book failed", "library", lib.Name, "path", b.RelPath, "err", err)
			res.Errors++
			rl.add("error", "error", func(e *catalog.RunEvent) { e.Path, e.Detail = b.RelPath, err.Error() })
			return
		}
		s.noteUnprobed(lib.ID, b)
		if b.ScanError != "" {
			res.Errors++
			rl.add("warn", "problem", func(e *catalog.RunEvent) {
				e.Path, e.Code, e.Detail = b.ScanErrorFile, b.ScanError, b.ScanErrorDetail
			})
		}
		switch {
		case r.existed && b.ReadCutShort:
			// Kept whole: UpsertBook recorded only its read problem (counted
			// above), nothing of the book was updated.
		case r.existed:
			res.Updated++
		case !movedTo[b.RelPath] && !joinsIndexed(b, sigs) && split[b.RelPath] == "":
			// New content. A moved book is counted as moved, a book joined from disc
			// books already indexed is a reshape logged as joined (carryJoinedState),
			// and a disc split back out of a joined book one logged as split (below):
			// none is new, nor announced as added.
			if b.SplitParent != "" {
				if addedSplit[b.SplitParent] {
					return
				}
				addedSplit[b.SplitParent] = true
			}
			res.Added++
			if len(res.AddedTitles) < maxAddedTitles {
				res.AddedTitles = append(res.AddedTitles, b.Title)
			}
		}
	}
	// flush writes the batch, in order. It runs past a Stop: its books were read
	// before it (writeNext checks), and a stopped scan keeps what it read.
	flush := func() {
		if len(batch) == 0 {
			return
		}
		books := make([]*catalog.Book, len(batch))
		for i, r := range batch {
			books[i] = r.book
		}
		for i, err := range s.cat.UpsertBooks(context.WithoutCancel(ctx), books, writeBatchAge) {
			recordWrite(batch[i], err)
		}
		clear(batch)
		batch = batch[:0]
	}
	// writeNext takes the oldest book read into the batch, once its read is done
	// (writing the batch first rather than waiting with it); ctx's error when the
	// scan was stopped by then, or is while it waits (the read may have been cut
	// short, or be held in a call on a share that stopped answering: stopped
	// leaves it).
	writeNext := func() error {
		r := pending[0]
		if !r.finished() {
			flush()
		}
		select {
		case <-r.done:
		case <-ctx.Done():
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		pending = pending[1:]
		if len(batch) == 0 {
			batchSince = time.Now()
		}
		batch = append(batch, r)
		if s.batched != nil {
			s.batched(len(batch))
		}
		if len(batch) >= writeBatch {
			flush()
		}
		return nil
	}
	// stopped ends the scan at a Stop (or its time limit): the batch is written,
	// the reads under way are dropped (one cut short by the stop would record a
	// false read problem), nothing more is written, and the backfills read so far
	// are kept. A read ends at the stop (its looks at the files are bounded by
	// ctx), so it is waited for up to stopGrace; one still under way then (held
	// in a call a share that stopped answering never returns from) is left to
	// end on its own, its book's flight with it, and counted
	// (Scanner.readsLeft), so the scan ends and the next one runs: its flight
	// marked left (bookRead.left), so the next scan keeps that book as stored
	// rather than wait for it (keepLeft).
	stopped := func(err error) (*ScanResult, error) {
		flush()
		grace := time.NewTimer(stopGrace)
		defer grace.Stop()
		left := 0
		for _, r := range pending {
			if left == 0 {
				select {
				case <-r.done:
					r.endFlight(s, errReadDropped)
					continue
				case <-grace.C:
				}
			} else if r.finished() {
				r.endFlight(s, errReadDropped)
				continue
			}
			left++
			r.left.Store(true)
			s.readsLeft.Add(1)
			go func() {
				<-r.done
				r.endFlight(s, errReadDropped)
				s.readsLeft.Add(-1)
			}()
		}
		if left > 0 {
			s.log.Warn("scan stopped with reads still under way, left to end on their own",
				"library", lib.Name, "reads", left, "left_by_all_scans", s.readsLeft.Load())
		}
		flushBackfills()
		return res, err
	}
	lastLog := time.Now()
	report := func(done int) {
		s.updateProgress(lib.ID, func(p *ScanProgress) {
			p.Total, p.Done, p.Indexed = len(books), done, res.Added+res.Updated
			p.Added, p.Updated, p.Moved, p.Removed = res.Added, res.Updated, res.Moved, res.Removed
		})
	}
	for i, b := range books {
		if err := ctx.Err(); err != nil {
			return stopped(err)
		}
		// Write the books read by now, and wait for the oldest once the window is full.
		for len(pending) > 0 && (len(pending) >= window || pending[0].finished()) {
			if err := writeNext(); err != nil {
				return stopped(err)
			}
		}
		if len(batch) > 0 && time.Since(batchSince) >= writeBatchAge {
			flush()
		}
		done := i - len(pending) - len(batch)
		report(done)
		// Heartbeat so a long indexing pass (ffprobe per file over a network share)
		// shows progress in the logs, not just in the admin UI counter.
		if time.Since(lastLog) >= 15*time.Second {
			s.log.Info("scan progress", "library", lib.Name, "done", done, "total", len(books), "indexed", res.Added+res.Updated)
			lastLog = time.Now()
		}
		old, existed := sigs[b.RelPath]
		if existed && old.MTime == b.MTime && old.Size == b.Size &&
			(!b.IsFolder || old.FileOrder == catalog.FileOrderKey(b.Files)) && !old.DurationStale &&
			((s.ffprobePath == "" && !s.rereadUnprobed(lib.ID, b)) || (old.Duration > 0 && old.Codec != "") || soleFileProblem(b, old)) &&
			!s.problemCleared(ctx, lib, old, slots) {
			// Unchanged since last scan, its files in the order stored (a book
			// indexed in byte order before #160 is read again in natural order),
			// its lengths read by today's revision (an MP3 book indexed before
			// SRV-16 is read again once, its header-less VBR files counted);
			// only skip the probe when a prior probe already stored both duration
			// and codec (so books indexed before the codec column get it
			// backfilled), or, with no ffprobe, the in-process probe has had its
			// one go at the book this run (rereadUnprobed), or the book's one
			// file has a read problem that problemCleared just found still there:
			// reading the book again would find the same.
			// Whether it is a disc of a split book turns on its siblings, so it can
			// change while the book doesn't (and starts unset before migration 0022).
			if old.SplitParent != b.SplitParent {
				splitBackfill[b.RelPath] = b.SplitParent
			}
			if b.IsFolder && old.FileOrderUnset {
				orderBackfill[b.RelPath] = old.FileOrder // the order found, as the check above says
			}
			if old.ContentHash == "" {
				// The fingerprint never landed (a one-off read error). Without one a
				// later move of this book couldn't be detected and its progress would
				// be stranded, so re-enrich to backfill it - but probe the fingerprint
				// first: if the file still can't be read, the full re-enrich would fail
				// the same way, so a persistently unreadable book costs one cheap
				// failed open per scan instead of an ffprobe sweep forever, and is
				// otherwise kept as any unchanged book is: the metadata files beside
				// it (an ABS metadata.json, attachments, an ebook) are still read below.
				b.ContentHash = s.fingerprint(ctx, absOf(lib, primaryPath(b)))
			}
			if old.ContentHash != "" || b.ContentHash == "" {
				// Rows indexed before migration 0016 have no cover flag; fill it with a
				// tag read (no ffprobe) rather than re-indexing the book.
				// A file that can't be opened or read right now (a flaky mount, a share
				// that stopped answering: the read is bounded) is left unknown for the
				// next scan rather than recorded as having no cover; one whose bounded
				// fingerprint just failed is not read again (a share that stopped
				// answering, a FIFO).
				primary := absOf(lib, primaryPath(b))
				fingerprintFailed := old.ContentHash == "" && b.ContentHash == ""
				if old.HasCover == nil && !fingerprintFailed {
					if art, _, err := media.EmbeddedCover(ctx, primary, s.probeTimeout); err == nil {
						coverBackfill[b.RelPath] = art != nil
					}
				}
				// A sidecar image replaced in place changes no audio: its own
				// version moves the book's cover_version (never back to one it had
				// for other art). A custom cover's identity is left alone.
				if old.CoverPath != "" && !strings.HasPrefix(old.CoverArt, "c") {
					artChecks = append(artChecks, artCheck{rel: b.RelPath, cover: old.CoverPath, was: old.CoverArt})
				}
				// Rows indexed before 0017 haven't been checked for holding several
				// books; the same kind of tag read does it.
				if old.SuspectUnchecked && b.IsFolder {
					if n, ok := suspectFromTags(ctx, lib, b, s.probeTimeout); ok {
						suspectBackfill[b.RelPath] = n
					}
				}
				// Rows indexed before 0037 haven't had their date read, nor rows
				// before 0049 their genre, and some books one folder deep, read when
				// that folder was taken for their series, need their tags to tell
				// (catalog.PathRead): one read of the primary file (tags and ffprobe,
				// as enrich reads it) does all three, not a re-index probing every
				// part. Unopenable now, it waits for the next scan; and
				// the names wait while ffprobe fails where it didn't before, as the
				// series it alone reads (an MP3's TXXX) would be recorded as none.
				// One whose names need its tags too (NamesCheck, below) waits for
				// that read: the snapshot alone would record today's revision with
				// the title or the leaf's series unread.
				if old.PathCheck == catalog.PathFromSnapshot && !old.NamesCheck {
					pathBackfill[b.RelPath] = nil
				}
				// Rows indexed before 0067 haven't had their ASIN, ISBN and language
				// tags read: the same read, and they wait with the names, as the
				// hints only ffprobe reads (an M4B's mdta items) would be recorded as none.
				// A title that may be the album naming the series (before revision 2)
				// or a leaf that states a series (before revision 3:
				// catalog.Signature.NamesCheck) is the same read.
				readPath := old.PathCheck == catalog.PathRead || old.NamesCheck
				if old.RevStamp {
					revBackfill = append(revBackfill, b.RelPath)
				}
				if old.ReleasedUnchecked || old.GenresUnchecked || old.HintsUnchecked || readPath {
					tagReads = append(tagReads, &tagRead{book: b, old: old, primary: primary, names: readPath})
					if len(tagReads) >= slots.size()*tagReadsPerSlot {
						readTags()
					}
				}
				// A file new, changed or gone since the last read, or one not read for
				// this index (none to read leaves an unread row as it is: nothing to add).
				// One that can't be looked at or read right now keeps the row's reading
				// for the next scan.
				if b.IsFolder && !b.ABS.Unread && (old.ABSSig != b.ABS.Sig || (old.ABSUnchecked && b.ABS.Sig != "")) {
					if f, read := s.readABS(ctx, lib, b.RelPath, b.ABS.Sig, nil); read {
						absBackfill[b.RelPath] = catalog.ABSReading{Sig: b.ABS.Sig, File: f}
					}
				}
				// The Bridge's attachments, the same way (any book: a single-file book's
				// sit beside it). A folder that couldn't be listed keeps what is stored.
				if !b.Attachments.Unread &&
					(old.AttachmentsSig != b.Attachments.Sig || (old.AttachmentsUnchecked && b.Attachments.Sig != "")) {
					if files, read := s.readAttachments(ctx, lib, b, b.Attachments.Sig, nil); read {
						// Recorded only over the state read when the scan began: a
						// refresh by the Bridge since then is newer than this read.
						attachBackfill[b.RelPath] = catalog.AttachmentsReading{Sig: b.Attachments.Sig, Files: files,
							Was: &catalog.AttachmentsState{Sig: old.AttachmentsSig, Unchecked: old.AttachmentsUnchecked}}
					}
				}
				// The ebook beside it (the reader's): a new, changed or gone one is
				// checked and recorded; one that can't be read right now keeps what is
				// stored, for the next scan.
				if old.EbookSig != b.EbookFile.Sig {
					if r, ok := checkEbook(ctx, lib.Root, b.EbookFile, s.probeTimeout, nil); ok {
						// Recorded only over the row as this scan read it: an index of
						// the book since (on demand) recorded a newer reading.
						r.Was = &catalog.EbookState{Sig: old.EbookSig, IndexedAt: old.IndexedAt}
						ebookBackfill[b.RelPath] = r
					}
				}
				if len(pathBackfill)+len(genresBackfill)+len(hintsBackfill)+len(absBackfill)+len(attachBackfill)+len(revBackfill)+
					len(orderBackfill)+len(ebookBackfill) >= pathFlushEvery {
					flushBackfills()
				}
				continue
			}
		}
		if existed {
			// Its row keeps the added_at it was first indexed with (UpsertBook never
			// updates it), so its files' birth times aren't read (stampAddedAt); one
			// whose row has gone since the scan began is first seen now.
			b.AddedAt = now()
		}
		r := &bookRead{book: b, existed: existed, done: make(chan struct{})}
		pending = append(pending, r)
		go func() {
			defer close(r.done)
			// Not beside an on-demand read of the book (indexShared, IndexMoved),
			// and one asked for while this read is under way waits for its write.
			// A read of it that has not returned in time is not waited on further:
			// the book is kept as stored for this scan.
			f, err := s.startFlight(ctx, bookKey{lib.ID, b.RelPath}, r)
			if errors.Is(err, errReadLeft) {
				s.keepLeft(ctx, lib, b, old)
				return
			}
			if err != nil {
				return // the scan stopped: writeNext sees it
			}
			r.flight = f
			s.enrich(metadata.WithProgress(ctx, f.progress), lib, b, slots)
		}()
	}
	for len(pending) > 0 {
		if err := writeNext(); err != nil {
			return stopped(err)
		}
		report(len(books) - len(pending) - len(batch))
	}
	flush()
	report(len(books))
	if err := s.cat.SetHasCover(ctx, lib.ID, coverBackfill); err != nil {
		s.log.Warn("record cover flags failed", "library", lib.Name, "err", err)
	}
	if err := s.cat.SetArtVersions(ctx, lib.ID, artChanges(ctx, lib.Root, artChecks, s.walkWidth(), s.probeTimeout)); err != nil {
		s.log.Warn("record cover art versions failed", "library", lib.Name, "err", err)
	}
	if err := s.cat.SetSuspectParts(ctx, lib.ID, suspectBackfill); err != nil {
		s.log.Warn("record suspect folders failed", "library", lib.Name, "err", err)
	}
	if err := s.cat.SetSplitParent(ctx, lib.ID, splitBackfill); err != nil {
		s.log.Warn("record split discs failed", "library", lib.Name, "err", err)
	}
	readTags() // the release dates' reads, before they are recorded
	if err := s.cat.SetReleased(ctx, lib.ID, releasedBackfill); err != nil {
		s.log.Warn("record release dates failed", "library", lib.Name, "err", err)
	}
	flushBackfills()

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
	// The ebooks of books with a read-along map that have no digest for the file
	// they have now (a map new or read again, an ebook changed): only those, as
	// hashing every ebook would read whole libraries over a network share.
	s.hashReadalongEbooks(ctx, lib, "")
	s.log.Info("library scanned", "library", lib.Name,
		"added", res.Added, "updated", res.Updated, "moved", res.Moved, "removed", res.Removed,
		"elapsed", time.Since(start))
	return res, nil
}

// writeBatch is how many books a scan writes in one transaction at most, and
// writeBatchAge how long a book read waits to be written, and so how long one
// of those transactions holds the writer, at most (catalog.UpsertBooks'
// budget). A transaction's begin and commit cost a scan about as much as a
// book's own writes (0.6 of 1.5 ms a book on a synthetic library, 1.3 of 3.9
// ms on a real one: SCAN-SPIKE.md); shared by 64 books they cost a few percent
// of that, and more books a transaction gain little while holding back every
// other write (a listener's progress) for longer. A scan whose reads are slower than its
// writes (ffprobe on every file) writes each book as its read is done, as
// before: the writer never waits on a read holding books.
const (
	writeBatch    = 64
	writeBatchAge = 250 * time.Millisecond
)

// stopGrace is how long a stopped scan waits for its reads under way to end
// before it leaves them (stopped).
const stopGrace = 2 * time.Second

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

// unreadable records a warning that the library-relative path couldn't be
// read (err's cause), the run's "unreadable" event; on a nil log it does nothing.
func (l *runLog) unreadable(rel string, err error) {
	l.add("warn", "unreadable", func(e *catalog.RunEvent) { e.Path, e.Detail = rel, fsaccess.Cause(err) })
}

// add records an event (fill sets its facts), or counts it once the log, or its
// share for problems, is full; on a nil log it does nothing. The cap leaves room
// for the truncation note and the closing event.
func (l *runLog) add(level, kind string, fill func(*catalog.RunEvent)) {
	if l == nil {
		return // a move outside a scan (IndexMoved): the server log has it
	}
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

// pathFlushEvery is how many path readings a scan gathers before writing them.
const pathFlushEvery = 500

// readNames is book b's title, author and series: the path's reading
// (metadata.DeriveFromPath, the series and position a leaf named "Author;
// Series; Book NN[; Title]" states included) as the baseline, then md's tags,
// which are authoritative when present (an unreadable file's md carries none;
// nil reads the same), except a book of several files' first title tag that
// names that file (metadata.PartsTitle: the album instead, unless the book's
// folder or last, its last file's tags, nil for unread, says the tag is the
// book's, or the two tags share a name past the album), and an album tag that
// only names the book's series, which gives way to the book's own title
// (metadata.TitleNotSeries), and md's narrator. So the leaf's series fills
// only a series the tags leave out.
func readNames(b *catalog.Book, md, last *metadata.Metadata) *metadata.Metadata {
	n := metadata.DeriveFromPath(b.RelPath, b.IsFolder)
	if md == nil {
		return n
	}
	title := md.Title
	if len(b.Files) > 1 {
		title = md.PartsTitle(metadata.PathTitle(b.RelPath, b.IsFolder), last)
	}
	n.Title = chooseTitle(title, n.Title)
	if strings.TrimSpace(md.Author) != "" {
		n.Author = md.Author
	}
	if strings.TrimSpace(md.Series) != "" {
		n.Series = md.Series
	}
	if md.SeriesIndex != 0 {
		n.SeriesIndex, n.SeriesIndexEnd = md.SeriesIndex, md.SeriesIndexEnd
	}
	n.Title = metadata.TitleNotSeries(n.Title, md, b.RelPath, b.IsFolder, len(b.Files) > 1)
	n.Narrator = md.Narrator
	return n
}

// partNamed reports whether book b's title turns on the tags of another of its
// files (metadata.PartNamed): a book of several files whose first file's tags,
// md, title it with a tag that may name that file. A scan then reads its last
// file's (lastPath) for readNames.
func partNamed(b *catalog.Book, md *metadata.Metadata) bool {
	return len(b.Files) > 1 && md.PartNamed(metadata.PathTitle(b.RelPath, b.IsFolder))
}

// lastPath is the last of a book of several files' files, whose tags tell
// whether the first file's title tag is the book's (partNamed).
func lastPath(b *catalog.Book) string { return b.Files[len(b.Files)-1].RelPath }

// tagsRead reports whether md is a file's tags as read: not left unread by a
// Stop (nil), an open that failed or a read cut short.
func tagsRead(md *metadata.Metadata) bool {
	return md != nil && md.OpenErr == nil && !md.TagsUnread()
}

// setNames sets b's title, author, series and position (readNames), its
// narrator and its tag hints (the ASIN, ISBN and language the tags give, never
// its identifiers) from md, its primary file's tags (nil for none), and last,
// its last file's where its title turns on them (partNamed; nil for unread):
// the names a scan reads, which enrich and the setup preview (Preview) share.
func setNames(b *catalog.Book, md, last *metadata.Metadata) {
	n := readNames(b, md, last)
	b.Title, b.Author, b.Series, b.SeriesIndex, b.SeriesIndexEnd = n.Title, n.Author, n.Series, n.SeriesIndex, n.SeriesIndexEnd
	if md != nil {
		b.Narrator = n.Narrator
		b.TagASIN, b.TagISBN, b.TagLanguage = md.TagASIN, md.TagISBN, md.TagLanguage
	}
}

// enrich fills metadata for a book from its primary file (tags + ffprobe) and
// folder context (path heuristics, sibling cover art, an Audiobookshelf
// metadata.json in a folder book's folder, when the walk found one: b.ABS.Sig,
// and the book's .audiosilo/ attachments: b.Attachments.Sig), and, unless the
// caller set it, when it first appeared on disk (stampAddedAt), which discovery
// leaves unread.
//
// A scan reads with its slots (an on-demand index with one, a file at a time)
// and its ctx, which ends the ffprobe runs at a Stop: the scan then drops
// the reading rather than writing it.
func (s *Scanner) enrich(ctx context.Context, lib catalog.Library, b *catalog.Book, slots *readSlots) {
	look := s.lookTimeout(ctx)
	if b.AddedAt == "" {
		stampAddedAt(ctx, lib, b, look)
	}
	primary := primaryPath(b)
	abs := absOf(lib, primary)
	md := slots.extract(ctx, abs, s.ffprobePath)
	if ctx.Err() != nil {
		return // stopped: the scan drops this reading, so read no more of the book
	}
	b.ScanError, b.ScanErrorFile, b.ScanErrorDetail = "", "", ""
	if !b.IsFolder {
		noteProblem(b, primary, b.Size, md)
	}
	// What the next scan's problemCleared asks of the file: a book's only file is
	// in the book's state as discovery found it, before this read; the first of
	// several parts' own state isn't known here (nil).
	var at *fileState
	if soleFile(b) {
		at = &fileState{mtime: b.MTime, size: b.Size}
	}
	s.noteRejected(abs, at, md)
	// A read cut short or refused (a share that stopped answering), or one whose
	// length ran out of time (readCutShort), knows nothing of the file, or not
	// its length: UpsertBook then rewrites nothing of a stored book but its read
	// problem, rather than path-only names and no length, codec, chapters,
	// cover, date or genres; a new book is written with what there is. A folder
	// book's other parts count too (buildMultiFileChapters).
	b.ReadCutShort = readCutShort(md)
	// Only a length out of time, with a part added, gone or changed since, is
	// written whole after all (catalog.Book.LengthOnly); a folder book's parts
	// say which it is (buildMultiFileChapters).
	b.LengthOnly = b.ReadCutShort && !md.TagsUnread()
	// A header-less VBR MP3 whose frames went uncounted (its length ffprobe's
	// estimate) leaves the book's length revision unstamped, so the next scan
	// reads it again; a folder book's other parts count too.
	b.LengthUncounted = md.LengthUncounted()
	setNames(b, md, nil)
	if md != nil {
		b.Released = md.Released
		b.Genres = md.Genres
		b.Codec = md.Codec
	}

	// The files beside the audio are looked at and read under the bounds of its
	// read (metadata.Bounded): one given up on (a share that stopped answering)
	// keeps the book as stored with that file's problem (givenUp.keep). With the
	// audio's own read cut short, or once one is given up on, no more is tried:
	// on a share that stopped answering each would only wait out its time too,
	// and the book is kept as stored anyway (a new one written with what there
	// is, the rest read as none for now).
	var gaveUp givenUp
	sideReads := func() bool { return !md.TagsUnread() && gaveUp.err == nil }
	// The ABS file: one that couldn't be looked at or read right now keeps what
	// is stored (ABSReading.Unread), for the next scan to read again.
	if b.IsFolder && !b.ABS.Unread {
		read := false
		if sideReads() {
			b.ABS.File, read = s.readABS(ctx, lib, b.RelPath, b.ABS.Sig, &gaveUp)
		}
		if !read {
			b.ABS = catalog.ABSReading{Unread: true}
		}
	}
	// The attachments: one that couldn't be read right now (or whose folder
	// couldn't be listed) keeps what is stored, for the next scan to read again
	// (UpsertBook and AttachmentsReading.Unread), never a part of the set.
	if !b.Attachments.Unread {
		files, read := metadata.AttachmentFiles{}, false
		if sideReads() {
			files, read = s.readAttachments(ctx, lib, b, b.Attachments.Sig, &gaveUp)
		}
		if read {
			b.Attachments.Files = files
		} else {
			b.Attachments = catalog.AttachmentsReading{Unread: true}
		}
	}

	// The ebook beside it: one that can't be read right now keeps what is stored
	// (Unread; a new row gets none), as a scan that finds the book unchanged
	// does, so a re-index on a flaky mount never takes a recorded ebook away; the
	// next scan looks again at one that changed.
	ebook, read := catalog.EbookReading{}, false
	if sideReads() {
		ebook, read = checkEbook(ctx, lib.Root, b.EbookFile, look, &gaveUp)
	}
	if read {
		b.EbookFile = ebook
	} else {
		b.EbookFile = catalog.EbookReading{Unread: true}
	}

	// Move-detection fingerprint (reused content_hash column); skip if already
	// computed during move detection.
	if b.ContentHash == "" && !md.TagsUnread() {
		b.ContentHash = s.fingerprint(ctx, abs)
	}

	// Normalize chapters so single-file and multi-file books look the same to a
	// client (see metadata.Chapter).
	if b.IsFolder {
		last := s.buildMultiFileChapters(ctx, lib, b, md, slots)
		if partNamed(b, md) {
			// Named again now its last file's tags are read: a title tag the
			// first file shares with it is the book's (metadata.PartsTitle).
			setNames(b, md, last)
		}
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
	if sideReads() {
		cover := coverLook{ctx: ctx, root: lib.Root, timeout: look, gaveUp: &gaveUp}
		bookAbs := absOf(lib, b.RelPath)
		if b.IsFolder {
			b.CoverPath = cover.find(bookAbs, true)
			// A multi-CD book's art often sits in the parent (e.g. ".../Book/CD1" with
			// the cover in ".../Book"); fall back there for disc-part subfolders.
			if b.CoverPath == "" && metadata.IsDiscFolder(filepath.Base(bookAbs)) {
				b.CoverPath = cover.find(filepath.Dir(bookAbs), true)
			}
			// A folder joined from its disc folders: its own art first (above), then the
			// first disc's.
			if first := dirOf(primary); b.CoverPath == "" && first != b.RelPath {
				b.CoverPath = cover.find(absOf(lib, first), true)
			}
		} else {
			b.CoverPath = cover.find(filepath.Dir(bookAbs), false)
		}
	}
	// Embedded art; UpsertBook adds a sibling cover (CoverPath) to the flag itself.
	hasCover := md != nil && md.HasCover
	b.HasCover = &hasCover
	if sideReads() {
		b.ArtVersion = artVersion(ctx, lib.Root, b.ArtFiles(), look, &gaveUp)
	}
	gaveUp.keep(b)
}

// keepLeft is a scan's reading of book b when another read of it may never
// return (errReadLeft: one a stopped scan left under way, or an on-demand one
// not ended in time): nothing of it is read, so it is kept as stored
// (ReadCutShort; a new one written with its path's names) with an unreadable
// problem, the one stored when there is one (old) or else on its first audio
// file, so Health lists it and the next scan looks at it again; the rest of
// the library scans meanwhile.
func (s *Scanner) keepLeft(ctx context.Context, lib catalog.Library, b *catalog.Book, old catalog.Signature) {
	if b.AddedAt == "" {
		stampAddedAt(ctx, lib, b, s.probeTimeout)
	}
	setNames(b, nil, nil)
	if b.IsFolder {
		b.ABS = catalog.ABSReading{Unread: true}
	}
	b.Attachments = catalog.AttachmentsReading{Unread: true}
	b.EbookFile = catalog.EbookReading{Unread: true}
	held := givenUp{file: primaryPath(b), err: metadata.ErrNotAnswered}
	if old.ScanError == problemUnreadable && old.ScanErrorFile != "" {
		held.file = old.ScanErrorFile
	}
	held.keep(b)
}

// ArtFile is the file a book's own art (no custom cover) is read from, the one
// rule the cover handler serves and versions it by: its sidecar image (CoverPath) inside root,
// else the audio file embedded art lives in. kind is catalog.ArtSidecar or
// catalog.ArtEmbedded; ok is false when neither path is inside root.
func ArtFile(root string, src catalog.CoverSource) (kind, abs string, ok bool) {
	if src.CoverPath != "" {
		if abs, err := SafeJoin(root, src.CoverPath); err == nil {
			return catalog.ArtSidecar, abs, true
		}
	}
	if abs, err := SafeJoin(root, src.AudioPath); err == nil {
		return catalog.ArtEmbedded, abs, true
	}
	return "", "", false
}

// Art is a book's own art file (ArtFile) as LookArt found it: its kind
// (catalog.ArtSidecar or catalog.ArtEmbedded), its absolute path and its stat.
type Art struct {
	Kind, Abs string
	Info      fs.FileInfo
}

// Version is the art's version as the cover handler names it
// (catalog.FileArtVersion); "" when it is no regular file.
func (a Art) Version() string {
	if a.Info == nil || !a.Info.Mode().IsRegular() {
		return ""
	}
	return catalog.FileArtVersion(a.Kind, a.Info.Size(), a.Info.ModTime())
}

// LookArt is ArtFile with the file's stat, the two (SafeJoin's lstats and the
// stat) one metadata.Bounded call under ctx, given up on after timeout, so art
// on a share that stopped answering costs a scan or a cover request at most
// that (metadata.GaveUp). err is fs.ErrNotExist when no file inside root is
// named or it is gone, else the stat's or a look given up on.
func LookArt(ctx context.Context, root string, src catalog.CoverSource, timeout time.Duration) (Art, error) {
	at := filepath.Join(root, filepath.FromSlash(cmp.Or(src.CoverPath, src.AudioPath)))
	return metadata.Bounded(ctx, at, timeout, func() (Art, error) {
		kind, abs, ok := ArtFile(root, src)
		if !ok {
			return Art{}, fs.ErrNotExist
		}
		info, err := os.Stat(abs)
		return Art{Kind: kind, Abs: abs, Info: info}, err
	}, nil)
}

// artVersion is the version of src's art file (ArtFile) as the cover handler
// names it (Art.Version). "" when that file can't be read now, its look given
// up on after timeout included (LookArt; noted in gaveUp).
func artVersion(ctx context.Context, root string, src catalog.CoverSource, timeout time.Duration, gaveUp *givenUp) string {
	art, err := LookArt(ctx, root, src, timeout)
	gaveUp.note(cmp.Or(src.CoverPath, src.AudioPath), err)
	if err != nil {
		return ""
	}
	return art.Version()
}

// artCheck is an unchanged book whose sidecar image may have been replaced in
// place: its path, the image's, and the cover art identity stored.
type artCheck struct {
	rel, cover, was string
	now             string
}

// artChanges reads the art version of each check's sidecar image, width at a
// time, each look given up on after timeout, and returns those that moved, by
// book path.
func artChanges(ctx context.Context, root string, checks []artCheck, width int, timeout time.Duration) map[string]catalog.ArtChange {
	pool.Each(ctx, min(width, len(checks)), checks, func() bool { return true }, func(c *artCheck) {
		c.now = artVersion(ctx, root, catalog.CoverSource{CoverPath: c.cover}, timeout, nil)
	})
	out := map[string]catalog.ArtChange{}
	for _, c := range checks {
		if c.now != "" && c.now != c.was {
			out[c.rel] = catalog.ArtChange{Was: c.was, Now: c.now}
		}
	}
	return out
}

// chooseTitle prefers a meaningful embedded title, falling back to the
// path-derived title when the embedded one is missing or generic ("Track 01").
func chooseTitle(embedded, pathDerived string) string {
	if embedded != "" && !metadata.IsGenericTitle(embedded) {
		return embedded
	}
	return pathDerived
}

// imageExts are image types accepted as a fallback cover when no conventionally
// named file is present.
var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}

// coverLook is how enrich looks for a book's cover image (find): each stat and
// listing a metadata.Bounded call under ctx, given up on after timeout and
// noted in gaveUp, in a library at root.
type coverLook struct {
	ctx     context.Context
	root    string
	timeout time.Duration
	gaveUp  *givenUp
}

// find returns a library-relative path to a book's cover image in dir: a
// conventionally-named file (cover.jpg, folder.png, …) if present, else - only
// when anyImage is set (a book's own folder, where a stray image is almost
// certainly its cover) - the first image file alphabetically. A hidden file is
// never a cover (isHidden: macOS writes an AppleDouble "._Book - Cover.jpg"
// beside each image on a share, a resource fork, not an image, and it sorts
// first). Returns "" if none, so the cover handler falls back to embedded art,
// and "" too, with no more looks, once one was given up on.
func (l coverLook) find(dir string, anyImage bool) string {
	if l.gaveUp != nil && l.gaveUp.err != nil {
		return ""
	}
	relOf := func(p string) string {
		rel, _ := filepath.Rel(l.root, p)
		return filepath.ToSlash(rel)
	}
	for _, name := range coverNames {
		p := filepath.Join(dir, name)
		_, err := metadata.StatBounded(l.ctx, p, l.timeout)
		if err == nil {
			return relOf(p)
		}
		if metadata.Unanswered(l.ctx, err) {
			l.gaveUp.note(relOf(p), err)
			return ""
		}
	}
	if !anyImage {
		return ""
	}
	entries, err := metadata.ReadDirBounded(l.ctx, dir, l.timeout) // sorted by name
	if err != nil {
		l.gaveUp.note(relOf(dir), err)
		return ""
	}
	var firstImage, coverish string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || isHidden(name) || !imageExts[strings.ToLower(filepath.Ext(name))] {
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
	return relOf(filepath.Join(dir, pick))
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
// first is the reading of its first part (enrich's of the primary file), so that
// file is read once; the others are read with slots (readFiles), then the
// chapters are built from them in file order. It returns the reading of the
// book's last part (first's for a book of one; nil when a Stop left it unread).
func (s *Scanner) buildMultiFileChapters(ctx context.Context, lib catalog.Library, b *catalog.Book, first *metadata.Metadata, slots *readSlots) *metadata.Metadata {
	var cum float64
	idx := 0
	parts := make([]partFacts, 0, len(b.Files))
	mds := s.readFiles(ctx, lib, b.Files, first, slots)
	tagsUnread := false
	for i := range b.Files {
		f := &b.Files[i]
		md := mds[i]
		tagsUnread = tagsUnread || md.TagsUnread()
		b.LengthUncounted = b.LengthUncounted || md.LengthUncounted()
		f.LengthUnread = lengthUnread(md)
		if readCutShort(md) && !b.ReadCutShort {
			// Its length and chapters unknown: see enrich. The book is kept as
			// stored, and what brings the next scan back to it is the problem
			// recorded (problemCleared), so this part's goes over an earlier
			// part's lasting one, which would never clear.
			b.ReadCutShort = true
			b.ScanError, b.ScanErrorFile, b.ScanErrorDetail = "", "", ""
		}
		noteProblem(b, f.RelPath, f.Size, md)
		if i > 0 { // the first is enrich's reading, noted there
			s.noteRejected(absOf(lib, f.RelPath), nil, md)
		}
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
	b.LengthOnly = b.ReadCutShort && !tagsUnread
	n := suspectParts(parts)
	b.SuspectParts = &n
	if len(mds) == 0 {
		return first
	}
	return mds[len(mds)-1]
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
//
// limit bounds the walk (nil for none, as a scan walks): once it is reached the
// books of the folders walked so far come back with errWalkCapped. width is how
// many filesystem calls the walk makes at once (Scanner.walkWidth), and timeout
// how long each may take (metadata.Bounded; the probe's time for a scan): a
// folder on a share that stopped answering is skipped for this walk, which is
// then partial (its books kept as stored), and logged and recorded as an
// unreadable path. epubs are
// the walk's .epub entries by folder (sidecarFiles.epubs), which a scan turns
// into its books' ebook candidates (ebooksOfWalk) and a setup preview ignores.
func discoverAuto(ctx context.Context, lib catalog.Library, overrides map[string]string, ignore *Ignore, log *slog.Logger, rl *runLog, limit *walkCap, width int, timeout time.Duration) (books []*catalog.Book, epubs map[string][]fs.DirEntry, hadErrors bool, err error) {
	walkRoot := lib.Root
	if resolved, rerr := filepath.EvalSymlinks(lib.Root); rerr == nil {
		walkRoot = resolved
	}
	dirs, side, hadErrors, err := audioDirs(ctx, lib, walkRoot, "", ignore, log, rl, limit, width, timeout)
	if err != nil && !errors.Is(err, errWalkCapped) {
		return nil, nil, hadErrors, err
	}
	gone, unanswered, serr := statAudio(ctx, dirs, width, timeout)
	if serr != nil {
		return nil, nil, hadErrors, serr
	}
	if len(unanswered) > 0 {
		// Folders with a file whose look was given up on: their books drop out
		// of this walk (built without that file, one would lose a part), so it
		// is partial, as with a folder that wouldn't list.
		hadErrors = true
		for dir, uerr := range unanswered {
			log.Warn("skipping a folder whose files didn't answer during discovery", "library", lib.Name, "path", absOf(lib, dir), "err", uerr)
			rl.unreadable(dir, uerr)
		}
	}
	// A folder whose listing was given up on (logged by the walk) is no less
	// unknown: a disc set it is a disc of can't be built from the rest.
	maps.Copy(unanswered, side.unanswered)
	dropUnanswered(dirs, unanswered)
	if gone > 0 {
		// Files listed by the walk but no longer there when looked at: the tree
		// changed under it (a book folder renamed or moved once listed). Their
		// books drop out of this walk, so it is partial, as with an unreadable
		// folder: a prune now would delete a book the next scan finds moved.
		hadErrors = true
		log.Warn("audio files gone during discovery", "library", lib.Name, "files", gone)
	}
	books = booksOf(lib, dirs, overrides)
	for _, b := range books {
		if b.IsFolder {
			b.ABS = side.abs[b.RelPath]
		}
	}
	attachmentsOfWalk(books, side)
	return books, side.epubs, hadErrors, err
}

// ebooksOfWalk sets each book's ebook candidate (EbookFile: the path BookEbook's
// rule names and its signature, not yet checked for readability: checkEbook)
// from the .epub entries the discovery walk found (audioDirs), by the folder
// holding them. A single-file book is handed only the entries named in some case
// or Unicode form of its "<stem>.epub" (all ebookCandidate looks at), found through a name index
// made once per folder, so a folder of many books and their epubs isn't searched
// once per book.
func ebooksOfWalk(lib catalog.Library, books []*catalog.Book, epubs map[string][]fs.DirEntry) {
	if len(epubs) == 0 {
		return
	}
	named := map[string]map[string][]fs.DirEntry{} // folder -> ebookNameKey -> entries
	for _, b := range books {
		dir := sidecarDir(b.RelPath, b.IsFolder)
		listed := epubs[dir]
		if len(listed) == 0 {
			continue
		}
		if !b.IsFolder {
			byName := named[dir]
			if byName == nil {
				byName = make(map[string][]fs.DirEntry, len(listed))
				for _, e := range listed {
					key := ebookNameKey(e.Name())
					byName[key] = append(byName[key], e)
				}
				named[dir] = byName
			}
			if listed = byName[ebookNameKey(ebookName(b.RelPath))]; len(listed) == 0 {
				continue
			}
		}
		b.EbookFile = ebookCandidate(lib.Root, b.RelPath, b.IsFolder, listed)
	}
}

// dropUnanswered drops from dirs (audioDirs') the folders a look was given up on
// in (unanswered: statAudio's, and the walk's folders that wouldn't list), and
// every disc folder directly in one of them or in a folder above one: the disc
// set they may make (discSets, which turns on all the audio in and beneath the
// folder) can't be known, so neither can a book joined from it (joinRoot: built
// from the discs that answered it would lose the others' parts, with one disc
// left be no set at all, or be a set only for want of the folder not looked
// at, its own audio's among them) nor the discs' split (markSplitDiscs).
func dropUnanswered(dirs map[string][]audioFile, unanswered map[string]error) {
	if len(unanswered) == 0 {
		return
	}
	parents := map[string]bool{}
	for dir := range unanswered {
		for p := dir; p != ""; p = dirOf(p) {
			parents[p] = true
		}
	}
	for dir := range dirs {
		_, gaveUp := unanswered[dir]
		if gaveUp || (parents[dirOf(dir)] && metadata.IsDiscFolder(path.Base(dir))) {
			delete(dirs, dir)
		}
	}
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

// maxIndexedPath is the longest library-relative path a scan indexes:
// catalog.MaxPathBytes, the longest any route takes (server#359). Only a
// Windows library can hold a longer one (its paths run to 32,767 characters);
// indexed, such a book would be listed but every route would refuse its path.
// A variable so a (sequential) test can lower it: no disk the tests run on
// holds a longer path.
var maxIndexedPath = catalog.MaxPathBytes

// overLong reports whether rel is past maxIndexedPath.
func overLong(rel string) bool { return len(rel) > maxIndexedPath }

// maxLoggedPath is how much of an overLong path the scan log and the server
// log keep.
const maxLoggedPath = 200

// loggedPath is an overLong path as it is logged: its first maxLoggedPath
// bytes (cut at a character) and its length, enough to find the folder. Kept
// whole, each would put tens of kilobytes in the scan's stored log and the
// console's reading of it (up to maxProblemEvents of them a scan).
func loggedPath(rel string) string {
	if len(rel) <= maxLoggedPath {
		return rel
	}
	cut := maxLoggedPath
	for cut > 0 && !utf8.RuneStart(rel[cut]) {
		cut--
	}
	return fmt.Sprintf("%s... (%d bytes)", rel[:cut], len(rel))
}

// errPathTooLong is the scan log's reason for a folder or file skipped as
// overLong.
var errPathTooLong = fmt.Errorf("the path is longer than the server allows (%d bytes), so it isn't indexed", catalog.MaxPathBytes)

// audioDirs walks walkFrom (the folder at library-relative fromRel, "" for the
// root) and returns the folders under it that directly hold audio, library-relative,
// each with its audio files as a book has them (folderAudio): the files audioEntries
// would list, read in the same walk rather than again. Hidden and ignored entries are skipped as
// discovery does; see discoverAuto for hadErrors and rl. walkFrom itself is never
// skipped, even when its own name begins with a dot. side is the metadata files
// found in the same walk (sidecarFiles). limit bounds the walk (nil for none):
// reaching it stops the walk with errWalkCapped, dirs holding what it found.
// width is how many folders it lists at once (walkDir), timeout how long each
// look may take (metadata.Bounded: walkDir's listings, a .audiosilo folder's,
// a metadata.json's lstat); its audio files are not
// looked at (statAudio: the callers building books from them do that). A
// folder or audio file whose path is overLong is skipped and logged as
// unreadable (rl), the folder with everything under it; discovery isn't
// partial for it (no book there can ever be indexed, so the prune is not at
// stake).
func audioDirs(ctx context.Context, lib catalog.Library, walkFrom, fromRel string, ignore *Ignore, log *slog.Logger, rl *runLog, limit *walkCap, width int, timeout time.Duration) (dirs map[string][]audioFile, side sidecarFiles, hadErrors bool, err error) {
	dirs = map[string][]audioFile{}
	side = sidecarFiles{abs: map[string]catalog.ABSReading{}, attachments: map[string]attachmentFiles{}, unlisted: map[string]bool{},
		epubs: map[string][]fs.DirEntry{}, unanswered: map[string]error{}}
	// Hidden folders (.Trash-1000, Syncthing .stversions, .git, and .audiosilo,
	// listed on its own below) and ignored ones are never gone into, so their
	// audio isn't indexed into books unreachable via the fs browse view (which
	// also hides them).
	skipped := func(d fs.DirEntry, rel string) bool {
		return isHidden(d.Name()) || (!ignore.Empty() && ignore.Match(rel, true))
	}
	// A folder past the path cap is logged once and never gone into (all
	// below it is past the cap too); an audio file past it is logged and left
	// out of its folder's book. Logged by the start of its path (loggedPath).
	noteOverLong := func(rel string) {
		short := loggedPath(rel)
		log.Warn("skipping a path longer than the server allows", "library", lib.Name, "path", short, "bytes", len(rel))
		rl.unreadable(short, errPathTooLong)
	}
	// The folders the walk goes into are listed ahead (walkDir), except by a
	// capped walk (a setup preview, a folder listing's disc-set check), which may
	// stop at any folder: it lists each as it reaches it, after the cap allows it.
	if limit != nil {
		width = 1
	}
	descend := func(p string, d fs.DirEntry) bool {
		rel := path.Join(fromRel, relPathOf(walkFrom, p))
		return !skipped(d, rel) && !overLong(rel)
	}
	err = walkDir(ctx, walkFrom, width, timeout, descend, func(p string, d fs.DirEntry, walkErr error) error {
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
			rl.unreadable(rel, walkErr)
			// A folder that wouldn't list as a share fails (metadata.ShareFault:
			// EIO, a permission) is as unknown as one that didn't answer: a disc
			// set it may be a disc of can't be built from the rest
			// (dropUnanswered). One gone since its parent listed it holds nothing.
			if walkErr = metadata.AsGaveUp(walkErr); metadata.GaveUp(walkErr) {
				side.unanswered[rel] = walkErr
			}
			return nil
		}
		if d.IsDir() {
			// A book's .audiosilo folder is listed once, never walked into: it holds
			// the Bridge's attachments, never audio.
			if rel != fromRel && isAttachmentsDir(d, rel, ignore) {
				files, lerr := listAttachments(ctx, p, timeout)
				switch {
				case lerr != nil:
					// Its books keep what is stored (Unread). No audio lives here, so
					// discovery isn't partial: the prune is not at stake.
					side.unlisted[dirOf(rel)] = true
					log.Warn("could not list a .audiosilo folder", "library", lib.Name, "path", absOf(lib, rel), "err", lerr)
					rl.unreadable(rel, lerr)
				case len(files) > 0:
					side.attachments[dirOf(rel)] = files
				}
				return fs.SkipDir
			}
			if rel != fromRel {
				if skipped(d, rel) {
					return fs.SkipDir
				}
				if overLong(rel) {
					noteOverLong(rel)
					return fs.SkipDir
				}
			}
			if !limit.another() {
				return errWalkCapped
			}
			return nil
		}
		if isABSEntry(d, rel, ignore) {
			if sig, serr := absSigAt(ctx, lib, dirOf(rel), ignore, timeout); serr != nil {
				// Not to be looked at right now (a flaky mount): its book keeps the
				// stored reading (ABSReading.Unread) rather than losing its ABS
				// values until the next scan.
				side.abs[dirOf(rel)] = catalog.ABSReading{Unread: true}
				log.Warn("could not look at an Audiobookshelf metadata.json", "library", lib.Name, "path", absOf(lib, rel), "err", serr)
				if metadata.GaveUp(serr) {
					rl.unreadable(rel, serr)
				}
			} else {
				side.abs[dirOf(rel)] = catalog.ABSReading{Sig: sig}
			}
			return nil
		}
		if isEpubEntry(d) {
			side.epubs[dirOf(rel)] = append(side.epubs[dirOf(rel)], d)
			return nil
		}
		if !isAudioEntry(d, rel, ignore) {
			return nil
		}
		if overLong(rel) {
			noteOverLong(rel)
			return nil
		}
		dir := dirOf(rel)
		dirs[dir] = append(dirs[dir], audioFile{abs: absOf(lib, rel), de: d})
		return nil
	})
	for dir, audio := range dirs {
		dirs[dir] = folderAudio(audio)
	}
	return dirs, side, hadErrors, err
}

// statAudio reads every audio file's lstat (fs.DirEntry.Info: its size and
// mtime, all a scan needs to find a book unchanged) width at a time, ahead of
// the books being built from them (statEntry). One at a time, a no-change scan
// of a library on a network share or a FUSE pool (Unraid's /mnt/user) spends
// most of its time waiting on these, one round trip each, the walk's own folder
// reads being far fewer. gone is how many of the files were no longer there.
// Once ctx has ended it is ctx's error, the files not reached by then left
// unread: their caller stops rather than building books from them, which would
// look at each of those files one at a time, on a share that may have stopped
// answering, long after its waiter has gone (a setup preview's, at its time,
// with the preview's slot for the mount held meanwhile). Each lstat is a
// metadata.Bounded call (ctx, at most timeout): unanswered is the folders with
// one given up on, each with that error (a share that stopped answering), or
// with one whose lstat failed as a share does (metadata.ShareFault), its error made a
// look given up on: the file listed a moment ago says nothing about whether it
// is there, so its book is kept as stored rather than built without it.
func statAudio(ctx context.Context, dirs map[string][]audioFile, width int, timeout time.Duration) (gone int, unanswered map[string]error, err error) {
	type dirFile struct {
		dir string
		f   *audioFile
	}
	var files []dirFile
	for dir, audio := range dirs {
		for i := range audio {
			files = append(files, dirFile{dir, &audio[i]})
		}
	}
	var missing atomic.Int64
	var mu sync.Mutex
	unanswered = map[string]error{}
	pool.Each(ctx, min(width, len(files)), files, func() bool { return true }, func(f *dirFile) {
		// The pool may still hand a file out as ctx ends (its select picks either):
		// none is looked at once it has.
		if ctx.Err() != nil {
			return
		}
		mu.Lock()
		_, skip := unanswered[f.dir]
		mu.Unlock()
		if skip {
			// Its folder is dropped anyway; on a share that stopped answering
			// each look would wait out its time.
			return
		}
		info, err := lstatAudio(ctx, f.f.abs, timeout) // the entry's Info, bounded
		err = metadata.AsGaveUp(err)
		switch {
		case metadata.GaveUp(err):
			mu.Lock()
			if _, ok := unanswered[f.dir]; !ok {
				unanswered[f.dir] = err
			}
			mu.Unlock()
		case errors.Is(err, fs.ErrNotExist):
			missing.Add(1)
		}
		f.f.de = newStatEntry(f.f.de, info, err)
	})
	return int(missing.Load()), unanswered, ctx.Err()
}

// lstatAudio is statAudio's look at each audio file, readDirLook the walk's
// and a book's own folder listings (walker.list, audioEntries, bookOnDisk) and
// statLook a book's own stat (resolveBook, bookOnDisk): a test injects a
// share's errors through them.
var (
	lstatAudio  = metadata.LstatBounded
	readDirLook = metadata.ReadDirBounded
	statLook    = metadata.StatBounded
)

// transientFault reports whether err is a share's fault that trying again may
// get past: EIO, ESTALE, ENOTCONN, ETIMEDOUT or EHOSTDOWN. Not a permission,
// which stays until an admin changes it, nor any other metadata.ShareFault (a link
// loop, a name the share refuses), which a retry would only meet again.
func transientFault(err error) bool {
	for _, errno := range []syscall.Errno{syscall.EIO, syscall.ESTALE, syscall.ENOTCONN, syscall.ETIMEDOUT, syscall.EHOSTDOWN} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// statEntry is a directory entry whose Info statAudio has read, keeping only
// what a book is built from (folderBook, fileBook: the size and mtime), not the
// whole lstat, for every audio file of a library at once, and the added time
// it tells (foundAdded). It is its own fs.FileInfo: Mode is the entry's type
// bits, Sys nil.
type statEntry struct {
	fs.DirEntry
	size    int64
	modTime time.Time
	found   int64 // foundAdded's time: the birth time Darwin's lstat carries, else the mtime
	err     error
}

func newStatEntry(de fs.DirEntry, info fs.FileInfo, err error) *statEntry {
	if err != nil {
		return &statEntry{DirEntry: de, err: err}
	}
	return &statEntry{DirEntry: de, size: info.Size(), modTime: info.ModTime(), found: lstatAdded(info)}
}

func (e *statEntry) Info() (fs.FileInfo, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e, nil
}

func (e *statEntry) Size() int64        { return e.size }
func (e *statEntry) Mode() fs.FileMode  { return e.Type() }
func (e *statEntry) ModTime() time.Time { return e.modTime }
func (e *statEntry) Sys() any           { return nil }

// sidecarFiles are the metadata files the discovery walk found beside the audio,
// each by the library-relative folder holding it: abs is the reading of each
// folder's Audiobookshelf metadata.json (a regular file, not ignored): its
// signature (metadata.ABSSignature), or Unread when it couldn't be looked at
// right now. attachments each .audiosilo folder's known files
// (listAttachments), and unlisted the folders whose .audiosilo couldn't be
// listed right now. epubs are each folder's .epub entries (isEpubEntry: links
// and ignored files included, as BookEbook's rule reads a folder), which
// ebooksOfWalk turns into each book's ebook candidate. unanswered is the
// folders whose look (the walk's lstat or listing) was given up on
// (metadata.GaveUp), each with that error: what they hold is unknown, so a
// disc set one of them may be a disc of can't be known either (dropUnanswered).
type sidecarFiles struct {
	abs         map[string]catalog.ABSReading
	attachments map[string]attachmentFiles
	unlisted    map[string]bool
	epubs       map[string][]fs.DirEntry
	unanswered  map[string]error
}

// gaveUp is one of the walk's looks given up on (unanswered), nil for none.
func (s sidecarFiles) gaveUp() error {
	for _, err := range s.unanswered {
		return err
	}
	return nil
}

// isABSEntry reports whether a directory entry (at library-relative rel) is an
// Audiobookshelf metadata.json the scan reads: a regular file of that name, not
// ignored (so an ignore rule naming it turns the file off for a library).
func isABSEntry(de fs.DirEntry, rel string, ignore *Ignore) bool {
	return de.Name() == metadata.ABSFileName && de.Type().IsRegular() &&
		(ignore.Empty() || !ignore.Match(rel, false))
}

// absSigAt is the signature of the Audiobookshelf metadata.json the scan reads
// (isABSEntry) in the folder at library-relative rel, "" for none: what the walk
// records for a folder whose entry it couldn't look at (audioDirs), for a book
// indexed without one (IndexPath). err is a file that couldn't be looked at
// right now (not one that isn't there), its lstat given up on after timeout
// included (metadata.LstatBounded).
func absSigAt(ctx context.Context, lib catalog.Library, rel string, ignore *Ignore, timeout time.Duration) (string, error) {
	info, err := metadata.LstatBounded(ctx, filepath.Join(absOf(lib, rel), metadata.ABSFileName), timeout)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !isABSEntry(fs.FileInfoToDirEntry(info), path.Join(rel, metadata.ABSFileName), ignore) {
		return "", nil
	}
	return metadata.ABSSignature(info), nil
}

// readABS reads the Audiobookshelf metadata.json in the folder at
// library-relative rel, whose signature is sig ("" for none: nothing to read).
// The file is nil when there is none or it can't be used (not JSON, too large:
// metadata.ErrABSFileInvalid, logged), and that reading is recorded with its
// signature, so it isn't read again until it changes. read is false when it
// couldn't be read right now (gone since the walk, a flaky mount, a share that
// stopped answering: the read is a metadata.Bounded call, given up on after the
// probe's time and noted in gaveUp; logged): the caller records nothing for it,
// so the next scan reads it again rather than taking the book's ASIN and the
// rest away until ABS next rewrites the file.
func (s *Scanner) readABS(ctx context.Context, lib catalog.Library, rel, sig string, gaveUp *givenUp) (f *metadata.ABSFile, read bool) {
	if sig == "" {
		return nil, true
	}
	file := path.Join(rel, metadata.ABSFileName)
	abs := absOf(lib, file)
	f, err := metadata.Bounded(ctx, abs, s.lookTimeout(ctx), func() (*metadata.ABSFile, error) { return metadata.ReadABSFile(abs) }, nil)
	if err != nil {
		s.log.Warn("unreadable Audiobookshelf metadata.json", "library", lib.Name, "path", file, "err", err)
		gaveUp.note(file, err)
		return nil, errors.Is(err, metadata.ErrABSFileInvalid)
	}
	return f, true
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
// directory entry (a statEntry once statAudio has read its lstat).
type audioFile struct {
	abs string
	de  fs.DirEntry
}

// audioEntries returns the non-hidden, non-ignored audio files directly inside
// absDir whose paths aren't overLong, as a book has them (folderAudio): none
// when it won't list. The
// listing is a metadata.Bounded call (ctx, at most timeout): err is one given
// up on (metadata.GaveUp) or ctx's.
func audioEntries(ctx context.Context, root, absDir string, ignore *Ignore, timeout time.Duration) ([]audioFile, error) {
	// A listing failing as a share does (metadata.ShareFault) is a look given
	// up on, as one that did not answer: never "no audio", which reads as the
	// book gone (bookOnDisk). A folder gone (ENOENT, ENOTDIR) holds none.
	entries, err := readDirLook(ctx, absDir, timeout)
	err = metadata.AsGaveUp(err)
	if metadata.Unanswered(ctx, err) {
		return nil, cmp.Or(err, ctx.Err()) // never "no audio" for a listing ctx ended on
	}
	if err != nil {
		return nil, nil
	}
	var audio []audioFile
	for _, de := range entries {
		abs := filepath.Join(absDir, de.Name())
		if rel := relPathOf(root, abs); isAudioEntry(de, rel, ignore) && !overLong(rel) {
			audio = append(audio, audioFile{abs: abs, de: de})
		}
	}
	return folderAudio(audio), nil
}

// folderAudio is one folder's audio files as a book has them: in play order
// (sortFileOrder), less any .mka beside a file of the same name in another audio
// format. Like an .aax beside its converted .m4b (metadata.AudioExtensions), that
// .mka is the other's source or copy, ignored before .mka was audio (#161), and
// reading both would lump them into one book with its chapters twice.
func folderAudio(audio []audioFile) []audioFile {
	names := make([]string, len(audio))
	for i, f := range audio {
		names[i] = f.de.Name()
	}
	shadowed := shadowedMKA(names)
	audio = slices.DeleteFunc(audio, func(f audioFile) bool { return shadowed[f.de.Name()] })
	sortFileOrder(audio)
	return audio
}

// shadowedMKA returns the names among one folder's audio files that are an .mka
// beside a file of the same name in another audio format (folderAudio's rule):
// the files a book of the folder leaves out, and so the folder listing too
// (listDir). nil when the folder holds no .mka.
func shadowedMKA(names []string) map[string]bool {
	stem := func(name string) string { return strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name))) }
	isMKA := func(name string) bool { return strings.EqualFold(filepath.Ext(name), ".mka") }
	if !slices.ContainsFunc(names, isMKA) {
		return nil
	}
	others := make(map[string]bool, len(names))
	for _, n := range names {
		if !isMKA(n) {
			others[stem(n)] = true
		}
	}
	out := map[string]bool{}
	for _, n := range names {
		if isMKA(n) && others[stem(n)] {
			out[n] = true
		}
	}
	return out
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

// addedAt is when a file first appeared on disk: its birth (creation) time
// where the OS/filesystem records one (birthKnown: a share that keeps none can
// answer 0), otherwise its mtime. Formatted as RFC3339
// UTC to match the other timestamp columns and sort lexicographically.
func addedAt(absPath string, info os.FileInfo) string {
	found := lstatAdded(info)
	if a, ok := addedFrom(absPath, found); ok {
		return a
	}
	return formatAdded(found)
}

// formatAdded is addedAt's format of a time in Unix seconds (the format has
// no fraction, so the seconds are all of it).
func formatAdded(sec int64) string { return time.Unix(sec, 0).UTC().Format(time.RFC3339) }

// foundAdded is the time addedAt would read from info, a lstat the walk did
// (the statEntry statAudio kept, or a directory entry's own), in Unix seconds:
// the birth time where the stat carries one (lstatAdded), else the mtime.
func foundAdded(info fs.FileInfo) int64 {
	if e, ok := info.(*statEntry); ok {
		return e.found
	}
	return lstatAdded(info)
}

// stampAddedAt sets b.AddedAt to when the book first appeared on disk: the
// earliest addedAt of its audio files. It is read only for a book about to be
// written (enrich), never by discovery: added_at is recorded when a book is
// first indexed and never updated, so a scan that finds a book unchanged has no
// use for it, and on Linux the birth time is a statx per file on top of the
// walk's lstat, which on a network share or a FUSE pool (Unraid's /mnt/user)
// is most of a no-change scan's cost. The walk's lstat (b.FoundAdded) is not
// taken again (addedFrom): on Darwin it carries the birth time, so a file
// costs nothing more; on Linux one statx reads the birth time, the walk's
// mtime standing in where the filesystem has none. A book built without it
// (a test's) lstats each file. A book none of whose files can be looked at now
// (gone since discovery) is stamped now, as first seen; on Darwin, which looks
// at none of them again, only a book discovery couldn't stat. Each file's
// look (Linux's statx, the lstat) is one metadata.Bounded call under ctx and
// timeout; once one is given up on (a share that stopped answering) the rest
// are not looked at, each would wait out its time too: that file and the rest
// take the walk's time (b.FoundAdded: the mtime on Linux, as on a filesystem
// with no birth time), and only a book built without it is stamped now.
func stampAddedAt(ctx context.Context, lib catalog.Library, b *catalog.Book, timeout time.Duration) {
	var paths []string
	if b.IsFolder {
		for _, f := range b.Files {
			paths = append(paths, f.RelPath)
		}
	} else {
		paths = []string{b.RelPath}
	}
	var added string // RFC3339 UTC sorts lexicographically, so min string = earliest
	for i, rel := range paths {
		abs := absOf(lib, rel)
		found, haveFound := int64(0), len(b.FoundAdded) == len(paths)
		if haveFound {
			found = b.FoundAdded[i]
		}
		a, err := metadata.Bounded(ctx, abs, timeout, func() (string, error) {
			if haveFound {
				if a, ok := addedFrom(abs, found); ok {
					return a, nil
				}
			}
			info, err := os.Lstat(abs)
			if err != nil {
				return "", err
			}
			return addedAt(abs, info), nil
		}, nil)
		if metadata.Unanswered(ctx, err) {
			if haveFound {
				// The walk's time of this file and the rest (no system call):
				// added_at is never written again, and now would hold a book
				// years old at the top of Recently added for good.
				for _, found := range b.FoundAdded[i:] {
					added = earlierAdded(added, formatAdded(found))
				}
			}
			break
		}
		if err != nil {
			continue
		}
		added = earlierAdded(added, a)
	}
	if added == "" {
		added = now()
	}
	b.AddedAt = added
}

// earlierAdded is the earlier of two addedAt times, "" being none.
func earlierAdded(a, b string) string {
	if a == "" || (b != "" && b < a) {
		return b
	}
	return a
}

// fileBook builds a single-file book for the audio file at absPath (its AddedAt
// is enrich's: stampAddedAt).
func fileBook(lib catalog.Library, absPath string, info os.FileInfo) *catalog.Book {
	rel, _ := filepath.Rel(lib.Root, absPath)
	return &catalog.Book{
		LibraryID:  lib.ID,
		RelPath:    filepath.ToSlash(rel),
		Format:     ext(filepath.Base(absPath)),
		Size:       info.Size(),
		MTime:      info.ModTime().Unix(),
		FoundAdded: []int64{foundAdded(info)},
	}
}

// folderBook builds a (possibly multi-file) book at absDir from audio, its
// files in play order (audioEntries' name order, or joinedBook's), or returns nil
// if there are none. Its AddedAt is enrich's (stampAddedAt).
func folderBook(lib catalog.Library, absDir string, audio []audioFile) *catalog.Book {
	var files []catalog.BookFile
	var found []int64
	var totalSize, maxMTime int64
	for _, f := range audio {
		info, ierr := f.de.Info()
		if ierr != nil {
			continue
		}
		frel, _ := filepath.Rel(lib.Root, f.abs)
		files = append(files, catalog.BookFile{
			RelPath: filepath.ToSlash(frel), Seq: len(files), Format: ext(f.de.Name()), Size: info.Size(),
		})
		found = append(found, foundAdded(info))
		totalSize += info.Size()
		if m := info.ModTime().Unix(); m > maxMTime {
			maxMTime = m
		}
	}
	if len(files) == 0 {
		return nil
	}
	rel, _ := filepath.Rel(lib.Root, absDir)
	return &catalog.Book{
		LibraryID:  lib.ID,
		RelPath:    filepath.ToSlash(rel),
		IsFolder:   true,
		Format:     files[0].Format,
		Size:       totalSize,
		MTime:      maxMTime,
		Files:      files,
		FoundAdded: found,
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
// Each caller is checked against allow before concurrent callers for the same
// book share one read of it (indexShared).
func (s *Scanner) IndexPathWithin(ctx context.Context, lib catalog.Library, relPath string, allow func(string) bool) (*catalog.Book, error) {
	book, err := s.resolveBook(ctx, lib, relPath, allow)
	if err != nil {
		return nil, err
	}
	return s.indexShared(ctx, lib, book)
}

// ReindexPath is IndexPath for a caller who asks for the book as it is on disk
// now (an admin's Re-read): it waits for a read of the book under way, a scan's
// included, then reads the book itself rather than loading what that read
// stored, which may be older than the change the caller wants picked up.
//
// A read that kept the book as stored because a file did not answer (a look
// given up on, or the audio's read cut short: catalog.Book.ReadCutShort with an
// unreadable problem, recorded) is a look given up on too
// (metadata.ErrNotAnswered): the caller asked for the book as it is now, and
// the share holding it is not answering.
func (s *Scanner) ReindexPath(ctx context.Context, lib catalog.Library, relPath string) (*catalog.Book, error) {
	book, err := s.resolveBook(ctx, lib, relPath, nil)
	if err != nil {
		return nil, err
	}
	return s.readOwn(ctx, lib, book, func(ctx context.Context) (string, error) { return s.movedFrom(ctx, lib, book) })
}

// readOwn reads book itself (leadIndex, the state at from carried first) once
// no read of it is under way (startFlight), then loads what it stored
// (awaitFlight): ReindexPath's and IndexMoved's read, which neither joins. A
// read that kept the book as stored (ReadCutShort with an unreadable problem)
// is a look given up on (metadata.ErrNotAnswered), as its caller asked for
// the book as it is on disk now.
func (s *Scanner) readOwn(ctx context.Context, lib catalog.Library, book *catalog.Book, from func(context.Context) (string, error)) (*catalog.Book, error) {
	f, err := s.startFlight(ctx, bookKey{lib.ID, book.RelPath}, nil)
	if err != nil {
		return nil, err
	}
	go s.leadIndex(ctx, lib, book, f, from)
	b, err := s.awaitFlight(ctx, f, false)
	// book is the read's (leadIndex's), done with once the flight has ended.
	if err == nil && book.ReadCutShort && book.ScanError == problemUnreadable {
		return nil, fmt.Errorf("library: reading %q: %w", book.ScanErrorFile, metadata.ErrNotAnswered)
	}
	return b, err
}

// tagRead is an unchanged book whose primary file a scan reads for its
// backfills (readTags): what to read, what for, and the reading.
type tagRead struct {
	book    *catalog.Book
	old     catalog.Signature
	primary string // its absolute path
	names   bool   // the names are read too (PathRead, Signature.NamesCheck)
	md      *metadata.Metadata
	// last is the reading of the book's last file, read when the names are and
	// the title turns on it (partNamed); nil otherwise, or left unread by a Stop.
	last *metadata.Metadata
}

// tagReadsPerSlot is how many backfill reads a scan gathers per read slot
// before reading them, at once as the slots allow.
const tagReadsPerSlot = 8

// bookKey names a book across libraries: (library_id, rel_path).
type bookKey struct {
	libraryID int64
	relPath   string
}

// indexFlight is one read of book key under way, by an on-demand index
// (indexShared, IndexMoved) or a scan (Scan's reads): done closes once it has
// ended, err nil when the book is stored at its path.
type indexFlight struct {
	key  bookKey
	done chan struct{}
	err  error
	// scan is the scan's read that leads the flight (nil for an on-demand
	// one): its book is written only after the read, in the scan's batches, so
	// the flight can outlast the read (waitFlight), and a stopped scan marks
	// the read it left under way (bookRead.left; Scanner.readsLeft), held in a
	// call that may never return, so no later read waits for it.
	scan *bookRead
	// progress is when the flight's read last moved on (each open, block read
	// or bounded look of it that returned in time: metadata.WithProgress on
	// the read's context), so a waiter gives up on a read that stopped, not on
	// one that is slow (waitFlight).
	progress *metadata.Progress
	// stuck is set once a waiter found the read had not moved on for a whole
	// look's time (the probe's): no later waiter waits on it.
	stuck atomic.Bool
}

// errReadLeft is waitFlight's error when another read of the book may never
// return: one a stopped scan left under way (bookRead.left), or one not
// returned within a look's time. It is a look given up on (metadata.GaveUp): an
// on-demand read's caller answers 503 share_not_answering, and a scan keeps
// the book as stored (keepLeft).
var errReadLeft = fmt.Errorf("library: another read of this book has not returned: %w", metadata.ErrNotAnswered)

// errReadDropped is a flight's outcome when a scan read the book but did not
// write it (the scan stopped, or the write failed): its waiters read it
// themselves.
var errReadDropped = errors.New("library: the scan's read of this book was not written")

// indexTimeout bounds a shared read once it has begun. It no longer stops with
// the request that began it (see leadIndex).
const indexTimeout = 10 * time.Minute

// lookTimeoutKey carries an on-demand read's look time (WithLookTimeout).
type lookTimeoutKey struct{}

// WithLookTimeout gives the on-demand reads under ctx (IndexPath and its kin,
// ReindexPath, IndexMoved, RefreshAttachments) d for each look at a library
// file (metadata.Bounded: resolveBook's, the files beside the audio, the wait
// on another read of the book) in place of the scan's probe time, which is a
// whole request's: a request on a share that stopped answering then hears it
// (metadata.GaveUp, the API's 503 share_not_answering) before its own time
// runs out. The audio's own read keeps the probe's time. It rides on ctx so
// the read leadIndex goes on with past the request keeps it.
func WithLookTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, lookTimeoutKey{}, d)
}

// lookTimeout is the time each look at a library file gets under ctx: its
// WithLookTimeout when shorter, else the probe's.
func (s *Scanner) lookTimeout(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(lookTimeoutKey{}).(time.Duration); ok && d > 0 && d < s.probeTimeout {
		return d
	}
	return s.probeTimeout
}

// indexShared is indexBook of a resolved book, read once for every caller that
// asks for it while a read is under way (an on-demand index's, IndexMoved's or
// a scan's): each waits for that read and loads the book it stored, its own
// copy. A caller whose own request ends stops waiting, the one that began the
// read too (the read goes on: leadIndex); one whose read was given up (a scan
// stopped, a read past indexTimeout) reads the book itself. One waiting on
// another's read that has not returned in time gives up on it (waitFlight).
func (s *Scanner) indexShared(ctx context.Context, lib catalog.Library, book *catalog.Book) (*catalog.Book, error) {
	for {
		f, lead := s.claimFlight(bookKey{lib.ID, book.RelPath}, nil)
		if lead {
			go s.leadIndex(ctx, lib, book, f, func(ctx context.Context) (string, error) { return s.movedFrom(ctx, lib, book) })
		}
		b, err := s.awaitFlight(ctx, f, !lead)
		if !readGivenUp(err) || ctx.Err() != nil {
			return b, err
		}
		if s.stopped() {
			// A stopped server would end each new read at once: the waiter
			// learns why (an API answers 503, not a failure).
			return nil, fmt.Errorf("%w: %w", ErrStopping, err)
		}
	}
}

// ErrStopping is an on-demand read's error when the server's stop
// (SetBaseContext) ended it while its caller still waited.
var ErrStopping = errors.New("library: the server is stopping")

// awaitFlight waits for flight f to end, then loads the book it stored; ctx's
// error when ctx ends first, the flight's when it stored none. A caller that
// joined another's read (joined) waits on it only as waitFlight does.
func (s *Scanner) awaitFlight(ctx context.Context, f *indexFlight, joined bool) (*catalog.Book, error) {
	if joined {
		if err := s.waitFlight(ctx, f); err != nil {
			return nil, err
		}
	} else {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.done:
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return s.cat.GetBookByPath(ctx, f.key.libraryID, f.key.relPath)
}

// readGivenUp reports whether a flight's error says its read was given up
// rather than failed: a waiter then reads the book itself.
func readGivenUp(err error) bool {
	return errors.Is(err, errReadDropped) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// startFlight registers a flight for key once no read of the book is under way,
// waiting for one that is (waitFlight's error when it gives up). The caller
// ends it with endFlight. A scan and IndexMoved take a book this way: neither
// can join a read made without them (a scan writes in its own batches,
// IndexMoved carries its book's state first), but neither reads it beside one
// either. A scan's read passes itself (indexFlight.scan; nil otherwise).
func (s *Scanner) startFlight(ctx context.Context, key bookKey, scan *bookRead) (*indexFlight, error) {
	for {
		f, lead := s.claimFlight(key, scan)
		if lead {
			return f, nil
		}
		if err := s.waitFlight(ctx, f); err != nil {
			return nil, err
		}
	}
}

// waitFlight waits for flight f, another's read of the book, to end: nil once
// it has, ctx's error when ctx ends first. A read that may never return (held
// in a call a share that stopped answering never returns from) is not waited
// on for good, or a scan would stall at the book and a request wait out its
// time: errReadLeft at once for one a stopped scan left under way
// (bookRead.left) or a waiter found stuck (indexFlight.stuck), and for any
// other once a look's time (lookTimeout) has passed with it not returned and
// not moved on (indexFlight.progress: a slow read that answers is waited on);
// one that has not moved on for the probe's time is marked stuck. A scan's
// read that has returned is waited on until the scan writes its book.
func (s *Scanner) waitFlight(ctx context.Context, f *indexFlight) error {
	if f.stuck.Load() || (f.scan != nil && f.scan.left.Load()) {
		return errReadLeft
	}
	look := s.lookTimeout(ctx)
	wait := time.NewTimer(look)
	defer wait.Stop()
	expired := wait.C
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.done:
			return nil
		case <-expired:
			if f.scan != nil && f.scan.finished() {
				expired = nil // returned: waited on until the scan writes it
				continue
			}
			// A read still moving on (a big book on a slow share that answers)
			// is waited on a look's time from its last progress.
			if idle := f.progress.Idle(); idle < look {
				wait.Reset(look - idle)
				continue
			}
			if f.progress.Idle() >= s.probeTimeout {
				f.stuck.Store(true) // stopped for a whole look's time: left
			}
			return errReadLeft
		}
	}
}

// claimFlight is the flight of key under way, or a new one registered for the
// caller to lead (lead true) when there is none, led by scan (indexFlight.scan).
func (s *Scanner) claimFlight(key bookKey, scan *bookRead) (f *indexFlight, lead bool) {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	if f, busy := s.flights[key]; busy {
		return f, false
	}
	f = &indexFlight{key: key, done: make(chan struct{}), scan: scan, progress: metadata.NewProgress()}
	s.flights[key] = f
	return f, true
}

// endFlight ends flight f with its outcome, for its waiters.
func (s *Scanner) endFlight(f *indexFlight, err error) {
	f.err = err
	s.flightMu.Lock()
	delete(s.flights, f.key)
	s.flightMu.Unlock()
	close(f.done)
}

// errIndexAborted is a shared read's outcome when it panicked: its waiters fail
// rather than load a book it never stored.
var errIndexAborted = errors.New("library: the shared index of this book did not finish")

// SetBaseContext binds the reads leadIndex runs past their requests to the
// server's lifetime (the API's base context): they stop when it ends, rather
// than read on and write to a store that is closing.
func (s *Scanner) SetBaseContext(ctx context.Context) { s.base.Store(&ctx) }

// stopped reports whether the server's base context (SetBaseContext) has ended.
func (s *Scanner) stopped() bool {
	b := s.base.Load()
	return b != nil && (*b).Err() != nil
}

// leadIndex is the read of flight f's book, on a goroutine of its own: it
// indexes the book after carrying the state of the book at from(ctx) to it
// (indexBookFrom), then ends the flight with the outcome; from's error ends it
// with nothing carried or indexed (movedFrom's move that can't be known now),
// every waiter's answer. It runs past the end
// of the request that began it (up to indexTimeout, and no longer than the
// server: SetBaseContext): throwing the read away at its last write would leave
// the next request to read the book again from scratch (server#211), or a
// moved book's state on a path with no row.
func (s *Scanner) leadIndex(ctx context.Context, lib catalog.Library, book *catalog.Book, f *indexFlight, from func(context.Context) (string, error)) {
	err := errIndexAborted // until from and indexBookFrom return
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("index book panicked", "library", lib.Name, "path", book.RelPath, "panic", p, "stack", string(debug.Stack()))
		}
		s.endFlight(f, err)
	}()
	ctx, cancel := context.WithTimeout(metadata.WithProgress(context.WithoutCancel(ctx), f.progress), indexTimeout)
	defer cancel()
	if base := s.base.Load(); base != nil {
		defer context.AfterFunc(*base, cancel)()
	}
	// Logged here: the request that began the read may be gone. A read the
	// server's stop ended is no failure.
	var old string
	if old, err = from(ctx); err == nil {
		err = s.indexBookFrom(ctx, lib, book, old)
	} else if ctx.Err() != nil {
		// from's look ended with the read (the server's stop, indexTimeout), not
		// a share's answer: a read given up (readGivenUp), as indexBook's is, so
		// a waiter hears ErrStopping or reads again rather than a 503
		// share_not_answering.
		err = fmt.Errorf("%w: %w", ctx.Err(), err)
	}
	if err != nil && !s.stopped() {
		s.log.Warn("index book failed", "library", lib.Name, "path", book.RelPath, "err", err)
	}
}

// movedFrom is where a book IndexPath indexes for the first time was indexed
// before, when it is a book moved or renamed on disk since the last scan (a
// renamed folder opened from Folders, server#209): an indexed book with its
// fingerprint that is no longer on disk. "" when there is none, or when the book
// is indexed already. carryFrom checks it as it checks IndexMoved's from.
//
// When that can't be known now (a book not indexed whose fingerprints can't be
// read for a reason a retry may get past, fingerprintUnknown, or a candidate's
// look is bookOnDisk's diskUnknown) its error is a look given up on
// (metadata.ErrNotAnswered) and nothing is indexed: a book indexed without the
// state it moved from would leave that state on the old row, which the next
// scan prunes. A later call, once the share answers, carries it.
func (s *Scanner) movedFrom(ctx context.Context, lib catalog.Library, book *catalog.Book) (string, error) {
	unknown := func(why string) (string, error) {
		return "", fmt.Errorf("library: indexing %q: %s: %w", book.RelPath, why, metadata.ErrNotAnswered)
	}
	failed := func(err error) (string, error) {
		s.log.Warn("looking for a moved book failed", "library", lib.Name, "path", book.RelPath, "err", err)
		return "", nil
	}
	// An indexed book has no move to find: its fingerprint is left to enrich,
	// which reads it only once the tags have been read, so a Re-read on a share
	// that stopped answering waits one probe's time, not two.
	_, err := s.cat.GetBookByPath(ctx, lib.ID, book.RelPath)
	if err == nil {
		return "", nil
	}
	if !errors.Is(err, catalog.ErrNotFound) {
		return failed(err)
	}
	var ferr error
	if book.ContentHash == "" {
		book.ContentHash, ferr = s.fingerprintErr(ctx, lib, book) // reused by enrich
	}
	if fingerprintUnknown(ferr) {
		return unknown("its fingerprint couldn't be read")
	}
	was, werr := s.byteFirstFingerprintErr(ctx, lib, book)
	candidates, err := s.cat.MoveCandidates(ctx, lib.ID, book.RelPath, []string{book.ContentHash, was})
	if err != nil {
		return failed(err)
	}
	for _, p := range candidates {
		switch s.bookOnDisk(ctx, lib, p) {
		case diskGone:
			return p, nil
		case diskUnknown:
			// The others' looks would wait on the same share.
			return unknown(fmt.Sprintf("whether the book indexed at %q is still on disk can't be known", p))
		}
	}
	if fingerprintUnknown(werr) {
		// A book indexed before #160 may be found only by that file's.
		return unknown("its first file by bytes couldn't be read")
	}
	return "", nil
}

// indexBook reads a resolved book (resolveBook) and indexes it. ctx is
// leadIndex's, which no request ends: the read stops only at indexTimeout or
// with the server, and then nothing of it is written (UpsertBook fails on the
// same ctx), rather than reading on to a write that can no longer happen.
func (s *Scanner) indexBook(ctx context.Context, lib catalog.Library, book *catalog.Book) error {
	// No walk found it, so look for its ebook here: only now it is indexed, as a
	// book resolved for another reason (bookOnDisk, a shared read's waiters)
	// never needs it. Its looks are one metadata.Bounded call: given up on, what
	// is stored for it stays (Unread), as for a folder that won't list.
	ebook, err := metadata.Bounded(ctx, absOf(lib, sidecarDir(book.RelPath, book.IsFolder)), s.lookTimeout(ctx),
		func() (catalog.EbookReading, error) { return ebookOnDisk(lib.Root, book.RelPath, book.IsFolder), nil }, nil)
	if err != nil {
		ebook = catalog.EbookReading{Unread: true}
	}
	book.EbookFile = ebook
	s.enrich(ctx, lib, book, s.readSlots(1))
	// UpsertBook layers any path-keyed enrichment and metadata overrides onto the
	// scanned values in the same transaction.
	_, err = s.cat.UpsertBook(ctx, book)
	return err
}

// resolveBook is the book relPath resolves to on disk, worked out exactly as a
// full scan would (IndexPathWithin's rules), with the signatures of its metadata
// files, but nothing read from its audio and nothing indexed.
func (s *Scanner) resolveBook(ctx context.Context, lib catalog.Library, relPath string, allow func(string) bool) (*catalog.Book, error) {
	if lib.Kind != catalog.KindBooks { // a podcast library, or one of a kind this server doesn't know
		return nil, ErrNotIndexable
	}
	// SafeJoin is the security gate (rejects traversal and symlink escapes). It
	// returns a symlink-RESOLVED path, but we derive the working path from an
	// unresolved join so the rel_path computed by fileBook/folderBook matches the
	// full scan, which names folders under lib.Root unresolved. Using SafeJoin's
	// resolved path here would yield a "../"-laden rel_path whenever any component
	// of lib.Root is a symlink (e.g. macOS /tmp -> /private/tmp, or a NAS /data ->
	// /mnt/...).
	//
	// Every look here at the library's files is a metadata.Bounded call given
	// the on-demand read's time (lookTimeout): on a share that stopped
	// answering the first given up on is the answer (metadata.GaveUp: the
	// API's 503 share_not_answering), the rest not tried.
	look := s.lookTimeout(ctx)
	if _, err := ContentJoinBounded(ctx, lib.Root, relPath, look); err != nil {
		return nil, err
	}
	abs := absOf(lib, relPath)
	// A stat failing as a share does (metadata.ShareFault: EIO, a permission)
	// says nothing about whether the book is there: a look given up on, never
	// "no book" (bookOnDisk would read that as gone).
	info, err := statLook(ctx, abs, look)
	err = metadata.AsGaveUp(err)
	if metadata.Unanswered(ctx, err) {
		return nil, cmp.Or(err, ctx.Err()) // ctx may have ended as the stat answered
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotIndexable, err)
	}
	// A case or normalization variant of an on-disk path opens on a
	// case-insensitive filesystem (macOS's and Windows' defaults), but the scan
	// names a book by the folder listings' spelling: indexing the variant would
	// add a duplicate everyone sees, which the next scan removes (CONNECT-04).
	rel := relPathOf(lib.Root, abs)
	if rel != "" {
		present, err := dirPresent(ctx, lib, rel, map[string]map[string]bool{}, look)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, fmt.Errorf("%w: %q is not spelled as on disk", ErrNotIndexable, relPath)
		}
	}

	// What the ignore rules keep out of a scan isn't in the library on demand either.
	ignore := ParseIgnore(lib.IgnorePatterns)
	if ignore.Covers(rel, info.IsDir()) {
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
	dirRel := rel
	if !info.IsDir() {
		dirRel = dirOf(dirRel)
	}
	own, err := audioEntries(ctx, lib.Root, absOf(lib, dirRel), ignore, look)
	if err != nil {
		return nil, err
	}
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
		} else if len(own) == 0 || !metadata.IsDiscFolder(path.Base(dirRel)) {
			return false
		}
		dirs, side, _, err := audioDirs(ctx, lib, absOf(lib, p), p, ignore, s.log, nil, nil, s.walkWidth(), look)
		if err == nil {
			// A folder of the set that wouldn't list: the set, and the book
			// joined from it, can't be known now.
			err = side.gaveUp()
		}
		if err != nil {
			walkErr = err
			return false
		}
		discs = dirs
		return discSets(maps.Keys(dirs))[p]
	}
	var candidates []*catalog.Book
	// The files a book is built from, each looked at (statAudio): a part whose
	// look was given up on is the answer, the book can't be known now.
	statParts := func(dirs map[string][]audioFile) error {
		_, unanswered, err := statAudio(ctx, dirs, s.walkWidth(), look)
		if err != nil {
			return err
		}
		for dir := range dirs {
			if uerr := unanswered[dir]; uerr != nil {
				return uerr
			}
		}
		return nil
	}
	if root, ok := joinRoot(overrides, dirRel, isDiscSet); ok {
		// The joined book is built from all of them.
		if err := statParts(discs); err != nil {
			return nil, err
		}
		if b := joinedBook(lib, root, discs); b != nil {
			candidates = []*catalog.Book{b}
		}
	} else if walkErr != nil {
		return nil, walkErr
	} else {
		if err := statParts(map[string][]audioFile{dirRel: own}); err != nil {
			return nil, err
		}
		candidates = booksInDir(lib, dirRel, own, overrides)
	}
	book := pickBook(candidates, rel)
	if book == nil {
		return nil, fmt.Errorf("%w: no book at %q", ErrNotIndexable, relPath)
	}
	if allow != nil && !allow(book.RelPath) {
		return nil, ErrNotAllowed
	}
	if book.SplitParent, err = s.splitParentOf(ctx, lib, book, ignore); err != nil {
		return nil, err
	}

	// No walk found it, so look for its metadata.json and attachments here. One
	// that can't be looked at keeps what is stored, but one given up on is the
	// answer, as every look above.
	if book.IsFolder {
		if sig, aerr := absSigAt(ctx, lib, book.RelPath, ignore, look); metadata.Unanswered(ctx, aerr) {
			return nil, cmp.Or(aerr, ctx.Err())
		} else if aerr != nil {
			s.log.Warn("could not look at the book's Audiobookshelf metadata.json", "library", lib.Name, "path", book.RelPath, "err", aerr)
			book.ABS.Unread = true // what is stored stays
		} else {
			book.ABS.Sig = sig
		}
	}
	if sig, aerr := attachmentsSigAt(ctx, lib, book, ignore, look); metadata.Unanswered(ctx, aerr) {
		return nil, cmp.Or(aerr, ctx.Err())
	} else if aerr != nil {
		s.log.Warn("could not list the book's .audiosilo folder", "library", lib.Name, "path", book.RelPath, "err", aerr)
		book.Attachments = catalog.AttachmentsReading{Unread: true} // what is stored stays
	} else {
		book.Attachments.Sig = sig
	}
	return book, nil
}

// IndexMoved is IndexPath for a book the caller has just moved from `from` (its
// path before; "" for none): the Bridge placing or renaming a book. It carries
// the book's durable state from `from` exactly as a full scan's move detection
// does (carryMove: listening state, edits, covers, enrichment, Bridge jobs; then
// the favourites of the folders it left) and drops the old book's row, but only
// when no book is at `from` on disk any more and the fingerprints match:
// otherwise `from` is ignored (logged), never an error, and the path is indexed
// as IndexPath would. The state is carried before the new book is indexed, so
// its row resolves with the carried edits as a scan's does.
//
// When that can't be known now (a look at `from` or the new book's
// fingerprint given up on: a share that stopped answering), nothing is carried
// and nothing indexed, and the error is a look given up on
// (metadata.ErrNotAnswered, the API's 503 share_not_answering): the caller
// tries again, and a scan meanwhile still sees the new path as new, so its move
// detection can carry the state. A read that kept the new book as stored
// (readOwn) is a look given up on too, its state carried already.
func (s *Scanner) IndexMoved(ctx context.Context, lib catalog.Library, relPath, from string) (*catalog.Book, error) {
	book, err := s.resolveBook(ctx, lib, relPath, nil)
	if err != nil {
		return nil, err
	}
	// After a read of the book under way.
	return s.readOwn(ctx, lib, book, func(context.Context) (string, error) { return from, nil })
}

// indexBookFrom indexes book (indexBook) after carrying the durable state of the
// book indexed at from to it, when carryFrom finds it the same book moved (from
// "": none), then drops the old book's row. Its caller (leadIndex) runs it past
// the end of the request that asked for it.
func (s *Scanner) indexBookFrom(ctx context.Context, lib catalog.Library, book *catalog.Book, from string) error {
	moved, err := s.carryFrom(ctx, lib, from, book)
	if err != nil {
		return err
	}
	if err := s.indexBook(ctx, lib, book); err != nil {
		return err
	}
	if moved {
		// The book's state is already at its new path; a row left here is pruned by
		// the next full scan, so this failing fails nothing.
		if err := s.cat.DeleteBookAt(ctx, lib.ID, from); err != nil {
			s.log.Warn("drop the moved book's old row failed", "library", lib.Name, "path", from, "err", err)
		}
	}
	return nil
}

// carryFrom carries the durable state of the indexed book at from to nb, the
// book now at its new path (not indexed yet), when it is the same book moved: no
// book at from on disk, an equal fingerprint, and not one folder seen two ways
// (reclassified). It reports whether it carried (never for a from of "" or
// nb's own path); why it didn't is logged. When
// that can't be known now (bookOnDisk's diskUnknown, or nb's fingerprint
// given up on while the old book has one) it carries nothing and its error is
// a look given up on (metadata.ErrNotAnswered).
func (s *Scanner) carryFrom(ctx context.Context, lib catalog.Library, from string, nb *catalog.Book) (bool, error) {
	if from == "" || from == nb.RelPath {
		return false, nil
	}
	ignored := func(why string, args ...any) (bool, error) {
		s.log.Info("move ignored: "+why, append([]any{"library", lib.Name, "from", from, "to", nb.RelPath}, args...)...)
		return false, nil
	}
	unknown := func(why string) (bool, error) {
		return false, fmt.Errorf("library: moving %q to %q: %s: %w", from, nb.RelPath, why, metadata.ErrNotAnswered)
	}
	old, err := s.cat.GetBookByPath(ctx, lib.ID, from)
	if errors.Is(err, catalog.ErrNotFound) {
		return ignored("no book was indexed there")
	}
	if err != nil {
		return ignored("the old book couldn't be read", "err", err)
	}
	switch s.bookOnDisk(ctx, lib, from) {
	case diskPresent:
		return ignored("the old book is still on disk")
	case diskUnknown:
		return unknown("whether the old book is still on disk can't be known")
	}
	if old.ContentHash == "" {
		return ignored("the fingerprints differ")
	}
	if nb.ContentHash == "" {
		var ferr error
		nb.ContentHash, ferr = s.fingerprintErr(ctx, lib, nb) // reused by enrich
		if fingerprintUnknown(ferr) {
			return unknown("the new book's fingerprint couldn't be read")
		}
	}
	if nb.ContentHash == "" {
		return ignored("the fingerprints differ")
	}
	if nb.ContentHash != old.ContentHash {
		was, werr := s.byteFirstFingerprintErr(ctx, lib, nb)
		if fingerprintUnknown(werr) {
			return unknown("the new book's first file by bytes couldn't be read")
		}
		if was != old.ContentHash {
			return ignored("the fingerprints differ")
		}
	}
	if reclassified(from, catalog.Signature{Size: old.Size, IsFolder: old.IsFolder}, nb) {
		return ignored("one folder seen two ways, not a move")
	}
	if !s.carryMove(ctx, lib, from, nb.RelPath, nil) {
		return false, nil
	}
	s.carryFolderFavourites(ctx, lib, map[string]string{from: nb.RelPath}, nil)
	return true, nil
}

// onDisk is bookOnDisk's answer about a book indexed at a path.
type onDisk int

const (
	// diskPresent: a book resolves at the path, or the path can't be checked
	// (a permission), so its state stays where it is.
	diskPresent onDisk = iota
	// diskGone: no book is at the path any more.
	diskGone
	// diskUnknown: a look was given up on (a share that stopped answering),
	// failed as a share does (transientFault: EIO, ESTALE) or ctx ended, so
	// whether the book is there can't be known now.
	diskUnknown
)

// bookOnDisk reports whether the book indexed at rel is still on disk: a book
// resolves at exactly that path, every name along it as the folder listings
// spell it (resolveBook's dirPresent check, so a case-only rename on a
// case-insensitive disk counts as gone). It errs toward present: a path it
// can't check keeps its state where it is. Its looks are metadata.Bounded
// calls (ctx, at most lookTimeout): one given up on (a share that stopped
// answering), or failed as a share does but for a permission (transientFault),
// is diskUnknown, which no caller takes as gone either; one refused for good
// (a permission) is present. Only a path gone (ENOENT, ENOTDIR) is diskGone
// (metadata.ShareFault's rule).
func (s *Scanner) bookOnDisk(ctx context.Context, lib catalog.Library, rel string) onDisk {
	// A path whose stat or listing fails as a share does (metadata.ShareFault)
	// can't be checked: a fault a retry may get past (transientFault: EIO,
	// ESTALE) is diskUnknown, any other (a permission, a link loop) present,
	// its state kept where it is. Only the path gone (ENOENT, or ENOTDIR: a
	// folder above it replaced by a file) is gone. resolveBook reads the same
	// faults on its own looks as looks given up on.
	abs := absOf(lib, rel)
	look := s.lookTimeout(ctx)
	info, err := statLook(ctx, abs, look)
	if err == nil && info.IsDir() {
		_, err = readDirLook(ctx, abs, look)
	}
	switch {
	case err == nil:
	case metadata.Unanswered(ctx, err), transientFault(err):
		return diskUnknown
	case !metadata.ShareFault(err):
		return diskGone
	default:
		return diskPresent
	}
	b, err := s.resolveBook(ctx, lib, rel, nil)
	switch {
	case errors.Is(err, ErrNotIndexable), err == nil && b.RelPath != rel:
		return diskGone
	case err == nil:
		return diskPresent
	}
	if cause, faulted := metadata.Faulted(err); faulted && !transientFault(cause) {
		return diskPresent // a look refused for good (a permission): it can't be checked
	}
	if metadata.Unanswered(ctx, err) {
		return diskUnknown
	}
	return diskPresent
}

// splitParentOf is books.split_parent for a book IndexPath indexes, worked out as a
// full scan would (markSplitDiscs): the folder holding it when that is a disc set
// (discSets, from the folder's subtree), read only for a folder book with a disc
// name. A disc set with a `book` override would have made the joined book instead.
func (s *Scanner) splitParentOf(ctx context.Context, lib catalog.Library, b *catalog.Book, ignore *Ignore) (string, error) {
	parent := dirOf(b.RelPath)
	if !b.IsFolder || parent == "" || !metadata.IsDiscFolder(path.Base(b.RelPath)) {
		return "", nil
	}
	dirs, side, _, err := audioDirs(ctx, lib, absOf(lib, parent), parent, ignore, s.log, nil, nil, s.walkWidth(), s.lookTimeout(ctx))
	if err == nil {
		err = side.gaveUp() // a disc that wouldn't list: the set can't be known now
	}
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

func ext(name string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
}

// fingerprint is the file at absPath's move-detection fingerprint
// (metadata.Fingerprint), its open and reads bounded as a scan's tag read's
// are: "" when it can't be read in that time.
func (s *Scanner) fingerprint(ctx context.Context, absPath string) string {
	return metadata.Fingerprint(ctx, absPath, s.probeTimeout)
}

// fingerprintErr is book b's fingerprint (its primary file's, as fingerprint
// reads it) with why it is "" (metadata.FingerprintErr's error).
func (s *Scanner) fingerprintErr(ctx context.Context, lib catalog.Library, b *catalog.Book) (string, error) {
	return metadata.FingerprintErr(ctx, absOf(lib, primaryPath(b)), s.probeTimeout)
}

// detectMoves migrates durable user state (progress/bookmarks/notes/history)
// from a vanished path to a new path with matching content, so a moved/renamed
// file keeps its state, then carries the favourites of the folders those moves
// say were renamed (renamedFolders). It only does work when something both
// disappeared and appeared, keeping fingerprinting off the hot path of normal scans.
// found is the paths of books (discovery's). It returns the moves it carried
// state across (old path -> new path), logging each to rl.
//
// partial says discovery did not see the whole tree (a folder unreadable, or
// given up on: a share that stopped answering), so a book it did not find is
// not known gone: a copy of it found elsewhere would take its listening state
// while the book itself is kept. Each old path a new book matches is then
// looked at (bookOnDisk, bounded, erring toward present), and only one gone
// from disk moves.
func (s *Scanner) detectMoves(ctx context.Context, lib catalog.Library, sigs map[string]catalog.Signature,
	books []*catalog.Book, found map[string]bool, partial bool, rl *runLog) (moved map[string]string) {
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
		fp := s.fingerprint(ctx, absOf(lib, primaryPath(nb)))
		nb.ContentHash = fp // reused by enrich, avoiding a second read
		if fp == "" {
			continue
		}
		was := s.byteFirstFingerprint(ctx, lib, nb)
		for oldPath, ofp := range oldFP {
			if (ofp != fp && ofp != was) || reclassified(oldPath, sigs[oldPath], nb) {
				continue
			}
			if partial && s.bookOnDisk(ctx, lib, oldPath) != diskGone {
				// Not known gone: its state stays, and it is not looked at again
				// for another new book (on a share that stopped answering each
				// look waits out its time).
				s.log.Info("move ignored: the old book is not known gone after a partial discovery",
					"library", lib.Name, "from", oldPath, "to", nb.RelPath)
				delete(oldFP, oldPath)
				continue
			}
			if s.carryMove(ctx, lib, oldPath, nb.RelPath, rl) {
				moved[oldPath] = nb.RelPath
			}
			delete(oldFP, oldPath)
			break
		}
	}
	s.carryFolderFavourites(ctx, lib, moved, rl)
	return moved
}

// byteFirstFingerprint is the fingerprint of the file a folder book had first
// before natural order (#160), when that order puts another first: the first by
// bytes in its first folder (a joined book's first disc). An index from before
// stored that file's fingerprint, so a book moved before its first re-index is
// still found by it. "" when it is the same file.
func (s *Scanner) byteFirstFingerprint(ctx context.Context, lib catalog.Library, b *catalog.Book) string {
	fp, _ := s.byteFirstFingerprintErr(ctx, lib, b)
	return fp
}

// byteFirstFingerprintErr is byteFirstFingerprint with why it is ""
// (metadata.FingerprintErr's error; nil when the book has no such file).
func (s *Scanner) byteFirstFingerprintErr(ctx context.Context, lib catalog.Library, b *catalog.Book) (string, error) {
	if !b.IsFolder || len(b.Files) < 2 {
		return "", nil
	}
	first := b.Files[0].RelPath
	byteFirst, dir := first, path.Dir(first)
	for _, f := range b.Files[1:] {
		if path.Dir(f.RelPath) == dir && f.RelPath < byteFirst {
			byteFirst = f.RelPath
		}
	}
	if byteFirst == first {
		return "", nil
	}
	return metadata.FingerprintErr(ctx, absOf(lib, byteFirst), s.probeTimeout)
}

// fingerprintUnknown reports whether err, a fingerprint read's
// (metadata.FingerprintErr), says the file can't be read now rather than how
// it is: given up on, failed as a share does that a retry may get past
// (transientFault), or cut short as it was read. Not a lasting refusal (a
// permission, no regular file), which a retry would only meet again.
func fingerprintUnknown(err error) bool {
	return metadata.GaveUp(err) || transientFault(err) || errors.Is(err, io.ErrUnexpectedEOF)
}

// carryMove migrates a moved book's durable state from oldPath to newPath
// (catalog.MoveDurableState), logging it to rl (nil: the log only). It reports
// whether it did. A full scan's move detection and the Bridge's moves
// (IndexMoved) share it.
func (s *Scanner) carryMove(ctx context.Context, lib catalog.Library, oldPath, newPath string, rl *runLog) bool {
	if err := s.cat.MoveDurableState(ctx, lib.ID, oldPath, newPath); err != nil {
		s.log.Warn("move state failed", "from", oldPath, "to", newPath, "err", err)
		rl.add("error", "error", func(e *catalog.RunEvent) { e.Path, e.To, e.Detail = oldPath, newPath, err.Error() })
		return false
	}
	s.log.Info("detected move", "library", lib.Name, "from", oldPath, "to", newPath)
	rl.add("info", "moved", func(e *catalog.RunEvent) { e.Path, e.To = oldPath, newPath })
	return true
}

// carryFolderFavourites re-keys the favourites of the folders the moves (old
// path -> new path) say were renamed (renamedFolders): a navigation folder (an
// author, a series) has no book of its own to move, so a favourite on it follows
// the folder its books went to. rl may be nil.
func (s *Scanner) carryFolderFavourites(ctx context.Context, lib catalog.Library, moved map[string]string, rl *runLog) {
	for from, to := range renamedFolders(ctx, lib, moved, s.lookTimeout(ctx)) {
		if err := s.cat.MoveFolderFavourites(ctx, lib.ID, from, to); err != nil {
			s.log.Warn("move folder favourites failed", "library", lib.Name, "from", from, "to", to, "err", err)
			rl.add("error", "error", func(e *catalog.RunEvent) { e.Path, e.To, e.Detail = from, to, err.Error() })
		}
	}
}

// renamedFolders works out the folders a scan's moves (old path -> new path) say
// were renamed or moved: the ancestors of a moved book, paired up from the end -
// the folders holding the two paths, then on up while the names of the pair match
// ("Series A/Book" -> "Series B/Book" pairs "Series A" with "Series B"; a book
// renamed as it went still left its folder for the new one). A pair counts only
// when every move agrees on where the folder went and the old folder is gone from
// disk (a book moved out of a folder that is still there says nothing about it),
// looked at under ctx and timeout (dirPresent: one that can't be is present,
// and once a look is given up on, so is every folder not yet looked at: on a
// share that stopped answering each would wait out its time too).
func renamedFolders(ctx context.Context, lib catalog.Library, moved map[string]string, timeout time.Duration) map[string]string {
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
	renamed := map[string]string{}
	for from, to := range out {
		if to == "" {
			continue
		}
		present, err := dirPresent(ctx, lib, from, listed, timeout)
		if err != nil {
			break // given up on: it, and every folder not looked at yet, is present
		}
		if !present {
			renamed[from] = to
		}
	}
	return renamed
}

// dirPresent reports whether a folder (or file) at exactly this path is on disk. A
// case-insensitive filesystem still answers to the old name of a folder renamed
// only in case ("WIth" -> "With"), or of any folder under one so renamed, so when
// the path still answers, each name along it must be in its parent's listing
// (listed caches the listings). It errs toward present: a folder it can't check
// (a look failed as a share does, metadata.ShareFault) keeps its favourites. Its lstat and listings are metadata.Bounded calls (ctx,
// at most timeout each): err is one given up on (metadata.GaveUp) or ctx's, the
// answer then present.
func dirPresent(ctx context.Context, lib catalog.Library, rel string, listed map[string]map[string]bool, timeout time.Duration) (bool, error) {
	if _, err := metadata.LstatBounded(ctx, absOf(lib, rel), timeout); err != nil {
		if metadata.Unanswered(ctx, err) {
			return true, err
		}
		// Gone (ENOENT, or ENOTDIR: a folder above it replaced), else a look a
		// share failed (metadata.ShareFault), which can't be checked.
		return metadata.ShareFault(err), nil
	}
	for dir := rel; dir != "."; dir = path.Dir(dir) {
		parent := path.Dir(dir)
		names, ok := listed[parent]
		if !ok {
			entries, err := metadata.ReadDirBounded(ctx, absOf(lib, parent), timeout)
			if metadata.Unanswered(ctx, err) {
				return true, cmp.Or(err, ctx.Err())
			}
			if err != nil {
				return true, nil
			}
			names = make(map[string]bool, len(entries))
			for _, e := range entries {
				names[e.Name()] = true
			}
			listed[parent] = names
		}
		if !names[path.Base(dir)] {
			return false, nil
		}
	}
	return true, nil
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
	return old.IsFolder && nb.IsFolder && metadata.IsDiscFolder(path.Base(nb.RelPath)) && old.Size > nb.Size
}

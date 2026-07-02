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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Scanner walks library roots and keeps the catalog index up to date. The
// filesystem view does not depend on it, so scanning can run in the background
// after startup without blocking client browsing.
type Scanner struct {
	cat         *catalog.Catalog
	ffprobePath string
	log         *slog.Logger

	mu       sync.Mutex
	scanning map[int64]bool         // library IDs currently scanning
	progress map[int64]ScanProgress // latest progress per library (for the admin UI)
}

// ScanProgress reports how far a (possibly running) library scan has gotten, so
// the admin UI can show a counter instead of guessing.
type ScanProgress struct {
	Running bool `json:"running"`
	Total   int  `json:"total"`
	Done    int  `json:"done"`
	Indexed int  `json:"indexed"`
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
		scanning:    map[int64]bool{},
		progress:    map[int64]ScanProgress{},
	}
}

// Progress returns the latest scan progress for a library (the zero value if it
// has never been scanned this process).
func (s *Scanner) Progress(libID int64) ScanProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progress[libID]
}

func (s *Scanner) setProgress(libID int64, p ScanProgress) {
	s.mu.Lock()
	s.progress[libID] = p
	s.mu.Unlock()
}

// ScanResult summarizes a scan.
type ScanResult struct {
	Indexed int
	Removed int
	Elapsed time.Duration
}

// ErrLibraryUnavailable means the library root could not be read, or it
// returned no audio files while the index still has books - a strong signal
// that a network share (e.g. SMB/NFS) is unmounted. The scanner refuses to
// prune in this case so a dropped mount never wipes the rebuildable index.
// (Durable user state - progress/bookmarks/notes - is path-keyed with no FK to
// books since migration 0003, so it would survive a prune anyway; protecting the
// index still avoids a needless full re-scan and a transient empty catalog.)
var ErrLibraryUnavailable = errors.New("library root unavailable; skipping scan to protect the index")

// ErrNotIndexable means a resolved path is not a book (e.g. a directory that
// holds no audio directly, or a directory the detector treats as a collection of
// separate books rather than one book).
var ErrNotIndexable = errors.New("path is not an indexable book")

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

// Scan indexes a single library. It is safe to call concurrently for different
// libraries; concurrent calls for the same library are coalesced (the second
// returns immediately).
func (s *Scanner) Scan(ctx context.Context, lib catalog.Library) (*ScanResult, error) {
	s.mu.Lock()
	if s.scanning[lib.ID] {
		s.mu.Unlock()
		return &ScanResult{}, nil
	}
	s.scanning[lib.ID] = true
	s.mu.Unlock()
	s.setProgress(lib.ID, ScanProgress{Running: true})
	defer func() {
		s.mu.Lock()
		delete(s.scanning, lib.ID)
		p := s.progress[lib.ID]
		p.Running = false
		s.progress[lib.ID] = p
		s.mu.Unlock()
	}()

	start := time.Now()
	sigs, err := s.cat.Signatures(ctx, lib.ID)
	if err != nil {
		return nil, err
	}

	// Guard against an unavailable root (e.g. an unmounted network share):
	// WalkDir would otherwise silently yield zero files and the prune step
	// would wipe the index. Fail fast before discovery.
	if info, statErr := os.Stat(lib.Root); statErr != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: %q: %v", ErrLibraryUnavailable, lib.Root, statErr)
	}

	overrides, err := s.cat.FolderOverrides(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	// Discovery walks the whole tree (slow on a large network share) and emits no
	// per-file output, so bookend it with logs - otherwise a long scan looks hung.
	s.log.Info("scan started: discovering books", "library", lib.Name, "root", lib.Root)
	books, partialDiscovery, err := discoverAuto(lib, overrides, s.log)
	if err != nil {
		return nil, err
	}
	s.log.Info("discovery complete; indexing", "library", lib.Name, "books", len(books))

	// A root that exists but now contains no audio files, while the index still
	// has books, almost always means the mount dropped to an empty directory.
	// Refuse to prune so the index (and cascaded progress/bookmarks) survive.
	if len(books) == 0 && len(sigs) > 0 {
		return nil, fmt.Errorf("%w: %q returned 0 audio files but %d are indexed",
			ErrLibraryUnavailable, lib.Root, len(sigs))
	}

	// Carry user state across moved/renamed files before indexing.
	s.detectMoves(ctx, lib, sigs, books)

	res := &ScanResult{}
	keep := make(map[string]bool, len(books))
	lastLog := time.Now()
	for i, b := range books {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		s.setProgress(lib.ID, ScanProgress{Running: true, Total: len(books), Done: i, Indexed: res.Indexed})
		// Heartbeat so a long indexing pass (ffprobe per file over a network share)
		// shows progress in the logs, not just in the admin UI counter.
		if time.Since(lastLog) >= 15*time.Second {
			s.log.Info("scan progress", "library", lib.Name, "done", i, "total", len(books), "indexed", res.Indexed)
			lastLog = time.Now()
		}
		keep[b.RelPath] = true
		if old, ok := sigs[b.RelPath]; ok && old.MTime == b.MTime && old.Size == b.Size &&
			(s.ffprobePath == "" || (old.Duration > 0 && old.Codec != "")) {
			// Unchanged since last scan; only skip the probe when ffprobe is
			// disabled, or a prior probe already stored both duration and codec
			// (so books indexed before the codec column get it backfilled).
			if old.ContentHash != "" {
				continue
			}
			// The fingerprint never landed (a one-off read error). Without one a
			// later move of this book couldn't be detected and its progress would
			// be stranded, so re-enrich to backfill it - but probe the fingerprint
			// first: if the file still can't be read, the full re-enrich would fail
			// the same way, so a persistently unreadable book costs one cheap
			// failed open per scan instead of an ffprobe sweep forever.
			b.ContentHash = fingerprintFile(filepath.Join(lib.Root, filepath.FromSlash(primaryPath(b))))
			if b.ContentHash == "" {
				continue
			}
		}
		s.enrich(lib, b)
		if _, err := s.cat.UpsertBook(ctx, b); err != nil {
			s.log.Warn("index book failed", "library", lib.Name, "path", b.RelPath, "err", err)
			continue
		}
		res.Indexed++
	}
	s.setProgress(lib.ID, ScanProgress{Running: true, Total: len(books), Done: len(books), Indexed: res.Indexed})

	// Only prune when discovery saw the whole tree. If a subtree was unreadable
	// (partialDiscovery), its books are missing from `keep` through a mount/permission
	// fault, not a real deletion - pruning would drop still-present books and their
	// cascaded index rows. Skip it; a later clean scan reconciles genuine deletions.
	if partialDiscovery {
		s.log.Warn("skipping prune after partial discovery to protect the index",
			"library", lib.Name)
	} else {
		removed, err := s.cat.DeleteBooksNotIn(ctx, lib.ID, keep)
		if err != nil {
			return res, err
		}
		res.Removed = removed
	}
	// Re-apply path-keyed enrichment (e.g. ASINs attached by the manager) onto the
	// freshly-indexed books - it lives outside the rebuildable index, so a scan must
	// restore it.
	if err := s.cat.ApplyEnrichments(ctx, lib.ID); err != nil {
		s.log.Warn("apply enrichments failed", "library", lib.Name, "err", err)
	}
	res.Elapsed = time.Since(start)
	s.log.Info("library scanned", "library", lib.Name,
		"indexed", res.Indexed, "removed", res.Removed, "elapsed", res.Elapsed)
	return res, nil
}

// enrich fills metadata for a book from its primary file (tags + ffprobe) and
// folder context (path heuristics, sibling cover art).
func (s *Scanner) enrich(lib catalog.Library, b *catalog.Book) {
	primary := primaryPath(b)
	// Baseline from the path, then overlay embedded tags/probe which are
	// authoritative when present.
	base := metadata.DeriveFromPath(b.RelPath, b.IsFolder)
	b.Title, b.Author, b.Series, b.SeriesIndex = base.Title, base.Author, base.Series, base.SeriesIndex

	abs := filepath.Join(lib.Root, filepath.FromSlash(primary))
	md, _ := metadata.Extract(abs, s.ffprobePath)
	if md != nil {
		b.Title = chooseTitle(md.Title, b.Title)
		b.Author = firstNonEmpty(md.Author, b.Author)
		b.Series = firstNonEmpty(md.Series, b.Series)
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
	bookAbs := filepath.Join(lib.Root, filepath.FromSlash(b.RelPath))
	if b.IsFolder {
		b.CoverPath = findCover(lib.Root, bookAbs, true)
		// A multi-CD book's art often sits in the parent (e.g. ".../Book/CD1" with
		// the cover in ".../Book"); fall back there for disc-part subfolders.
		if b.CoverPath == "" && isDiscFolder(filepath.Base(bookAbs)) {
			b.CoverPath = findCover(lib.Root, filepath.Dir(bookAbs), true)
		}
	} else {
		b.CoverPath = findCover(lib.Root, filepath.Dir(bookAbs), false)
	}
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
	for i := range b.Files {
		f := &b.Files[i]
		abs := filepath.Join(lib.Root, filepath.FromSlash(f.RelPath))
		md, _ := metadata.Extract(abs, s.ffprobePath)
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
				Title:      partTitle(f.RelPath),
				FileIndex:  f.Seq,
				FilePath:   f.RelPath,
				Start:      0,
				End:        dur,
				BookOffset: cum,
			})
			idx++
		}
		f.Duration = dur
		cum += dur
	}
	b.Duration = cum
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
// gets it wrong (see booksInDir).
// The bool return reports whether discovery hit any per-entry error (an
// unreadable/permission-denied subtree on a partially-mounted share). When true,
// the caller must NOT prune: the books under the failed subtree are absent from the
// result through no fault of the filesystem-of-truth, and pruning them would drop a
// still-present book and its cascaded index rows until the mount recovers.
func discoverAuto(lib catalog.Library, overrides map[string]string, log *slog.Logger) (books []*catalog.Book, hadErrors bool, err error) {
	dirs := map[string]bool{}
	rootClean := filepath.Clean(lib.Root)
	err = filepath.WalkDir(lib.Root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Warn and skip just the unreadable entry rather than aborting the whole
			// scan, but record that discovery was partial so the caller skips pruning.
			hadErrors = true
			log.Warn("skipping unreadable path during discovery",
				"library", lib.Name, "path", path, "err", walkErr)
			return nil
		}
		if d.IsDir() {
			// Skip hidden directories (.Trash-1000, Syncthing .stversions, .git) so
			// their audio isn't indexed into books unreachable via the fs browse view
			// (which also hides them). Never skip the library root itself, even when
			// its own name begins with a dot; WalkDir passes the root argument
			// verbatim, so a direct comparison identifies the root callback.
			if path != lib.Root && isHidden(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if isHidden(d.Name()) || !metadata.IsAudio(d.Name()) {
			return nil
		}
		dirs[filepath.Dir(path)] = true
		return nil
	})
	if err != nil {
		return nil, hadErrors, err
	}
	for dir := range dirs {
		books = append(books, booksInDir(lib, dir, filepath.Clean(dir) == rootClean, overrides)...)
	}
	return books, hadErrors, nil
}

// booksInDir turns the audio files directly inside absDir into books. The model
// matches the dominant "folder per book" convention (and Audiobookshelf): a
// directory that directly contains audio is ONE book, with all those files as its
// tracks/chapters - whether it holds a single m4b or fifty mp3 chapters. The two
// exceptions: audio sitting directly in the library root has no enclosing book
// folder, so each such file is its own single-file book ("flat"); and a folder of
// loose single-file books (one book per file) is expressed with the `collection`
// override. `book` forces the folder-is-one-book reading (e.g. at the root).
func booksInDir(lib catalog.Library, absDir string, isRoot bool, overrides map[string]string) []*catalog.Book {
	audio := audioEntries(absDir)
	if len(audio) == 0 {
		return nil
	}
	asBook := func() []*catalog.Book {
		if b := folderBook(lib, absDir); b != nil {
			return []*catalog.Book{b}
		}
		return nil
	}
	switch overrides[relPathOf(lib.Root, absDir)] {
	case catalog.OverrideBook:
		return asBook()
	case catalog.OverrideCollection:
		return fileBooksIn(lib, absDir, audio)
	}
	if isRoot {
		return fileBooksIn(lib, absDir, audio)
	}
	return asBook()
}

// audioEntries returns the non-hidden audio files directly inside absDir, in the
// stable name order os.ReadDir provides.
func audioEntries(absDir string) []os.DirEntry {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil
	}
	var audio []os.DirEntry
	for _, de := range entries {
		if de.IsDir() || isHidden(de.Name()) || !metadata.IsAudio(de.Name()) {
			continue
		}
		audio = append(audio, de)
	}
	return audio
}

// fileBooksIn builds one single-file book per audio file in absDir.
func fileBooksIn(lib catalog.Library, absDir string, audio []os.DirEntry) []*catalog.Book {
	var books []*catalog.Book
	for _, de := range audio {
		info, err := de.Info()
		if err != nil {
			continue
		}
		books = append(books, fileBook(lib, filepath.Join(absDir, de.Name()), info))
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

// folderBook builds a (possibly multi-file) book from the audio files directly
// inside absDir, or returns nil if the directory contains no audio. os.ReadDir
// returns entries already sorted by name, giving stable part ordering.
func folderBook(lib catalog.Library, absDir string) *catalog.Book {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil
	}
	var files []catalog.BookFile
	var totalSize, maxMTime int64
	var added string // earliest file added time = when the book first appeared
	for _, de := range entries {
		if de.IsDir() || isHidden(de.Name()) || !metadata.IsAudio(de.Name()) {
			continue
		}
		info, ierr := de.Info()
		if ierr != nil {
			continue
		}
		abs := filepath.Join(absDir, de.Name())
		frel, _ := filepath.Rel(lib.Root, abs)
		files = append(files, catalog.BookFile{
			RelPath: filepath.ToSlash(frel), Seq: len(files), Format: ext(de.Name()), Size: info.Size(),
		})
		totalSize += info.Size()
		if m := info.ModTime().Unix(); m > maxMTime {
			maxMTime = m
		}
		if a := addedAt(abs, info); added == "" || a < added {
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
	// SafeJoin is the security gate (rejects traversal and symlink escapes). It
	// returns a symlink-RESOLVED path, but we derive the working path from an
	// unresolved join so the rel_path computed by fileBook/folderBook matches the
	// full scan, which walks lib.Root unresolved. Using SafeJoin's resolved path
	// here would yield a "../"-laden rel_path whenever any component of lib.Root
	// is a symlink (e.g. macOS /tmp -> /private/tmp, or a NAS /data -> /mnt/...).
	if _, err := SafeJoin(lib.Root, relPath); err != nil {
		return nil, err
	}
	abs := filepath.Join(lib.Root, filepath.FromSlash(relPath))
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotIndexable, err)
	}

	overrides, err := s.cat.FolderOverrides(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	// Classify the containing directory exactly as a full scan would, then pick
	// the book the requested path resolves to: the folder book itself, a
	// single-file book, or the folder book a clicked part belongs to.
	dir := abs
	if !info.IsDir() {
		dir = filepath.Dir(abs)
	}
	rootClean := filepath.Clean(lib.Root)
	candidates := booksInDir(lib, dir, filepath.Clean(dir) == rootClean, overrides)
	book := pickBook(candidates, relPathOf(lib.Root, abs))
	if book == nil {
		return nil, fmt.Errorf("%w: no book at %q", ErrNotIndexable, relPath)
	}

	s.enrich(lib, book)
	id, err := s.cat.UpsertBook(ctx, book)
	if err != nil {
		return nil, err
	}
	// Restore any path-keyed enrichment (e.g. an ASIN) onto this just-indexed book.
	// Scope it to the one book we resolved - IndexPath is a hot per-request path, so
	// re-sweeping the whole library here would be O(enriched books) per lookup.
	if err := s.cat.ApplyEnrichment(ctx, lib.ID, book.RelPath); err != nil {
		s.log.Warn("apply enrichment failed", "library", lib.Name, "path", book.RelPath, "err", err)
	}
	return s.cat.GetBook(ctx, id)
}

// pickBook returns the book from candidates whose path matches want - the book's
// own rel_path (single-file or folder book) or, for a part the client clicked
// inside a folder book, one of that book's files.
func pickBook(candidates []*catalog.Book, want string) *catalog.Book {
	for _, b := range candidates {
		if b.RelPath == want {
			return b
		}
		for _, f := range b.Files {
			if f.RelPath == want {
				return b
			}
		}
	}
	return nil
}

func ext(name string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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
// file keeps its state. It only does work when something both disappeared and
// appeared, keeping fingerprinting off the hot path of normal scans.
func (s *Scanner) detectMoves(ctx context.Context, lib catalog.Library, sigs map[string]catalog.Signature, books []*catalog.Book) {
	discovered := make(map[string]bool, len(books))
	for _, b := range books {
		discovered[b.RelPath] = true
	}
	var disappeared []string
	for relPath := range sigs {
		if !discovered[relPath] {
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
		return
	}
	// The stored fingerprints for the disappeared paths are already in sigs
	// (Signatures selects content_hash), so no extra query is needed.
	oldFP := make(map[string]string, len(disappeared))
	for _, p := range disappeared {
		if h := sigs[p].ContentHash; h != "" {
			oldFP[p] = h
		}
	}
	for _, nb := range newBooks {
		fp := fingerprintFile(filepath.Join(lib.Root, filepath.FromSlash(primaryPath(nb))))
		nb.ContentHash = fp // reused by enrich, avoiding a second read
		if fp == "" {
			continue
		}
		for oldPath, ofp := range oldFP {
			if ofp == fp {
				if err := s.cat.MoveDurableState(ctx, lib.ID, oldPath, nb.RelPath); err != nil {
					s.log.Warn("move state failed", "from", oldPath, "to", nb.RelPath, "err", err)
				} else {
					s.log.Info("detected move", "library", lib.Name, "from", oldPath, "to", nb.RelPath)
				}
				delete(oldFP, oldPath)
				break
			}
		}
	}
}

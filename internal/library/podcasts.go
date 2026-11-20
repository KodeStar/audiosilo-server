package library

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// podcastLooks are a podcast scan's or rescan's looks at a show (its folder,
// feed.json, its listing and each entry's info, the publish barriers, a
// chapter attachment), each a metadata.Bounded call of its own (ctx, at most
// timeout, on the path's mount), as a books scan's looks and the root's
// listing are: on a share that stopped answering each is given up on
// (metadata.GaveUp) and, once enough hang, refused at once, so the show reads
// as unread and the scan goes on, never held at a look.
type podcastLooks struct {
	ctx     context.Context
	timeout time.Duration
}

// podcastLooks are the looks of a scan or rescan under ctx: each given the
// time of a look at a library file (lookTimeout).
func (s *Scanner) podcastLooks(ctx context.Context) podcastLooks {
	return podcastLooks{ctx: ctx, timeout: s.lookTimeout(ctx)}
}

// lookAt is fn, a look at p that reads no file's contents (an lstat, an
// entry's info), as one of l's bounded looks; readAt is one that reads p.
func lookAt[T any](l podcastLooks, p string, fn func() (T, error)) (T, error) {
	return metadata.LookBounded(l.ctx, p, l.timeout, fn)
}

func readAt[T any](l podcastLooks, p string, fn func() (T, error)) (T, error) {
	return metadata.Bounded(l.ctx, p, l.timeout, fn, nil)
}

func (l podcastLooks) lstat(p string) (os.FileInfo, error) {
	return lookAt(l, p, func() (os.FileInfo, error) { return podcastLstat(p) })
}

func (l podcastLooks) readDir(p string) ([]fs.DirEntry, error) {
	return metadata.ReadDirBounded(l.ctx, p, l.timeout)
}

// info is the info of e, an entry dir listed.
func (l podcastLooks) info(dir string, e fs.DirEntry) (fs.FileInfo, error) {
	return lookAt(l, filepath.Join(dir, e.Name()), e.Info)
}

// join is ContentJoin of show under root.
func (l podcastLooks) join(root, show string) (string, error) {
	return ContentJoinBounded(l.ctx, root, show, l.timeout)
}

// readFeed is feed.ReadFile of a show's feed.json, filename under the library
// root root, read through it.
func (l podcastLooks) readFeed(root, filename, show string, log *slog.Logger) (*feed.File, error) {
	return readAt(l, filename, func() (*feed.File, error) { return feed.ReadFile(root, filename, show, log) })
}

func feedSignature(info os.FileInfo) string {
	return strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

func (s *Scanner) scanPodcasts(ctx context.Context, lib catalog.Library, res *ScanResult) (*ScanResult, error) {
	// The shows known before the root is listed are the only ones this scan may
	// prune: one the Bridge indexes meanwhile isn't in the listing yet.
	old, err := s.cat.PodcastShowIDs(ctx, lib.ID)
	if err != nil {
		return res, err
	}
	// The root's listing is bounded as a books scan's looks are (the probe's
	// time, on the root's mount): a root on a share that stopped answering is
	// the library unavailable, not a scan held at its first look. The looks
	// at each show after it are bounded the same way (podcastLooks): a share
	// that stops answering between them leaves its shows unread, the run
	// partial.
	entries, err := metadata.ReadDirBounded(ctx, lib.Root, s.probeTimeout)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return res, cerr
		}
		return res, ErrLibraryUnavailable
	}
	if len(entries) == 0 && len(old) > 0 {
		return res, ErrLibraryUnavailable
	}
	keep := map[string]bool{}
	// partial: a show folder could not be read; unread: that, or its feed.json
	// can't be used for now (unreadPodcastFolder). A stray folder is neither
	// (strayPodcastFolder).
	partial, unread := false, false
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !e.IsDir() || !feed.ShowPath(e.Name()) {
			continue
		}
		n, changed, err := s.indexPodcastShow(ctx, lib, e.Name(), false)
		unread = unread || unreadPodcastFolder(err)
		if errors.Is(err, ErrNotIndexable) {
			continue
		}
		if err != nil {
			partial = true
			res.Errors++
			s.log.Warn("podcast show could not be read", "library", lib.ID)
			continue
		}
		keep[e.Name()] = true
		res.Books += n
		if changed {
			if _, exists := old[e.Name()]; exists {
				res.Updated += n
			} else {
				res.Added += n
			}
		}
	}
	res.Partial = partial
	// Before the prune: a folder renamed by hand leaves its show's row at the
	// old name until then, and the move takes that row's episodes' listening
	// state along. With a folder unread, a move over another show's state
	// waits (catalog.MovePodcastShows); a plain rename does not, whatever else
	// the root holds (a lost+found). A move's old folders keep their rows
	// through the prune: a held move's row is the show's one way back to a
	// place it has there with no follow, feed state or policy (an applied
	// move dropped its own already).
	for _, from := range s.movePodcastShows(ctx, lib, "", unread) {
		keep[from] = true
	}
	if !partial {
		var gone []string
		for show := range old {
			if !keep[show] {
				gone = append(gone, show)
			}
		}
		if err := s.cat.PrunePodcastShows(ctx, lib.ID, gone); err != nil {
			return res, err
		}
	}
	// What an older server's books scan left behind (a rollback), whatever
	// this scan could read.
	if err := s.cat.PruneNonEpisodeBooks(ctx, lib.ID); err != nil {
		return res, err
	}
	return res, nil
}

// IndexPodcastPath is the Bridge's targeted rescan. Only show folders and an
// already indexed episode can name a show; it never discovers arbitrary audio.
// The show is indexed at the spelling the root lists (listedShowFolder), which
// it answers.
func (s *Scanner) IndexPodcastPath(ctx context.Context, lib catalog.Library, p string) (string, int, error) {
	p = catalog.CleanRelPath(p)
	show := p
	if strings.Contains(p, "/") {
		b, err := s.cat.GetBookByPath(ctx, lib.ID, p)
		if err != nil || b.Episode == nil {
			return "", 0, ErrNotIndexable
		}
		show = b.Episode.ShowPath
	}
	if lib.Kind != catalog.KindPodcasts || !feed.ShowPath(show) {
		return "", 0, ErrNotIndexable
	}
	show = s.listedShowFolder(ctx, lib.Root, show)
	n, _, err := s.indexPodcastShow(ctx, lib, show, true)
	if err == nil {
		s.movePodcastShows(ctx, lib, show, false)
	}
	return show, n, err
}

// listedShowFolder is show as the root of a library lists it. The Bridge asks
// for a rescan at its feed.json's spelling (NFC, or the case a folder had
// before a rename by hand), which a disk that ignores the difference (APFS,
// SMB) opens as the folder it lists in another, and a scan indexes that one:
// indexed at the spelling asked, the folder would have a second row, which
// only a move would fold back. Only a listed name that differs from show in
// Unicode normal form or case and is the same folder (os.SameFile) is taken;
// show as given otherwise (listed itself, gone, two folders on a disk that
// keeps the two apart, a root or folder that can't be read or did not answer
// in time: indexPodcastShow answers those), nor a listed name no show folder
// may have (feed.ShowPath: a scan skips it). The root is listed once (names
// only, no stat of each), and only a name that matches is looked at, all of it
// within one lookTimeout (metadata.ReadDirNamesBounded, LstatBounded).
func (s *Scanner) listedShowFolder(ctx context.Context, root, show string) string {
	deadline := time.Now().Add(s.lookTimeout(ctx))
	names, err := metadata.ReadDirNamesBounded(ctx, root, metadata.WaitLeft(deadline))
	if err != nil {
		return show
	}
	nfc := norm.NFC.String(show)
	var asked os.FileInfo
	for _, name := range names {
		if name == show {
			return show
		}
		if !strings.EqualFold(norm.NFC.String(name), nfc) || !feed.ShowPath(name) {
			continue
		}
		if asked == nil {
			if asked, err = metadata.LstatBounded(ctx, filepath.Join(root, show), metadata.WaitLeft(deadline)); err != nil {
				return show
			}
		}
		if st, err := metadata.LstatBounded(ctx, filepath.Join(root, name), metadata.WaitLeft(deadline)); err == nil && st.IsDir() && os.SameFile(asked, st) {
			return name
		}
	}
	return show
}

// movePodcastShows moves the durable state of a show folder renamed by hand
// (follows, feed state, retention policy, listening state) to the folder that
// holds the show now, and drops the old folder's row. The index finds the
// show at both paths by its show_id (catalog.PodcastIdentityPaths); the disk
// tells a rename from a copy: an identity moves only when exactly one of its
// paths holds it (its feed.json names it) and every other one is known not
// to (gone, no feed.json, not a folder, or another show's). A copy, both
// folders holding the show, moves and merges nothing, nor does a path that
// can't be read or whose feed.json is invalid (podcastHeldID). With at not ""
// (a targeted rescan), only the identities at that path are looked at. A
// folder whose row names the moving show while it holds another (two folder
// names swapped by hand) is indexed again first, so both shows move in one
// call. unread (a scan's) is that a folder the root lists could not be read
// or its feed.json used (unreadPodcastFolder); a rescan finds that out itself
// (indexNewPodcastFolders, stalePodcastRow). It goes to
// catalog.MovePodcastShows, which then holds a move over another show's
// state. It answers the old folders of the
// moves it planned, applied or held. Failures are logged: the next scan or
// rescan looks again.
func (s *Scanner) movePodcastShows(ctx context.Context, lib catalog.Library, at string, unread bool) (from []string) {
	// The root's folder names, read when first needed (fresh, not a scan's
	// listing from before its indexing: a copy made since is a folder too).
	var listed map[string]bool
	moves, err := s.settledPodcastMoves(ctx, lib, at, &listed)
	if err == nil && at != "" && len(moves) > 0 {
		// A rescan knows only the folders indexed so far. Another show may
		// have been renamed by hand too, into a folder not indexed yet (B to
		// a new name, then this one to B's old name): its follows still sit
		// at this move's folder, and a move without B's own would replace
		// them. Index such folders first, as a scan does, so both move in
		// one call (a folder whose row no longer describes it counts as
		// unread instead: stalePodcastRow).
		var n int
		var skipped bool
		if n, skipped, err = s.indexNewPodcastFolders(ctx, lib, &listed); err == nil && n > 0 {
			moves, err = s.settledPodcastMoves(ctx, lib, at, &listed)
		}
		unread = unread || skipped
		if err == nil && !unread && len(moves) > 0 {
			unread, err = s.stalePodcastRow(ctx, lib, moves, listed)
		}
	}
	if err != nil {
		s.log.Warn("podcasts: looking for renamed show folders failed", "library", lib.ID, "err", err)
		return nil
	}
	for _, m := range moves {
		from = append(from, m.From...)
	}
	if len(moves) == 0 {
		return from
	}
	n, err := s.cat.MovePodcastShows(ctx, lib.ID, moves, unread)
	if err != nil {
		s.log.Warn("podcasts: moving a renamed show folder's state failed", "library", lib.ID, "err", err)
		return from
	}
	if n > 0 {
		s.log.Info("podcasts: moved the state of show folders renamed by hand", "library", lib.ID, "shows", n)
	}
	return from
}

// settledPodcastMoves plans, and plans once more when the plan indexed a
// folder again (planPodcastMoves' again).
func (s *Scanner) settledPodcastMoves(ctx context.Context, lib catalog.Library, at string, listed *map[string]bool) ([]catalog.PodcastMove, error) {
	moves, again, err := s.planPodcastMoves(ctx, lib, at, listed, true)
	if err == nil && again {
		moves, _, err = s.planPodcastMoves(ctx, lib, at, listed, false)
	}
	return moves, err
}

// indexNewPodcastFolders indexes the show folders the root lists that have no
// index row yet, and answers how many it indexed and whether one was unread
// (unreadPodcastFolder; a stray folder is not: strayPodcastFolder).
func (s *Scanner) indexNewPodcastFolders(ctx context.Context, lib catalog.Library, listed *map[string]bool) (n int, unread bool, err error) {
	known, err := s.cat.PodcastShowIDs(ctx, lib.ID)
	if err != nil {
		return 0, false, err
	}
	l := s.podcastLooks(ctx)
	if *listed == nil {
		if *listed, err = podcastFolderNames(l, lib.Root); err != nil {
			return 0, false, err
		}
	}
	for name := range *listed {
		if err := ctx.Err(); err != nil {
			return n, unread, err
		}
		if !feed.ShowPath(name) {
			continue
		}
		if _, ok := known[name]; ok {
			continue
		}
		switch _, _, err := s.indexPodcastShow(ctx, lib, name, false); {
		case err == nil:
			n++
		case unreadPodcastFolder(err):
			unread = true
		}
	}
	return n, unread, nil
}

// stalePodcastRow is a rescan's unread for catalog.MovePodcastShows: whether
// an indexed folder the root lists (listed) holds a show other than its row's
// or can't be told (podcastHeldID). The rescan leaves such a row as it is, and
// the show now in it may be the one whose state sits at a move's new folder,
// so the move waits as for an unread folder. Only the show identity counts,
// and the folders are read only when a move's new folder carries the state of
// another show indexed nowhere (catalog.PodcastMovesOverOthers), the only move
// that can wait on it. With none it answers true unread: the index was asked
// outside the move's transaction, and by then every other show at a new folder
// is indexed elsewhere, which holds the move whatever unread says. So true
// changes nothing unless such a show lost its row in between (a scan's prune,
// a rescan), and then the move waits for the next look instead of applying
// over state these folders were never read for.
func (s *Scanner) stalePodcastRow(ctx context.Context, lib catalog.Library, moves []catalog.PodcastMove, listed map[string]bool) (bool, error) {
	if over, err := s.cat.PodcastMovesOverOthers(ctx, lib.ID, moves); err != nil || !over {
		return true, err
	}
	known, err := s.cat.PodcastShowIDs(ctx, lib.ID)
	if err != nil {
		return false, err
	}
	l := s.podcastLooks(ctx)
	for name := range listed {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		id, ok := known[name]
		if !ok {
			continue
		}
		if now, ok := podcastHeldID(l, lib.Root, name); !ok || now != "" && now != id {
			return true, nil
		}
	}
	return false, nil
}

// planPodcastMoves is movePodcastShows' look at the disk. With reindex, a
// path whose row names the moving show while its folder holds another is
// indexed again, and again reports that the index changed under the plan.
func (s *Scanner) planPodcastMoves(ctx context.Context, lib catalog.Library, at string, listed *map[string]bool, reindex bool) (moves []catalog.PodcastMove, again bool, err error) {
	found, err := s.cat.PodcastIdentityPaths(ctx, lib.ID, at)
	if err != nil {
		return nil, false, err
	}
	l := s.podcastLooks(ctx)
identities:
	for id, paths := range found {
		occupied := map[string]bool{} // the path holds a show, this one or another
		var holders []catalog.PodcastIdentityPath
		for _, p := range paths {
			now, ok := podcastHeldID(l, lib.Root, p.Path)
			if !ok {
				continue identities
			}
			occupied[p.Path] = now != ""
			if now == id {
				holders = append(holders, p)
			}
		}
		if len(holders) > 1 {
			// On a case-insensitive or normalizing file system the old
			// spelling of a folder renamed only in case or form still
			// opens it: only the name the root lists is the folder.
			if *listed == nil {
				if *listed, err = podcastFolderNames(l, lib.Root); err != nil {
					return nil, false, err
				}
			}
			holders = slices.DeleteFunc(holders, func(p catalog.PodcastIdentityPath) bool {
				if (*listed)[p.Path] {
					return false
				}
				occupied[p.Path] = false
				return true
			})
		}
		// Exactly one folder holds the show, and the index knows it there:
		// more is a copy, none or an unindexed one is left to a later look.
		if len(holders) != 1 || !holders[0].Indexed {
			continue
		}
		files, ok := podcastFeedFiles(l, lib.Root, holders[0].Path)
		if !ok {
			continue
		}
		m := catalog.PodcastMove{ShowID: id, To: holders[0].Path, Files: files}
		for _, p := range paths {
			if p.Path == m.To {
				continue
			}
			if reindex && p.Indexed && occupied[p.Path] {
				// Its row names this show, its folder another: index it
				// as that one, then look again.
				if _, _, err := s.indexPodcastShow(ctx, lib, p.Path, true); err != nil {
					s.log.Warn("podcasts: re-indexing a show folder renamed by hand failed", "library", lib.ID, "err", err)
					continue identities
				}
				again = true
			}
			m.From = append(m.From, p.Path)
		}
		moves = append(moves, m)
	}
	return moves, again, nil
}

// podcastFolderNames are the names of the folders root lists, as listed.
func podcastFolderNames(l podcastLooks, root string) (map[string]bool, error) {
	entries, err := l.readDir(root)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out[e.Name()] = true
		}
	}
	return out, nil
}

// podcastFeedFiles are the episode file names feed.json in folder show of
// root lists, whatever folder its paths name: one renamed by hand names its
// old folder until the Bridge takes it in, and where that old name now holds
// another show (names swapped or reused) no index row lists them. ok is false
// when feed.json can't be read.
func podcastFeedFiles(l podcastLooks, root, show string) (files []string, ok bool) {
	dir, err := l.join(root, show)
	if err != nil {
		return nil, false
	}
	raw, err := readAt(l, filepath.Join(dir, "feed.json"), func() ([]byte, error) {
		return metadata.ReadRegularFile(root, filepath.Join(dir, "feed.json"), feed.MaxReadBytes, feed.ErrInvalidFeed, feed.ErrInvalidFeed)
	})
	if err != nil {
		return nil, false
	}
	var f struct {
		Episodes []struct {
			Path string `json:"path"`
		} `json:"episodes"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return nil, false
	}
	for _, e := range f.Episodes {
		if folder, file, cut := strings.Cut(e.Path, "/"); cut && feed.EpisodePath(folder, e.Path) {
			files = append(files, file)
		}
	}
	return files, true
}

// podcastHeldID is the show identity folder show of root holds now: its
// feed.json's show_id, "" when the folder or its feed.json is gone or the
// name can be no show folder's. ok is false when that can't be told: a read
// error, a feed.json of a newer version, and one that is invalid, too large
// or no regular file, which may be the show's own feed.json half written or
// damaged for now (so a copy beside it is not taken for the show's one
// folder, and nothing merges into it).
func podcastHeldID(l podcastLooks, root, show string) (id string, ok bool) {
	if !feed.ShowPath(show) {
		return "", true
	}
	dir, err := l.join(root, show)
	if metadata.Unanswered(l.ctx, err) {
		return "", false // the join's looks given up on: not known
	}
	if err != nil {
		return "", true
	}
	at := filepath.Join(root, show)
	fi, err := lookAt(l, at, func() (os.FileInfo, error) { return os.Lstat(at) })
	if metadata.NotExist(err) {
		return "", true
	}
	if err != nil {
		return "", false
	}
	if !fi.IsDir() {
		return "", true
	}
	f, err := l.readFeed(root, filepath.Join(dir, "feed.json"), show, nil)
	switch {
	case metadata.NotExist(err):
		return "", true
	case err != nil:
		return "", false
	}
	return f.ShowID, true
}

// errFeedUnusable is a show folder's feed.json that is invalid, too large, no
// regular file or of a newer version: not indexable, but it may be a show's
// own, half written or damaged for now (unreadPodcastFolder).
var errFeedUnusable = fmt.Errorf("%w: unusable feed.json", ErrNotIndexable)

// unreadPodcastFolder reports whether err, indexPodcastShow's, is that the
// folder could not be read or its feed.json used for now: it may be where
// another show renamed by hand is now (movePodcastShows' unread).
func unreadPodcastFolder(err error) bool {
	return err != nil && (errors.Is(err, errFeedUnusable) || !errors.Is(err, ErrNotIndexable))
}

// errFolderClosed is a show folder the server could not look into: whether it
// holds a feed.json at all can't be told.
var errFolderClosed = errors.New("podcast folder could not be looked into")

// fileSystemFolders are the folders a file system or NAS keeps at a volume's
// or share's root for itself, which its server user is often refused: never a
// show folder a listener renamed by hand.
var fileSystemFolders = []string{"lost+found", "$RECYCLE.BIN", "System Volume Information", "#recycle", "#snapshot", "@eaDir"}

// strayPodcastFolder reports whether err, podcastFolderSig's for folder show,
// is a file system's own folder (fileSystemFolders: a root-owned lost+found)
// the server may not look into (a permission, which stays until an admin
// changes it), where the index and the podcast state know nothing
// (catalog.PodcastPathKnown). indexPodcastShow, but for a forced rescan, takes
// such an entry for no show folder (ErrNotIndexable), so it makes no scan
// partial (its prune would never run, and a deleted show's leftover follow
// would hold a rename into its name for ever) and is no unread folder. Any
// other name stays both, as a show renamed by hand into a folder the server
// can't read looks just the same: the scan keeps its old row and state until
// the folder reads again. So does any other fault (EIO, ESTALE, a timeout), one
// on a folder the index knows, or one past a feed.json seen there.
func (s *Scanner) strayPodcastFolder(ctx context.Context, lib catalog.Library, show string, err error) bool {
	if !errors.Is(err, errFolderClosed) || !errors.Is(err, fs.ErrPermission) ||
		!slices.ContainsFunc(fileSystemFolders, func(f string) bool { return strings.EqualFold(f, show) }) {
		return false
	}
	known, err := s.cat.PodcastPathKnown(ctx, lib.ID, show)
	return err == nil && !known
}

// podcastFolderSig is folder show of root's directory, its feed.json's
// details and the folder's signature (podcastShowSignature), which an index
// row of the folder records. err is ErrNotIndexable where no show folder or
// feed.json is, errFeedUnusable where the feed.json is no regular file, and
// wraps errFolderClosed where the folder can't be looked into (the folder
// itself not statable is the bare error: unread, never stray). Either is a
// look given up on (transientGaveUp) for a fault a retry may get past, as is
// one of l's looks that did not answer, or ctx's error.
func podcastFolderSig(l podcastLooks, root, show string) (dir string, info os.FileInfo, sig string, err error) {
	dir, err = l.join(root, show)
	if metadata.Unanswered(l.ctx, err) {
		return "", nil, "", cmp.Or(err, l.ctx.Err()) // the join's looks given up on: unread, never no show
	}
	if err != nil {
		return "", nil, "", ErrNotIndexable
	}
	// A top-level show must itself be a real directory, even if a symlink would
	// remain inside the root. The Bridge owns only this one directory level.
	// Only a folder that is gone is no show's: one the server can't stat (a
	// share that dropped, a root it may list but not search) may still be one.
	fi, err := l.lstat(filepath.Join(root, show))
	if metadata.NotExist(err) || err == nil && !fi.IsDir() {
		return "", nil, "", ErrNotIndexable
	}
	if err != nil {
		return "", nil, "", transientGaveUp(err) // a share's hiccup: a look given up on
	}
	info, err = l.lstat(filepath.Join(dir, "feed.json"))
	if metadata.NotExist(err) {
		return "", nil, "", ErrNotIndexable
	}
	if err != nil {
		// A fault a retry may get past is a look given up on, as the folder's.
		return "", nil, "", fmt.Errorf("%w: %w", errFolderClosed, transientGaveUp(err))
	}
	if !info.Mode().IsRegular() {
		return "", nil, "", errFeedUnusable
	}
	sig, err = podcastShowSignature(l, dir, info)
	return dir, info, sig, err
}

func (s *Scanner) indexPodcastShow(ctx context.Context, lib catalog.Library, show string, force bool) (int, bool, error) {
	l := s.podcastLooks(ctx)
	dir, info, sig, err := podcastFolderSig(l, lib.Root, show)
	if err != nil && !force && s.strayPodcastFolder(ctx, lib, show, err) {
		return 0, false, ErrNotIndexable
	}
	if err != nil {
		return 0, false, err
	}
	filename := filepath.Join(dir, "feed.json")
	feedSig := feedSignature(info)
	// prior: the folder is unchanged since its row was written, but what its
	// feed.json lists may not be: a re-rooted read (feed.File.Rerooted) depends
	// on the folder its paths name too, so its row records the episodes read
	// (rerootedSignature), and a row listing none may be a server's from before
	// the re-rooting. Either is read again, quietly, and indexed only when it
	// lists otherwise now and could tell (feed.File.Unsure; CONVENTIONS.md: a
	// show folder renamed by hand).
	prior, count, log, quiet := "", 0, s.log, false
	if !force {
		old, err := s.cat.PodcastShow(ctx, lib.ID, show, 0)
		if err != nil && !errors.Is(err, catalog.ErrNotFound) {
			return 0, false, err
		}
		if err == nil && (old.FeedSig == sig || strings.HasPrefix(old.FeedSig, sig+rerootedSep)) {
			// An unchanged feed.json is skipped only while its episodes are
			// still indexed: an older server run on this database (a rollback)
			// scans the library as books and removes them, leaving the show.
			rows := 0
			if old.EpisodeCount > 0 {
				if rows, err = s.cat.PodcastEpisodeRows(ctx, lib.ID, show); err != nil {
					return 0, false, err
				}
			}
			switch {
			case rows < old.EpisodeCount:
				s.log.Info("re-indexing a podcast show whose episodes are missing from the index", "library", lib.ID, "indexed", rows, "listed", old.EpisodeCount)
				force = true
			case old.FeedSig == sig && old.EpisodeCount > 0:
				return old.EpisodeCount, false, nil
			default:
				prior, count, log = old.FeedSig, old.EpisodeCount, nil
			}
		}
	}
	f, err := l.readFeed(lib.Root, filename, show, log)
	if errors.Is(err, feed.ErrInvalidFeed) || errors.Is(err, feed.ErrVersion) {
		s.log.Warn("skipping invalid or unsupported podcast feed.json", "library", lib.ID)
		return 0, false, errFeedUnusable
	}
	if err != nil {
		return 0, false, err
	}
	if f.Rerooted {
		sig = rerootedSignature(sig, f)
	}
	if f.Unsure {
		// A read that could not tell what the disk lists (feed.File.Unsure: a
		// share's fault on a folder its paths name) keeps an indexed row as it
		// is, however the read came (a quiet re-read, a folder changed since,
		// the Bridge's forced rescan): re-indexed, the episodes it left out
		// would drop from the listing until a read could tell, the row's new
		// signature would move its cover_version, and the next read, of the
		// row it wrote, would be a quiet one, telling none of the news this
		// feed.json brought. The next read that can tell indexes the change.
		// Only a show with no row yet is indexed without them (a first index
		// tells nothing anyway).
		if prior != "" {
			return count, false, nil
		}
		switch old, err := s.cat.PodcastShow(ctx, lib.ID, show, 0); {
		case err == nil:
			return old.EpisodeCount, false, nil
		case !errors.Is(err, catalog.ErrNotFound):
			return 0, false, err
		}
	}
	if prior != "" {
		if sig == prior && len(f.Episodes) == count {
			return count, false, nil
		}
		force = true // the folder's signature is the row's: index it all the same
		// Its feed.json is the one the row was read from, so nothing it lists
		// is the Bridge's news, however it lists more (a folder its paths name
		// changed, episodes a first index that could not tell left out):
		// telling none.
		quiet = true
	}
	entries, err := l.readDir(dir)
	if err != nil {
		return 0, false, err
	}
	present := map[string]catalog.PodcastFile{}
	cover, version := "", ""
	for _, e := range entries {
		// Hidden names are never episodes or artwork; the Bridge's staging
		// files among them may be renamed into place during this listing.
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := l.info(dir, e)
		if metadata.NotExist(err) {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		present[e.Name()] = catalog.PodcastFile{Size: info.Size(), MTime: info.ModTime().UnixNano(), FeedSig: feedSig}
		if cover == "" && (e.Name() == "cover.jpg" || e.Name() == "cover.png") {
			cover = path.Join(show, e.Name())
			version = feedSignature(info)
		}
	}
	for _, ep := range f.Episodes {
		base := path.Base(ep.Path)
		file, ok := present[base]
		if !ok {
			continue
		}
		if berr := podcastBarrier(l.lstat, dir, base); berr != nil {
			if metadata.GaveUp(berr) {
				return 0, false, berr // unknown, never the episode unpublished
			}
			delete(present, base)
			continue
		}
		file.Chapters, err = readAt(l, filepath.Join(dir, metadata.AttachmentsDir), func() (*metadata.ChapterList, error) {
			return podcastChapters(lib.Root, dir, base, ep.Fetch)
		})
		if metadata.Unanswered(ctx, err) {
			return 0, false, cmp.Or(err, ctx.Err()) // unknown, never the episode's chapters gone
		}
		if err != nil {
			s.log.Warn("unusable podcast chapter attachment", "library", lib.ID)
		}
		present[base] = file
	}
	changed, err := s.cat.IndexPodcastShow(ctx, lib.ID, show, sig, cover, version, f, present, force, quiet)
	return len(f.Episodes), changed, err
}

// rerootedSep joins a show folder's signature (hex and digits, so never "+")
// and the hash of the episode paths a re-rooted read of its feed.json listed:
// the row's signature then changes when another folder changes what the read
// lists (indexPodcastShow).
const rerootedSep = "+"

func rerootedSignature(sig string, f *feed.File) string {
	h := sha256.New()
	for _, ep := range f.Episodes {
		_, _ = io.WriteString(h, ep.Path+"\n") // no episode path holds a line break (feed.ShowPath)
	}
	return sig + rerootedSep + hex.EncodeToString(h.Sum(nil))
}

// BrowsePodcasts reads only the index, so episodes without files remain visible
// and feed.json and artwork never become player entries.
func (s *Scanner) BrowsePodcasts(ctx context.Context, lib catalog.Library, scope catalog.Scope, user int64, rel string, offset, limit int) (*Listing, error) {
	rel = catalog.CleanRelPath(rel)
	out := &Listing{Path: rel, Entries: []Entry{}, Offset: max(offset, 0)}
	all := []Entry{}
	if rel == "" {
		shows, err := s.cat.PodcastShows(ctx, scope, user, false)
		if err != nil {
			return nil, err
		}
		for _, show := range shows {
			if scope.VisibleInBrowse(show.Path) {
				all = append(all, Entry{Name: show.Title, Path: show.Path, IsDir: true, Title: show.Title, Author: show.Author})
			}
		}
	} else {
		if !feed.ShowPath(rel) || !scope.VisibleInBrowse(rel) {
			return nil, ErrNotIndexable
		}
		show, err := s.cat.PodcastShow(ctx, lib.ID, rel, user)
		if err != nil {
			return nil, err
		}
		order := "newest"
		if show.Type == "serial" {
			order = "oldest"
		}
		cursor := ""
		for {
			page, err := s.cat.PodcastEpisodesWithin(ctx, lib.ID, rel, order, 200, cursor, scope)
			if err != nil {
				return nil, err
			}
			for _, e := range page.Episodes {
				all = append(all, Entry{Name: e.Title, Path: e.Path, IsAudio: true, IsBook: true, Title: e.Title, Author: show.Author, Duration: e.Duration, Size: e.Size})
			}
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
	}
	out.Total = len(all)
	limit = min(max(limit, 1), 2000)
	start := min(out.Offset, len(all))
	end := min(start+limit, len(all))
	out.Entries = all[start:end]
	if end < len(all) {
		out.NextOffset = end
	}
	return out, nil
}

// ReadLibraryMarker reads only a small regular file beneath a real .audiosilo
// directory, looked at and read through the library's root as every library
// file is (metadata.LstatBelow, ReadRegularFile). A link, there or swapped in
// for the folder since its look, cannot make the server attest another
// library's marker.
func ReadLibraryMarker(root string, id int64) (string, error) {
	dir := filepath.Join(root, metadata.AttachmentsDir)
	st, err := metadata.LstatBelow(root, dir)
	if err != nil || !st.IsDir() {
		return "", ErrNotIndexable
	}
	raw, err := metadata.ReadRegularFile(root, filepath.Join(dir, "library.json"), 4096, ErrNotIndexable, ErrNotIndexable)
	if err != nil {
		return "", ErrNotIndexable
	}
	var marker struct {
		Version   int    `json:"version"`
		LibraryID int64  `json:"library_id"`
		Nonce     string `json:"nonce"`
	}
	if json.Unmarshal(raw, &marker) != nil || marker.Version != 1 || marker.LibraryID != id || !feed.ValidHex(marker.Nonce, 32) {
		return "", ErrNotIndexable
	}
	return marker.Nonce, nil
}

// podcastShowSignature notices content and attachment changes even if feed.json
// kept its size and mtime. Hidden staging files are not content.
func podcastShowSignature(l podcastLooks, dir string, info os.FileInfo) (string, error) {
	h := sha256.New()
	_, _ = fmt.Fprint(h, feedSignature(info))
	for _, sub := range []string{"", metadata.AttachmentsDir} {
		at := filepath.Join(dir, sub)
		if sub != "" {
			st, e := lookAt(l, at, func() (os.FileInfo, error) { return os.Lstat(at) })
			if metadata.NotExist(e) {
				continue
			}
			if e != nil {
				return "", e
			}
			if !st.IsDir() {
				_, _ = fmt.Fprint(h, "invalid attachments")
				continue
			}
		}
		entries, err := l.readDir(at)
		if metadata.NotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if sub == "" && e.Name() != ".audiosilo" && strings.HasPrefix(e.Name(), ".") {
				continue
			}
			st, err := l.info(at, e)
			if metadata.NotExist(err) {
				continue // renamed away since the listing (a Bridge temp or trash move)
			}
			if err != nil {
				return "", err
			}
			_, _ = fmt.Fprintf(h, "%s/%s:%s:%s;", sub, e.Name(), st.Mode(), feedSignature(st))
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// podcastBarrier is nil when an episode is published: no publish marker beside
// it in the show's .audiosilo folder. A look that fails as a share does that a
// retry may get past is a look given up on, one refused for good
// ErrShowRefused (podcastShowLookErr: a new download would not mend it); any
// other answer but no marker is ErrNotIndexable. lstat is its look:
// podcastLstat inside OpenPodcast's one bounded call, a scan's bounded one
// (podcastLooks.lstat).
func podcastBarrier(lstat func(string) (os.FileInfo, error), dir, base string) error {
	side := filepath.Join(dir, metadata.AttachmentsDir)
	st, err := lstat(side)
	switch {
	case metadata.NotExist(err):
		return nil
	case err != nil:
		return podcastShowLookErr(err)
	case !st.IsDir():
		return ErrNotIndexable
	}
	_, err = lstat(filepath.Join(side, stemOf(base)+".publish.json"))
	switch {
	case metadata.NotExist(err):
		return nil
	case err != nil:
		return podcastShowLookErr(err)
	}
	return ErrNotIndexable
}

// podcastChapters is an episode's chapter attachment, nil when it has none:
// an error for one that can't be used (ErrNotIndexable), or a look given up
// on when its read failed as a share does that a retry may get past. It is
// read through the library's root (metadata.ReadChapterList's), dir the
// show's folder under it, so a link swapped in for that folder or for its
// .audiosilo folder is never followed out of the library.
func podcastChapters(root, dir, base string, f *feed.Fetch) (*metadata.ChapterList, error) {
	if !feed.ValidFetch(f) || f.DeletedAt != "" {
		return nil, nil
	}
	l, err := metadata.ReadChapterList(root, filepath.Join(dir, metadata.AttachmentsDir, stemOf(base)+"."+metadata.AttachmentChapters))
	if metadata.TransientFault(err) {
		return nil, metadata.AsGaveUp(err) // a share's hiccup: unknown, never no chapters
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || l.From != "id3_chap" || len(l.Files) != 1 || l.Files[0].Path != base || l.Files[0].Size != f.Bytes || l.Files[0].DurationMS != f.DurationMS || l.GeneratedAt != f.FetchedAt {
		return nil, ErrNotIndexable
	}
	for _, ch := range l.Chapters {
		if ch.StartMS > f.DurationMS || ch.LengthMS > f.DurationMS-ch.StartMS {
			return nil, ErrNotIndexable
		}
	}
	return l, nil
}

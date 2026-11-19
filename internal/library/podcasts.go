package library

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

func feedSignature(info os.FileInfo) string {
	return strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

func (s *Scanner) scanPodcasts(ctx context.Context, lib catalog.Library, res *ScanResult) (*ScanResult, error) {
	// The shows known before the root is listed are the only ones this scan may
	// prune: one the Bridge indexes meanwhile isn't in the listing yet.
	old, err := s.cat.PodcastSignatures(ctx, lib.ID)
	if err != nil {
		return res, err
	}
	entries, err := os.ReadDir(lib.Root)
	if err != nil {
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
	n, _, err := s.indexPodcastShow(ctx, lib, show, true)
	if err == nil {
		s.movePodcastShows(ctx, lib, show, false)
	}
	return show, n, err
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
// (indexNewPodcastFolders). It goes to catalog.MovePodcastShows, which then
// holds a move over another show's state. It answers the old folders of the
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
		// unread instead: indexNewPodcastFolders).
		var n int
		var skipped bool
		if n, skipped, err = s.indexNewPodcastFolders(ctx, lib, &listed); err == nil && n > 0 {
			moves, err = s.settledPodcastMoves(ctx, lib, at, &listed)
		}
		unread = unread || skipped
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
// (unreadPodcastFolder; a stray folder is not: strayPodcastFolder) or holds what
// its row no longer describes (a scan would index it again: another show
// renamed into it by hand, the row's own gone). The rescan leaves such a row
// as it is, so the show now in that folder is found by the next scan, and a
// move over another show's state waits until then.
func (s *Scanner) indexNewPodcastFolders(ctx context.Context, lib catalog.Library, listed *map[string]bool) (n int, unread bool, err error) {
	known, err := s.cat.PodcastSignatures(ctx, lib.ID)
	if err != nil {
		return 0, false, err
	}
	if *listed == nil {
		if *listed, err = podcastFolderNames(lib.Root); err != nil {
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
		if sig, ok := known[name]; ok {
			if !unread {
				_, _, now, err := podcastFolderSig(lib.Root, name)
				unread = err == nil && now != sig || unreadPodcastFolder(err)
			}
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

// planPodcastMoves is movePodcastShows' look at the disk. With reindex, a
// path whose row names the moving show while its folder holds another is
// indexed again, and again reports that the index changed under the plan.
func (s *Scanner) planPodcastMoves(ctx context.Context, lib catalog.Library, at string, listed *map[string]bool, reindex bool) (moves []catalog.PodcastMove, again bool, err error) {
	found, err := s.cat.PodcastIdentityPaths(ctx, lib.ID, at)
	if err != nil {
		return nil, false, err
	}
identities:
	for id, paths := range found {
		occupied := map[string]bool{} // the path holds a show, this one or another
		var holders []catalog.PodcastIdentityPath
		for _, p := range paths {
			now, ok := podcastHeldID(lib.Root, p.Path)
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
				if *listed, err = podcastFolderNames(lib.Root); err != nil {
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
		files, ok := podcastFeedFiles(lib.Root, holders[0].Path)
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
func podcastFolderNames(root string) (map[string]bool, error) {
	entries, err := os.ReadDir(root)
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
func podcastFeedFiles(root, show string) (files []string, ok bool) {
	dir, err := ContentJoin(root, show)
	if err != nil {
		return nil, false
	}
	raw, err := metadata.ReadRegularFile(filepath.Join(dir, "feed.json"), feed.MaxReadBytes, feed.ErrInvalidFeed, feed.ErrInvalidFeed)
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
func podcastHeldID(root, show string) (id string, ok bool) {
	dir, err := ContentJoin(root, show)
	if err != nil || !feed.ShowPath(show) {
		return "", true
	}
	fi, err := os.Lstat(filepath.Join(root, show))
	if errors.Is(err, os.ErrNotExist) {
		return "", true
	}
	if err != nil {
		return "", false
	}
	if !fi.IsDir() {
		return "", true
	}
	f, err := feed.ReadFile(filepath.Join(dir, "feed.json"), show, nil)
	switch {
	case errors.Is(err, os.ErrNotExist):
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

// strayPodcastFolder reports whether err, podcastFolderSig's for folder show,
// is a root entry the server may not look into (a permission, which stays
// until an admin changes it), where the index and the podcast state know
// nothing (catalog.PodcastPathKnown): a root-owned lost+found, say.
// indexPodcastShow, but for a forced rescan, takes such an entry for no show
// folder (ErrNotIndexable), so it makes no scan partial (its prune would never
// run, and a deleted show's leftover follow would hold a rename into its name
// for ever) and is no unread folder. Any other fault (EIO, ESTALE, a timeout),
// one on a folder the index knows, or one past a feed.json seen there (a
// show's, unreadable) stays both.
func (s *Scanner) strayPodcastFolder(ctx context.Context, lib catalog.Library, show string, err error) bool {
	if !errors.Is(err, errFolderClosed) || !errors.Is(err, fs.ErrPermission) {
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
// itself not statable is the bare error: unread, never stray).
func podcastFolderSig(root, show string) (dir string, info os.FileInfo, sig string, err error) {
	dir, err = ContentJoin(root, show)
	if err != nil {
		return "", nil, "", ErrNotIndexable
	}
	// A top-level show must itself be a real directory, even if a symlink would
	// remain inside the root. The Bridge owns only this one directory level.
	// Only a folder that is gone is no show's: one the server can't stat (a
	// share that dropped, a root it may list but not search) may still be one.
	fi, err := os.Lstat(filepath.Join(root, show))
	if errors.Is(err, os.ErrNotExist) || err == nil && !fi.IsDir() {
		return "", nil, "", ErrNotIndexable
	}
	if err != nil {
		return "", nil, "", err
	}
	info, err = os.Lstat(filepath.Join(dir, "feed.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, "", ErrNotIndexable
	}
	if err != nil {
		return "", nil, "", fmt.Errorf("%w: %w", errFolderClosed, err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, "", errFeedUnusable
	}
	sig, err = podcastShowSignature(dir, info)
	return dir, info, sig, err
}

func (s *Scanner) indexPodcastShow(ctx context.Context, lib catalog.Library, show string, force bool) (int, bool, error) {
	dir, info, sig, err := podcastFolderSig(lib.Root, show)
	if err != nil && !force && s.strayPodcastFolder(ctx, lib, show, err) {
		return 0, false, ErrNotIndexable
	}
	if err != nil {
		return 0, false, err
	}
	filename := filepath.Join(dir, "feed.json")
	feedSig := feedSignature(info)
	if !force {
		old, err := s.cat.PodcastShow(ctx, lib.ID, show, 0)
		if err != nil && !errors.Is(err, catalog.ErrNotFound) {
			return 0, false, err
		}
		if err == nil && old.FeedSig == sig {
			// An unchanged feed.json is skipped only while its episodes are
			// still indexed: an older server run on this database (a rollback)
			// scans the library as books and removes them, leaving the show.
			rows, err := s.cat.PodcastEpisodeRows(ctx, lib.ID, show)
			if err != nil {
				return 0, false, err
			}
			if rows >= old.EpisodeCount {
				return old.EpisodeCount, false, nil
			}
			s.log.Info("re-indexing a podcast show whose episodes are missing from the index", "library", lib.ID, "indexed", rows, "listed", old.EpisodeCount)
			force = true
		}
	}
	f, err := feed.ReadFile(filename, show, s.log)
	if errors.Is(err, feed.ErrInvalidFeed) || errors.Is(err, feed.ErrVersion) {
		s.log.Warn("skipping invalid or unsupported podcast feed.json", "library", lib.ID)
		return 0, false, errFeedUnusable
	}
	if err != nil {
		return 0, false, err
	}
	entries, err := os.ReadDir(dir)
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
		info, err := e.Info()
		if errors.Is(err, os.ErrNotExist) {
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
		if podcastBarrier(dir, base) != nil {
			delete(present, base)
			continue
		}
		file.Chapters, err = podcastChapters(dir, base, ep.Fetch)
		if err != nil {
			s.log.Warn("unusable podcast chapter attachment", "library", lib.ID)
		}
		present[base] = file
	}
	changed, err := s.cat.IndexPodcastShow(ctx, lib.ID, show, sig, cover, version, f, present, force)
	return len(f.Episodes), changed, err
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
// directory. A symlink cannot make the server attest another library's marker.
func ReadLibraryMarker(root string, id int64) (string, error) {
	dir := filepath.Join(root, metadata.AttachmentsDir)
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() {
		return "", ErrNotIndexable
	}
	raw, err := metadata.ReadRegularFile(filepath.Join(dir, "library.json"), 4096, ErrNotIndexable, ErrNotIndexable)
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
func podcastShowSignature(dir string, info os.FileInfo) (string, error) {
	h := sha256.New()
	_, _ = fmt.Fprint(h, feedSignature(info))
	for _, sub := range []string{"", metadata.AttachmentsDir} {
		if sub != "" {
			st, e := os.Lstat(filepath.Join(dir, sub))
			if errors.Is(e, os.ErrNotExist) {
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
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if sub == "" && e.Name() != ".audiosilo" && strings.HasPrefix(e.Name(), ".") {
				continue
			}
			st, err := e.Info()
			if errors.Is(err, os.ErrNotExist) {
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
func feedSignatureMust(filename string) string {
	st, err := os.Lstat(filename)
	if err != nil {
		return ""
	}
	return feedSignature(st)
}
func podcastBarrier(dir, base string) error {
	side := filepath.Join(dir, metadata.AttachmentsDir)
	st, err := os.Lstat(side)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !st.IsDir() {
		return ErrNotIndexable
	}
	_, err = os.Lstat(filepath.Join(side, stemOf(base)+".publish.json"))
	if !errors.Is(err, os.ErrNotExist) {
		return ErrNotIndexable
	}
	return nil
}
func podcastChapters(dir, base string, f *feed.Fetch) (*metadata.ChapterList, error) {
	if !feed.ValidFetch(f) || f.DeletedAt != "" {
		return nil, nil
	}
	l, err := metadata.ReadChapterList(filepath.Join(dir, metadata.AttachmentsDir, stemOf(base)+"."+metadata.AttachmentChapters))
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

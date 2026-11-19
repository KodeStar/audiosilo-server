package library

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	partial := false
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !e.IsDir() || !feed.ShowPath(e.Name()) {
			continue
		}
		n, changed, err := s.indexPodcastShow(ctx, lib, e.Name(), false)
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
	// state along. A partial scan moves nothing, as it prunes nothing: the
	// folder it could not read may be where another show renamed by hand is
	// now, and a move to that show's old name would replace its follows.
	if !partial {
		s.movePodcastShows(ctx, lib, "")
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
		s.movePodcastShows(ctx, lib, show)
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
// call. Nothing moves either while a folder that may hold another show can't
// be read (a partial scan; a rescan's indexNewPodcastFolders). Failures are
// logged: the next scan or rescan looks again.
func (s *Scanner) movePodcastShows(ctx context.Context, lib catalog.Library, at string) {
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
		// one call.
		var n int
		if n, err = s.indexNewPodcastFolders(ctx, lib, &listed); err == nil && n > 0 {
			moves, err = s.settledPodcastMoves(ctx, lib, at, &listed)
		}
	}
	if err != nil {
		s.log.Warn("podcasts: looking for renamed show folders failed", "library", lib.ID, "err", err)
		return
	}
	if len(moves) == 0 {
		return
	}
	n, err := s.cat.MovePodcastShows(ctx, lib.ID, moves)
	if err != nil {
		s.log.Warn("podcasts: moving a renamed show folder's state failed", "library", lib.ID, "err", err)
		return
	}
	if n > 0 {
		s.log.Info("podcasts: moved the state of show folders renamed by hand", "library", lib.ID, "shows", n)
	}
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
// index row yet, and answers how many it indexed. One that can't be read is an
// error (no move on this look): it may be the folder the other show is in.
func (s *Scanner) indexNewPodcastFolders(ctx context.Context, lib catalog.Library, listed *map[string]bool) (int, error) {
	known, err := s.cat.PodcastSignatures(ctx, lib.ID)
	if err != nil {
		return 0, err
	}
	if *listed == nil {
		if *listed, err = podcastFolderNames(lib.Root); err != nil {
			return 0, err
		}
	}
	n := 0
	for name := range *listed {
		if _, ok := known[name]; ok || !feed.ShowPath(name) {
			continue
		}
		switch _, _, err := s.indexPodcastShow(ctx, lib, name, false); {
		case err == nil:
			n++
		case !errors.Is(err, ErrNotIndexable):
			return n, err
		}
	}
	return n, nil
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

func (s *Scanner) indexPodcastShow(ctx context.Context, lib catalog.Library, show string, force bool) (int, bool, error) {
	dir, err := ContentJoin(lib.Root, show)
	if err != nil {
		return 0, false, ErrNotIndexable
	}
	// A top-level show must itself be a real directory, even if a symlink would
	// remain inside the root. The Bridge owns only this one directory level.
	fi, err := os.Lstat(filepath.Join(lib.Root, show))
	if err != nil || !fi.IsDir() {
		return 0, false, ErrNotIndexable
	}
	filename := filepath.Join(dir, "feed.json")
	info, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, ErrNotIndexable
	}
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, false, ErrNotIndexable
	}
	feedSig := feedSignature(info)
	sig, err := podcastShowSignature(dir, info)
	if err != nil {
		return 0, false, err
	}
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
		return 0, false, ErrNotIndexable
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

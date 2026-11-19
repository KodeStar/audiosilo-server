package library

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

const renameShowID = "a1b2c3d4e5f60718" // the fixture feed's show_id

// renameBed is a podcast library holding the fixture show at "Example Show",
// scanned, followed, with an archive policy and a place in an episode.
type renameBed struct {
	ctx  context.Context
	cat  *catalog.Catalog
	s    *Scanner
	lib  *catalog.Library
	root string
	user int64
	feed []byte
}

const renameEpisode = "2026-10-19-03fd13fa08.mp3" // the fixture's first episode file

// renameFetched is the fixture's one downloaded episode's file.
const renameFetched = "2026-10-12-2fa6ddd63e.m4a"

// writeFetched writes name as the fixture's downloaded episode's file: its
// fetch's size (a hole: no audio is read).
func writeFetched(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(name, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(name, 24000000); err != nil {
		t.Fatal(err)
	}
}

func newRenameBed(t *testing.T) renameBed {
	t.Helper()
	db := storetest.Open(t)
	b := renameBed{ctx: context.Background(), cat: catalog.New(db, time.Now), root: t.TempDir(), user: namedUser(t, db, "ann")}
	raw, err := os.ReadFile("../podcast/testdata/feed-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	b.feed = raw
	b.writeShow(t, "Example Show", "Example Show")
	if b.lib, err = b.cat.CreateLibrary(b.ctx, catalog.Library{Name: "Podcasts", Root: b.root, Kind: catalog.KindPodcasts}); err != nil {
		t.Fatal(err)
	}
	b.s = NewScanner(b.cat, "a-probe-that-must-never-run", slog.Default())
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.FollowPodcast(b.ctx, b.user, b.lib.ID, "Example Show", renameShowID); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.SetPodcastPolicy(b.ctx, b.lib.ID, "Example Show", renameShowID, &catalog.Policy{Mode: "archive", Keep: 3, Days: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.cat.SaveProgress(b.ctx, b.user, catalog.Progress{Ref: catalog.Ref{LibraryID: b.lib.ID, Path: "Example Show/" + renameEpisode}, Position: 42, Duration: 1720, UpdatedAt: "2026-11-01T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	return b
}

// writeShow writes the fixture feed.json into folder, its episode paths
// naming named (the folder itself once the Bridge has taken it in).
func (b renameBed) writeShow(t *testing.T, folder, named string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(b.root, folder), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := bytes.ReplaceAll(b.feed, []byte(`"Example Show/`), []byte(`"`+named+`/`))
	if err := os.WriteFile(filepath.Join(b.root, folder, "feed.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// rename renames the show folder by hand; taken reports whether the Bridge has
// taken it in yet (its feed.json naming the new folder).
func (b renameBed) rename(t *testing.T, to string, taken bool) {
	t.Helper()
	if err := os.Rename(filepath.Join(b.root, "Example Show"), filepath.Join(b.root, to)); err != nil {
		t.Fatal(err)
	}
	if taken {
		b.writeShow(t, to, to)
	}
}

func (b renameBed) shows(t *testing.T) []string {
	t.Helper()
	all, err := b.cat.PodcastShows(b.ctx, catalog.Scope{LibraryID: b.lib.ID, AllowAll: true}, b.user, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range all {
		out = append(out, s.Path)
	}
	return out
}

func (b renameBed) checkFollowed(t *testing.T, at string) {
	t.Helper()
	got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, at, b.user)
	if err != nil || !got.Following || got.Retention.Mode != "archive" {
		t.Fatalf("%s: %+v %v", at, got, err)
	}
	p, err := b.cat.GetProgress(b.ctx, b.user, catalog.Ref{LibraryID: b.lib.ID, Path: at + "/" + renameEpisode})
	if err != nil || p == nil || p.Position != 42 {
		t.Fatalf("%s: episode progress %+v %v", at, p, err)
	}
}

// The Bridge's rescan of the folder it took in (a poll's or a fetch's) moves
// the show there at once: one row, followed, its policy kept, before any scan.
func TestPodcastRescanMovesARenamedShow(t *testing.T) {
	b := newRenameBed(t)
	b.rename(t, "Renamed", true)
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if got := b.shows(t); len(got) != 1 || got[0] != "Renamed" {
		t.Fatalf("shows %v", got)
	}
	b.checkFollowed(t, "Renamed")
	due, err := b.cat.DuePodcastPolls(b.ctx, 10)
	if err != nil || len(due) != 1 || due[0].Path != "Renamed" {
		t.Fatalf("due %+v %v", due, err)
	}
}

// A rename in case only moves the show too, also where the file system still
// opens the old spelling (case-insensitive: macOS, Windows shares), as only
// the name the root lists is the folder.
func TestPodcastRescanMovesACaseOnlyRename(t *testing.T) {
	b := newRenameBed(t)
	b.rename(t, "example show", true)
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "example show"); err != nil {
		t.Fatal(err)
	}
	if got := b.shows(t); len(got) != 1 || got[0] != "example show" {
		t.Fatalf("shows %v", got)
	}
	b.checkFollowed(t, "example show")
}

// A scan finding a folder renamed by hand before the Bridge took it in (its
// feed.json still naming the old folder) moves the show too, its episodes'
// listening state included.
func TestPodcastScanMovesAFolderRenamedByHand(t *testing.T) {
	b := newRenameBed(t)
	b.rename(t, "Renamed", false)
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	if got := b.shows(t); len(got) != 1 || got[0] != "Renamed" {
		t.Fatalf("shows %v", got)
	}
	b.checkFollowed(t, "Renamed")
	// The Bridge takes it in: its episodes come back where the state went.
	b.writeShow(t, "Renamed", "Renamed")
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.cat.GetBookByPath(b.ctx, b.lib.ID, "Renamed/"+renameEpisode); err != nil {
		t.Fatal(err)
	}
}

// A scan's move lists the renamed folder's downloaded episodes at once, before
// the Bridge takes it in (its feed.json still naming the old folder), so the
// moved place opens; an episode not downloaded (a file there, but no fetch in
// feed.json) or with no file there waits for the Bridge.
func TestPodcastScanListsARenamedFoldersEpisodes(t *testing.T) {
	b := newRenameBed(t)
	if err := os.WriteFile(filepath.Join(b.root, "Example Show", renameEpisode), []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFetched(t, filepath.Join(b.root, "Example Show", renameFetched))
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	if _, err := b.cat.SaveProgress(b.ctx, b.user, catalog.Progress{Ref: catalog.Ref{LibraryID: b.lib.ID, Path: "Example Show/" + renameFetched}, Position: 99, Duration: 1512, UpdatedAt: "2026-11-01T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	b.rename(t, "Renamed", false)
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Renamed")
	if _, err := b.cat.GetBookByPath(b.ctx, b.lib.ID, "Renamed/"+renameFetched); err != nil {
		t.Fatalf("moved episode: %v", err)
	}
	if p, err := b.cat.GetProgress(b.ctx, b.user, catalog.Ref{LibraryID: b.lib.ID, Path: "Renamed/" + renameFetched}); err != nil || p == nil || p.Position != 99 {
		t.Fatalf("moved episode's place: %+v %v", p, err)
	}
	if _, err := b.cat.GetBookByPath(b.ctx, b.lib.ID, "Renamed/"+renameEpisode); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("an episode not downloaded listed: %v", err)
	}
	if n, err := b.cat.PodcastEpisodeRows(b.ctx, b.lib.ID, "Renamed"); err != nil || n != 1 {
		t.Fatalf("episodes %d %v", n, err)
	}
}

// A move by hand done as a copy and a delete, a scan in between: the copy
// lists nothing beside its original (its feed.json names that folder, which
// holds the show), and once the original is gone the next scan lists its
// downloaded episode and moves the show there, though the copy's own folder
// has not changed since its row was written.
func TestPodcastScanListsACopyOnceItsOriginalIsGone(t *testing.T) {
	b := newRenameBed(t)
	writeFetched(t, filepath.Join(b.root, "Example Show", renameFetched))
	b.writeShow(t, "Moved", "Example Show")
	writeFetched(t, filepath.Join(b.root, "Moved", renameFetched))
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	if n, err := b.cat.PodcastEpisodeRows(b.ctx, b.lib.ID, "Moved"); err != nil || n != 0 {
		t.Fatalf("a copy beside its original lists %d %v", n, err)
	}
	b.checkFollowed(t, "Example Show")
	if err := os.RemoveAll(filepath.Join(b.root, "Example Show")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Moved")
	if _, err := b.cat.GetBookByPath(b.ctx, b.lib.ID, "Moved/"+renameFetched); err != nil {
		t.Fatalf("the moved episode: %v", err)
	}
	if got := b.shows(t); len(got) != 1 || got[0] != "Moved" {
		t.Fatalf("shows %v", got)
	}
}

// State an older server's scan left at a pruned folder's path (the show indexed
// only at its new name) is moved by the next scan.
func TestPodcastScanMovesStateLeftAtAPrunedFolder(t *testing.T) {
	b := newRenameBed(t)
	b.rename(t, "Renamed", true)
	if err := b.cat.PrunePodcastShows(b.ctx, b.lib.ID, []string{"Example Show"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Renamed")
}

// Denied: a copy (both folders holding the show) moves and merges nothing, on
// a scan or on the copy's rescan.
func TestPodcastCopyMovesNothing(t *testing.T) {
	b := newRenameBed(t)
	b.writeShow(t, "Copy", "Copy")
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Copy"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	if got := b.shows(t); len(got) != 2 {
		t.Fatalf("shows %v", got)
	}
	b.checkFollowed(t, "Example Show")
	got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Copy", b.user)
	if err != nil || got.Following || got.Retention.Mode != "keep" {
		t.Fatalf("copy %+v %v", got, err)
	}
	// Removing the original makes the copy the show's one folder: it moves then.
	if err := os.RemoveAll(filepath.Join(b.root, "Example Show")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Copy")
}

// Denied: a folder that can't be told apart (its feed.json unreadable for now)
// keeps its state where it is.
func TestPodcastRenameWaitsOnAnUnreadableFolder(t *testing.T) {
	b := newRenameBed(t)
	b.writeShow(t, "Renamed", "Renamed")
	// The old folder's feed.json of a newer version: it may still hold the show.
	if err := os.WriteFile(filepath.Join(b.root, "Example Show", "feed.json"), []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Example Show", b.user); err != nil {
		t.Fatalf("the old row went: %v", err)
	}
	got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Renamed", b.user)
	if err != nil || got.Following {
		t.Fatalf("moved anyway: %+v %v", got, err)
	}
}

// Denied: the show's own folder with a feed.json that is invalid for now (half
// written, damaged, too large) beside a copy. The copy is not taken for the
// show's one folder: nothing moves or merges into it, on its rescan or on a
// scan, and the state is where it was once the feed.json reads again.
func TestPodcastCopyBesideAnInvalidFeedMovesNothing(t *testing.T) {
	b := newRenameBed(t)
	b.writeShow(t, "Copy", "Copy")
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Copy"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.root, "Example Show", "feed.json"), b.feed[:len(b.feed)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	unfollowed := func() {
		t.Helper()
		got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Copy", b.user)
		if err != nil || got.Following || got.Retention.Mode != "keep" {
			t.Fatalf("the copy took the show's state: %+v %v", got, err)
		}
	}
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Copy"); err != nil {
		t.Fatal(err)
	}
	unfollowed()
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	unfollowed()
	b.writeShow(t, "Example Show", "Example Show")
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	unfollowed()
	b.checkFollowed(t, "Example Show")
}

// Two folder names swapped by hand: the Bridge's rescan of one moves both
// shows, the listener following each keeping both.
func TestPodcastRescanSwapsTwoFolders(t *testing.T) {
	b := newRenameBed(t)
	named := b.addOther(t)
	// Swap the names, then the Bridge takes in both folders.
	b.renameEach(t, [2]string{"Example Show", "tmp"}, [2]string{"Other", "Example Show"}, [2]string{"tmp", "Other"})
	b.writeShow(t, "Other", "Other")
	if err := os.WriteFile(filepath.Join(b.root, "Example Show", "feed.json"), named("Example Show"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Other"); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Other")
	b.checkOther(t, "Example Show")
	if got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Example Show", b.user); err != nil || got.Retention.Mode != "keep" {
		t.Fatalf("the other show took this one's policy: %+v %v", got, err)
	}
	if p, err := b.cat.GetProgress(b.ctx, b.user, catalog.Ref{LibraryID: b.lib.ID, Path: "Example Show/" + renameEpisode}); err != nil || p != nil {
		t.Fatalf("a place left at the other show's folder: %+v %v", p, err)
	}
}

const otherShowID = "0123456789abcdef"

// otherEpisode is the other show's first episode file (otherFeed's names).
const otherEpisode = "2025-10-19-03fd13fa08.mp3"

// addOther adds a second show at "Other" (its own id and episode files),
// scanned and followed by the bed's listener, with a place in its first
// episode, and answers its feed.json as it names folder.
func (b renameBed) addOther(t *testing.T) func(folder string) []byte {
	t.Helper()
	other := bytes.ReplaceAll(b.feed, []byte(renameShowID), []byte(otherShowID))
	// Its own episode files (their names carry the episode's id).
	other = bytes.ReplaceAll(other, []byte(`/2026-`), []byte(`/2025-`))
	other = bytes.ReplaceAll(other, []byte(`/undated-`), []byte(`/undated-other-`))
	named := func(folder string) []byte {
		return bytes.ReplaceAll(other, []byte(`"Example Show/`), []byte(`"`+folder+`/`))
	}
	if err := os.MkdirAll(filepath.Join(b.root, "Other"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.root, "Other", "feed.json"), named("Other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.FollowPodcast(b.ctx, b.user, b.lib.ID, "Other", otherShowID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.cat.SaveProgress(b.ctx, b.user, catalog.Progress{Ref: catalog.Ref{LibraryID: b.lib.ID, Path: "Other/" + otherEpisode}, Position: 77, Duration: 1720, UpdatedAt: "2026-11-01T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	return named
}

// checkOther checks the other show is followed at at, with its episode place.
func (b renameBed) checkOther(t *testing.T, at string) {
	t.Helper()
	got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, at, b.user)
	if err != nil || !got.Following || got.ShowID != otherShowID {
		t.Fatalf("the other show at %s: %+v %v", at, got, err)
	}
	p, err := b.cat.GetProgress(b.ctx, b.user, catalog.Ref{LibraryID: b.lib.ID, Path: at + "/" + otherEpisode})
	if err != nil || p == nil || p.Position != 77 {
		t.Fatalf("the other show's episode progress at %s: %+v %v", at, p, err)
	}
}

// addThird indexes a third show (its own id, no follow) at "Third".
func (b renameBed) addThird(t *testing.T) {
	t.Helper()
	third := bytes.ReplaceAll(b.feed, []byte(renameShowID), []byte("fedcba9876543210"))
	if err := os.MkdirAll(filepath.Join(b.root, "Third"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.root, "Third", "feed.json"), third, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Third"); err != nil {
		t.Fatal(err)
	}
}

// chain renames by hand the other show (addOther) to "Third", then this one to
// the other's old name, the Bridge having taken this one in, and answers the
// other's feed.json as it names folder (the caller writes Third's).
func (b renameBed) chain(t *testing.T) func(folder string) []byte {
	t.Helper()
	named := b.addOther(t)
	b.renameEach(t, [2]string{"Other", "Third"}, [2]string{"Example Show", "Other"})
	b.writeShow(t, "Other", "Other")
	return named
}

func (b renameBed) renameEach(t *testing.T, moves ...[2]string) {
	t.Helper()
	for _, mv := range moves {
		if err := os.Rename(filepath.Join(b.root, mv[0]), filepath.Join(b.root, mv[1])); err != nil {
			t.Fatal(err)
		}
	}
}

// Two folder names swapped by hand before the Bridge took either in (each
// feed.json still naming the other folder, so no index row lists either
// show's episodes once the scan indexed both again): a scan moves both shows
// with their episodes' places, read off each new folder's feed.json.
func TestPodcastScanSwapsTwoFoldersNotTakenIn(t *testing.T) {
	b := newRenameBed(t)
	b.addOther(t)
	b.renameEach(t, [2]string{"Example Show", "tmp"}, [2]string{"Other", "Example Show"}, [2]string{"tmp", "Other"})
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Other")
	b.checkOther(t, "Example Show")
}

// The same swap with each show's downloaded episode on disk: each new folder
// lists its own show's episode at once (read from a feed.json naming the
// other folder, which holds the other show), and each episode's place moves
// with its show, neither crossing to the other's.
func TestPodcastScanSwapListsEachShowsOwnEpisodes(t *testing.T) {
	b := newRenameBed(t)
	b.addOther(t)
	const mine, theirs = renameFetched, "2025-10-12-2fa6ddd63e.m4a" // each show's downloaded episode
	for _, f := range [][2]string{{"Example Show", mine}, {"Other", theirs}} {
		writeFetched(t, filepath.Join(b.root, f[0], f[1]))
		if _, err := b.cat.SaveProgress(b.ctx, b.user, catalog.Progress{Ref: catalog.Ref{LibraryID: b.lib.ID, Path: f[0] + "/" + f[1]}, Position: float64(len(f[0])), Duration: 1512, UpdatedAt: "2026-11-01T10:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.renameEach(t, [2]string{"Example Show", "tmp"}, [2]string{"Other", "Example Show"}, [2]string{"tmp", "Other"})
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Other")
	b.checkOther(t, "Example Show")
	for _, f := range [][3]string{{"Other", mine, "Example Show"}, {"Example Show", theirs, "Other"}} {
		if _, err := b.cat.GetBookByPath(b.ctx, b.lib.ID, f[0]+"/"+f[1]); err != nil {
			t.Fatalf("%s/%s not listed: %v", f[0], f[1], err)
		}
		p, err := b.cat.GetProgress(b.ctx, b.user, catalog.Ref{LibraryID: b.lib.ID, Path: f[0] + "/" + f[1]})
		if err != nil || p == nil || p.Position != float64(len(f[2])) {
			t.Fatalf("%s/%s place %+v %v", f[0], f[1], p, err)
		}
	}
}

// A chain of renames by hand: the other show to a new name, then this one to
// the other's old name. The Bridge's rescan of this show's new folder comes
// first, the other's new folder not indexed yet: it indexes that one too, so
// both move in one call and the listener following both keeps both follows
// (the move doesn't put this show's follow over the other's) and places.
func TestPodcastRescanMovesAChainOfRenames(t *testing.T) {
	b := newRenameBed(t)
	named := b.chain(t)
	if err := os.WriteFile(filepath.Join(b.root, "Third", "feed.json"), named("Other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Other"); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Other")
	b.checkOther(t, "Third")
}

// A chain of renames rescanned at its far end first (the other show's new
// folder): the other show moves alone, with the heart on its old folder, which
// this show holds by now only by the rename. This show's own rescan then moves
// it there without touching either.
func TestPodcastRescanOfAChainsFarEndTakesItsFolderHeart(t *testing.T) {
	b := newRenameBed(t)
	named := b.addOther(t)
	if err := b.cat.AddFavourite(b.ctx, b.user, catalog.Ref{LibraryID: b.lib.ID, Path: "Other"}); err != nil {
		t.Fatal(err)
	}
	b.renameEach(t, [2]string{"Other", "Third"}, [2]string{"Example Show", "Other"})
	b.writeShow(t, "Other", "Other")
	if err := os.WriteFile(filepath.Join(b.root, "Third", "feed.json"), named("Third"), 0o600); err != nil {
		t.Fatal(err)
	}
	hearted := func() {
		t.Helper()
		favs, err := b.cat.ListAllFavourites(b.ctx, b.user, []catalog.Scope{{LibraryID: b.lib.ID, AllowAll: true}})
		if err != nil || len(favs) != 1 || favs[0].Path != "Third" {
			t.Fatalf("the other show's heart %+v %v", favs, err)
		}
	}
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Third"); err != nil {
		t.Fatal(err)
	}
	b.checkOther(t, "Third")
	hearted()
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Other"); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Other")
	b.checkOther(t, "Third")
	hearted()
}

// Denied: a chain of renames by hand whose other folder can't be read or used
// for now (the other show renamed to "Third", this one to its old name):
// unreadable, or its feed.json of a newer version or invalid (half written).
// The folder itself may be one the server can't look into ("closed"): only a
// file system's own folder (a lost+found) is a stray, so this one counts too.
// That folder may be where the other show is, and a move to its old name would
// put this show's follow over the other's for a listener following both:
// nothing moves, on a scan (partial when it can't read the folder) or on the
// Bridge's rescan, until the folder reads again and both move in one call.
func TestPodcastRenameChainWaitsOnAnUnreadableFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a file the server's user can't read")
	}
	for _, how := range []string{"unreadable", "closed", "newer", "invalid"} {
		for _, rescan := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rescan=%v", how, rescan), func(t *testing.T) {
				b := newRenameBed(t)
				named := b.chain(t)
				third := filepath.Join(b.root, "Third", "feed.json")
				raw := named("Third")
				switch how {
				case "newer":
					raw = []byte(`{"version":2}`)
				case "invalid":
					raw = raw[:len(raw)/2]
				}
				if err := os.WriteFile(third, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if how == "unreadable" {
					if err := os.Chmod(third, 0o000); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(third, 0o600) })
				}
				dir := filepath.Dir(third)
				if how == "closed" {
					if err := os.Chmod(dir, 0o000); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
				}
				partial := how == "unreadable" || how == "closed"
				if rescan {
					if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Other"); err != nil {
						t.Fatal(err)
					}
				} else if res, err := b.s.Scan(b.ctx, *b.lib); err != nil || res.Partial != partial {
					t.Fatalf("scan %+v %v", res, err)
				}
				if got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Other", b.user); err != nil || got.Following {
					t.Fatalf("moved over the other show's follow: %+v %v", got, err)
				}
				if err := os.Chmod(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(third, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(third, named("Third"), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
					t.Fatal(err)
				}
				b.checkFollowed(t, "Other")
				b.checkOther(t, "Third")
			})
		}
	}
}

// An unrelated folder the server can't read (a root-owned lost+found on an
// ext4 volume) holds up no plain rename: a scan, not partial for it
// (strayPodcastFolder), and the Bridge's rescan each move the show to its new
// folder.
func TestPodcastRenameMovesBesideAnUnreadableFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder the server's user can't read")
	}
	for _, rescan := range []bool{false, true} {
		b := newRenameBed(t)
		lost := filepath.Join(b.root, "lost+found")
		if err := os.Mkdir(lost, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(lost, 0o700) })
		b.rename(t, "Renamed", true)
		if rescan {
			if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Renamed"); err != nil {
				t.Fatal(err)
			}
		} else if res, err := b.s.Scan(b.ctx, *b.lib); err != nil || res.Partial {
			t.Fatalf("scan %+v %v", res, err)
		}
		if got := b.shows(t); len(got) != 1 || got[0] != "Renamed" {
			t.Fatalf("rescan %v: shows %v", rescan, got)
		}
		b.checkFollowed(t, "Renamed")
	}
}

// A root entry the server can't look into for good (a root-owned lost+found)
// that the index knows nothing at makes no scan partial: a show deleted
// beside it is pruned, so its leftover follow is no show's indexed anywhere,
// and a rename by hand into its old name then moves, the stray folder not
// counting as one that may hold the deleted show (on a scan or the Bridge's
// rescan).
func TestPodcastScanPrunesBesideAStrayUnreadableFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder the server's user can't read")
	}
	for _, rescan := range []bool{false, true} {
		t.Run(fmt.Sprintf("rescan=%v", rescan), func(t *testing.T) {
			b := newRenameBed(t)
			b.addOther(t)
			lost := filepath.Join(b.root, "lost+found")
			if err := os.Mkdir(lost, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(lost, 0o700) })
			if err := os.RemoveAll(filepath.Join(b.root, "Other")); err != nil {
				t.Fatal(err)
			}
			if res, err := b.s.Scan(b.ctx, *b.lib); err != nil || res.Partial {
				t.Fatalf("scan %+v %v", res, err)
			}
			if got := b.shows(t); len(got) != 1 || got[0] != "Example Show" {
				t.Fatalf("the deleted show was not pruned: %v", got)
			}
			b.rename(t, "Other", true)
			if rescan {
				if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Other"); err != nil {
					t.Fatal(err)
				}
			} else if res, err := b.s.Scan(b.ctx, *b.lib); err != nil || res.Partial {
				t.Fatalf("scan %+v %v", res, err)
			}
			b.checkFollowed(t, "Other")
		})
	}
}

// Denied: a folder the server can't look into still makes the scan partial
// where the index knows it (a show's folder, its permissions changed), so the
// show is not pruned, and where podcast state is left at its name (a follow
// of a show gone from the index).
func TestPodcastScanStaysPartialOnAKnownUnreadableFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder the server's user can't read")
	}
	b := newRenameBed(t)
	b.addOther(t)
	other := filepath.Join(b.root, "Other")
	if err := os.Chmod(other, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(other, 0o700) })
	if res, err := b.s.Scan(b.ctx, *b.lib); err != nil || !res.Partial {
		t.Fatalf("scan %+v %v", res, err)
	}
	if got := b.shows(t); len(got) != 2 {
		t.Fatalf("a show whose folder can't be read was pruned: %v", got)
	}
	// Its row gone (an older run's prune), the follow left there still counts.
	if err := b.cat.PrunePodcastShows(b.ctx, b.lib.ID, []string{"Other"}); err != nil {
		t.Fatal(err)
	}
	if res, err := b.s.Scan(b.ctx, *b.lib); err != nil || !res.Partial {
		t.Fatalf("scan with state at the folder %+v %v", res, err)
	}
}

// Denied: a root the server may list but not search (no x bit, or a share
// dropping after its listing) lets no show folder be stat'ed. That is a
// partial scan, not every show gone: nothing is pruned.
func TestPodcastScanKeepsShowsItCannotStat(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder the server's user can't search")
	}
	b := newRenameBed(t)
	if err := os.Chmod(b.root, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(b.root, 0o700) })
	if res, err := b.s.Scan(b.ctx, *b.lib); err != nil || !res.Partial {
		t.Fatalf("scan %+v %v", res, err)
	}
	if err := os.Chmod(b.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := b.shows(t); len(got) != 1 || got[0] != "Example Show" {
		t.Fatalf("a show the scan could not stat was pruned: %v", got)
	}
}

// Denied: a show with only a place (no follow, feed state or policy), renamed
// by hand into a folder the server can't look into under a name nothing knows,
// is no stray: the scan is partial and keeps its old row, its one way back to
// the place, so the place moves once the folder reads again.
func TestPodcastScanKeepsAShowRenamedIntoAClosedFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder the server's user can't read")
	}
	b := newRenameBed(t)
	if err := b.cat.UnfollowPodcast(b.ctx, b.user, b.lib.ID, "Example Show"); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.SetPodcastPolicy(b.ctx, b.lib.ID, "Example Show", renameShowID, nil); err != nil {
		t.Fatal(err)
	}
	b.rename(t, "Renamed", true)
	dir := filepath.Join(b.root, "Renamed")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if res, err := b.s.Scan(b.ctx, *b.lib); err != nil || !res.Partial {
		t.Fatalf("scan %+v %v", res, err)
	}
	if got := b.shows(t); len(got) != 1 || got[0] != "Example Show" {
		t.Fatalf("the renamed show's old row was pruned: %v", got)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if res, err := b.s.Scan(b.ctx, *b.lib); err != nil || res.Partial {
		t.Fatalf("scan %+v %v", res, err)
	}
	p, err := b.cat.GetProgress(b.ctx, b.user, catalog.Ref{LibraryID: b.lib.ID, Path: "Renamed/" + renameEpisode})
	if err != nil || p == nil || p.Position != 42 {
		t.Fatalf("the place: %+v %v", p, err)
	}
}

// Only a refusal to look into a folder (EACCES, EPERM) makes it stray: a fault
// a retry may get past (metadata.TransientFault) or another errno does not, nor a
// refusal past a feed.json seen there, nor any error that is no file system's.
func TestStrayPodcastFolderErrors(t *testing.T) {
	b := newRenameBed(t)
	closed := func(err error) error {
		return fmt.Errorf("%w: %w", errFolderClosed, &os.PathError{Op: "lstat", Path: "x", Err: err})
	}
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{closed(syscall.EACCES), true},
		{closed(syscall.EPERM), true},
		{closed(syscall.EIO), false},
		{closed(syscall.ESTALE), false},
		{closed(syscall.ETIMEDOUT), false},
		{closed(syscall.ELOOP), false},
		{&os.PathError{Op: "open", Path: "x", Err: syscall.EACCES}, false},
		{fmt.Errorf("%w: %w", errFolderClosed, context.Canceled), false},
		{nil, false},
	} {
		if got := b.s.strayPodcastFolder(b.ctx, *b.lib, "lost+found", tc.err); got != tc.want {
			t.Errorf("%v: stray %v, want %v", tc.err, got, tc.want)
		}
	}
	// A refusal at a folder the index knows is no stray, nor at any name but a
	// file system's own: a show renamed by hand there looks just the same.
	if b.s.strayPodcastFolder(b.ctx, *b.lib, "Example Show", closed(syscall.EACCES)) {
		t.Error("a known show's folder taken for a stray")
	}
	if b.s.strayPodcastFolder(b.ctx, *b.lib, "Renamed", closed(syscall.EACCES)) {
		t.Error("an unknown folder taken for a stray")
	}
	if !b.s.strayPodcastFolder(b.ctx, *b.lib, "System Volume Information", closed(syscall.EPERM)) {
		t.Error("a file system's own folder not taken for a stray")
	}
}

// Denied: a chain of renames by hand into a folder still indexed as a third
// show's (that show deleted, its row not pruned yet): the other show renamed
// to it, this one to the other's old name. The Bridge's rescan of this one's
// new folder indexes no folder that has a row, so it can't find the other
// show there; it takes that folder for unread, and the move waits rather than
// put this show's follow over the other's. A scan then moves both.
func TestPodcastRescanChainIntoAStaleRowFolderWaits(t *testing.T) {
	b := newRenameBed(t)
	named := b.addOther(t)
	b.addThird(t)
	if err := os.RemoveAll(filepath.Join(b.root, "Third")); err != nil {
		t.Fatal(err)
	}
	b.renameEach(t, [2]string{"Other", "Third"}, [2]string{"Example Show", "Other"})
	b.writeShow(t, "Other", "Other")
	if err := os.WriteFile(filepath.Join(b.root, "Third", "feed.json"), named("Third"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Other"); err != nil {
		t.Fatal(err)
	}
	if got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Other", b.user); err != nil || got.Following {
		t.Fatalf("moved over the other show's follow: %+v %v", got, err)
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Other")
	b.checkOther(t, "Third")
}

// Denied: a rescan asks the index whether a move's new folder carries a show
// indexed nowhere before the move's transaction does. The other show here is
// indexed elsewhere then (two copies, so its own move is not planned), which
// holds the move with no folder read; the copies' rows go in between (their
// folders removed, a scan's prune), and the move still waits: the folders were
// never read for that show, which a folder with a stale row holds. A scan then
// moves both.
func TestPodcastRescanMoveWaitsWhenTheOtherShowLosesItsRows(t *testing.T) {
	b := newRenameBed(t)
	named := b.addOther(t)
	b.addThird(t)
	copies := []string{"Copy One", "Copy Two"}
	for _, f := range copies {
		if err := os.MkdirAll(filepath.Join(b.root, f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b.root, f, "feed.json"), named(f), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, f); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Join(b.root, "Third")); err != nil {
		t.Fatal(err)
	}
	b.renameEach(t, [2]string{"Other", "Third"}, [2]string{"Example Show", "Other"})
	b.writeShow(t, "Other", "Other")
	if err := os.WriteFile(filepath.Join(b.root, "Third", "feed.json"), named("Third"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The rescan of "Other", step by step (IndexPodcastPath, movePodcastShows).
	if _, _, err := b.s.indexPodcastShow(b.ctx, *b.lib, "Other", true); err != nil {
		t.Fatal(err)
	}
	var listed map[string]bool
	moves, err := b.s.settledPodcastMoves(b.ctx, *b.lib, "Other", &listed)
	if err != nil || len(moves) != 1 || moves[0].ShowID != renameShowID || moves[0].To != "Other" {
		t.Fatalf("planned %+v %v", moves, err)
	}
	if n, skipped, err := b.s.indexNewPodcastFolders(b.ctx, *b.lib, &listed); err != nil || n != 0 || skipped {
		t.Fatalf("new folders %d %v %v", n, skipped, err)
	}
	unread, err := b.s.stalePodcastRow(b.ctx, *b.lib, moves, listed)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range copies {
		if err := os.RemoveAll(filepath.Join(b.root, f)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.cat.PrunePodcastShows(b.ctx, b.lib.ID, copies); err != nil {
		t.Fatal(err)
	}
	if n, err := b.cat.MovePodcastShows(b.ctx, b.lib.ID, moves, unread); err != nil || n != 0 {
		t.Fatalf("moved %d %v over a show whose folders were not read", n, err)
	}
	if got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Other", b.user); err != nil || got.Following {
		t.Fatalf("moved over the other show's follow: %+v %v", got, err)
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Other")
	b.checkOther(t, "Third")
}

// A rescan's move onto a folder carrying a gone show's follow looks at the
// indexed folders for that show, by identity only: an unrelated show with a
// new episode file since its row was written (its signature changed, its
// show_id not) holds nothing up, so the move goes as a scan's would.
func TestPodcastRescanMoveIgnoresANewEpisodeElsewhere(t *testing.T) {
	b := newRenameBed(t)
	b.addOther(t)
	b.addThird(t)
	if err := os.WriteFile(filepath.Join(b.root, "Third", renameEpisode), []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(b.root, "Other")); err != nil {
		t.Fatal(err)
	}
	b.renameEach(t, [2]string{"Example Show", "Other"})
	b.writeShow(t, "Other", "Other")
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Other"); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Other")
}

// Denied: a chain of renames whose other show can't move yet, as it is in two
// folders (renamed to "Third", and copied to "Fourth"). This show, renamed to
// the other's old name, waits too, so the follow of the other show stays; once
// the copy goes, both move in one call.
func TestPodcastRenameChainWaitsOnACopiedPartner(t *testing.T) {
	b := newRenameBed(t)
	named := b.chain(t)
	for _, f := range []string{"Third", "Fourth"} {
		if err := os.MkdirAll(filepath.Join(b.root, f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b.root, f, "feed.json"), named(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	if got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Other", b.user); err != nil || got.Following {
		t.Fatalf("moved over the other show's follow: %+v %v", got, err)
	}
	if err := os.RemoveAll(filepath.Join(b.root, "Fourth")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Other")
	b.checkOther(t, "Third")
}

// A held move's old folder keeps its index row through the scan's prune (the
// feed.json not usable is no partial scan): a show whose only state there is
// a place (no follow, feed state or policy) is found at both folders again
// once the hold clears, so its place moves.
func TestPodcastHeldMoveKeepsItsOldRowThroughThePrune(t *testing.T) {
	b := newRenameBed(t)
	named := b.chain(t)
	if err := b.cat.UnfollowPodcast(b.ctx, b.user, b.lib.ID, "Example Show"); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.SetPodcastPolicy(b.ctx, b.lib.ID, "Example Show", renameShowID, nil); err != nil {
		t.Fatal(err)
	}
	third := filepath.Join(b.root, "Third", "feed.json")
	if err := os.WriteFile(third, named("Third")[:100], 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := b.s.Scan(b.ctx, *b.lib); err != nil || res.Partial {
		t.Fatalf("scan %+v %v", res, err)
	}
	if got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Other", b.user); err != nil || got.ShowID != renameShowID {
		t.Fatalf("other %+v %v", got, err)
	}
	if err := os.WriteFile(third, named("Third"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	p, err := b.cat.GetProgress(b.ctx, b.user, catalog.Ref{LibraryID: b.lib.ID, Path: "Other/" + renameEpisode})
	if err != nil || p == nil || p.Position != 42 {
		t.Fatalf("the held show's place: %+v %v", p, err)
	}
	b.checkOther(t, "Third")
}

// listenEpisodeNews counts the new-episode notices b's catalog tells.
func (b renameBed) listenEpisodeNews() *atomic.Int32 {
	var told atomic.Int32
	b.cat.SetPodcastNotice(&catalog.PodcastListener{
		Event: func(n catalog.PodcastNotice) (string, map[string]any, string) {
			if n.Kind == catalog.PodcastEpisodesNew {
				told.Add(int32(len(n.EpisodeIDs)))
			}
			return "", nil, ""
		},
		Deliver: func(context.Context, catalog.ServerEvent) {},
	})
	return &told
}

// faultFolder makes the rerooter's looks at b's folder fail with err until the
// test ends (feed.FaultForTests; nil clears it), the folder named as the
// rerooter names it: below the root with its links resolved (SafeJoin's), as
// macOS's temporary folder is reached through one (/var -> /private/var).
func (b renameBed) faultFolder(t *testing.T, folder string, err error) {
	t.Helper()
	root, rerr := filepath.EvalSymlinks(b.root)
	if rerr != nil {
		t.Fatal(rerr)
	}
	name := filepath.Join(root, folder)
	feed.FaultForTests(name, err)
	t.Cleanup(func() { feed.FaultForTests(name, nil) })
}

// Denied: a scan's quiet re-read of a renamed folder (its row re-rooted) that
// can't tell whether its old folder holds the show (EIO there: feed.File.Unsure)
// keeps the row exactly as it was: no episode dropped, no re-index, no notice,
// the same signature and cover_version. Once the fault is gone, nothing
// changes either.
func TestPodcastQuietReReadKeepsItsRowOnAFault(t *testing.T) {
	b := newRenameBed(t)
	writeFetched(t, filepath.Join(b.root, "Example Show", renameFetched))
	if err := os.WriteFile(filepath.Join(b.root, "Example Show", "cover.jpg"), []byte("jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.rename(t, "Renamed", false)
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	before, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Renamed", b.user)
	if err != nil || before.EpisodeCount != 1 || before.CoverVersion == "" {
		t.Fatalf("renamed: %+v %v", before, err)
	}
	told := b.listenEpisodeNews()
	for _, fault := range []error{syscall.EIO, nil} {
		b.faultFolder(t, "Example Show", fault)
		if res, err := b.s.Scan(b.ctx, *b.lib); err != nil || res.Added+res.Updated != 0 {
			t.Fatalf("%v: scan %+v %v", fault, res, err)
		}
		after, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Renamed", b.user)
		if err != nil || after.FeedSig != before.FeedSig || after.CoverVersion != before.CoverVersion || after.EpisodeCount != 1 {
			t.Fatalf("%v: the row %+v, was %+v (%v)", fault, after, before, err)
		}
		if _, err := b.cat.GetBookByPath(b.ctx, b.lib.ID, "Renamed/"+renameFetched); err != nil {
			t.Fatalf("%v: the episode: %v", fault, err)
		}
	}
	if n := told.Load(); n != 0 {
		t.Fatalf("told %d new episodes", n)
	}
}

// Denied: a read that can't tell (EIO at the old folder: feed.File.Unsure)
// keeps an indexed row as it was however it comes, the folder changed since
// (a scan that is no quiet re-read) or the Bridge's forced rescan: the
// episode it left out stays listed. Allowed: once the fault is gone, the next
// scan indexes the change.
func TestPodcastUnsureReadKeepsAnIndexedRow(t *testing.T) {
	b := newRenameBed(t)
	writeFetched(t, filepath.Join(b.root, "Example Show", renameFetched))
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	b.rename(t, "Renamed", false)
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	before, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Renamed", b.user)
	if err != nil || before.EpisodeCount != 1 {
		t.Fatalf("renamed: %+v %v", before, err)
	}
	if err := os.WriteFile(filepath.Join(b.root, "Renamed", "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.faultFolder(t, "Example Show", syscall.EIO)
	kept := func(how string) {
		t.Helper()
		after, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Renamed", b.user)
		if err != nil || after.FeedSig != before.FeedSig || after.EpisodeCount != 1 {
			t.Fatalf("%s: the row %+v, was %+v (%v)", how, after, before, err)
		}
		if _, err := b.cat.GetBookByPath(b.ctx, b.lib.ID, "Renamed/"+renameFetched); err != nil {
			t.Fatalf("%s: the episode: %v", how, err)
		}
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	kept("scan")
	if show, n, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Renamed"); err != nil || show != "Renamed" || n != 1 {
		t.Fatalf("rescan: %q %d %v", show, n, err)
	}
	kept("rescan")
	b.faultFolder(t, "Example Show", nil)
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	after, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Renamed", b.user)
	if err != nil || after.FeedSig == before.FeedSig || after.EpisodeCount != 1 {
		t.Fatalf("read again: the row %+v, was %+v (%v)", after, before, err)
	}
}

// Denied: a first read of a folder renamed only in case (a disk that ignores
// case) that can't tell (EIO at the old spelling) lists none of the episodes
// it would read at the folder's spelling and tells nothing; the next scan,
// which can, lists them all, telling nothing either: its feed.json is the one
// the row was read from, so they are no news, however recent.
func TestPodcastFirstReadOnAFaultTellsNothing(t *testing.T) {
	b := newRenameBed(t)
	b.rename(t, "example show", false)
	if _, err := os.Lstat(filepath.Join(b.root, "Example Show")); err != nil {
		t.Skipf("the disk keeps names apart by case: %v", err)
	}
	recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	raw := bytes.ReplaceAll(b.feed, []byte(`"first_seen": "2026-10-20T08:00:00Z"`), []byte(`"first_seen": "`+recent+`"`))
	if bytes.Equal(raw, b.feed) {
		t.Fatal("the fixture's first_seen is not in its feed")
	}
	if err := os.WriteFile(filepath.Join(b.root, "example show", "feed.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFetched(t, filepath.Join(b.root, "example show", renameFetched))
	told := b.listenEpisodeNews()
	b.faultFolder(t, "Example Show", syscall.EIO)
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	if n, err := b.cat.PodcastEpisodeRows(b.ctx, b.lib.ID, "example show"); err != nil || n != 0 {
		t.Fatalf("read on a fault: episodes %d %v", n, err)
	}
	b.faultFolder(t, "Example Show", nil)
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	if n, err := b.cat.PodcastEpisodeRows(b.ctx, b.lib.ID, "example show"); err != nil || n != 3 {
		t.Fatalf("read again: episodes %d %v", n, err)
	}
	if n := told.Load(); n != 0 {
		t.Fatalf("told %d new episodes", n)
	}
}

// The Bridge's rescan at its feed.json's spelling of a folder the root lists in
// another (renamed by hand only in case, or to the other Unicode form, on a
// disk that opens both: APFS, SMB) indexes the folder at the spelling listed
// and answers it, so it has one row, the show's state moved there; the
// spelling asked gets none.
func TestPodcastRescanAtTheFeedsSpellingIndexesTheListedFolder(t *testing.T) {
	for _, c := range []struct{ name, listed, asked string }{
		{"case", "example show", "Example Show"},
		{"form", "Café Show", "Café Show"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newRenameBed(t)
			b.rename(t, c.listed, false)
			if _, err := os.Lstat(filepath.Join(b.root, c.asked)); err != nil {
				t.Skipf("the disk keeps the two spellings apart: %v", err)
			}
			if names, err := os.ReadDir(b.root); err != nil || len(names) != 1 || names[0].Name() != c.listed {
				t.Skipf("the disk does not keep the name as given: %v %v", names, err)
			}
			b.writeShow(t, c.listed, c.asked)
			show, n, err := b.s.IndexPodcastPath(b.ctx, *b.lib, c.asked)
			if err != nil || show != c.listed || n != 3 {
				t.Fatalf("rescan: %q %d %v", show, n, err)
			}
			if got := b.shows(t); len(got) != 1 || got[0] != c.listed {
				t.Fatalf("shows %q", got)
			}
			b.checkFollowed(t, c.listed)
		})
	}
}

package library

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
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

// Two folder names swapped by hand: the Bridge's rescan of one moves both
// shows, the listener following each keeping both.
func TestPodcastRescanSwapsTwoFolders(t *testing.T) {
	b := newRenameBed(t)
	other := bytes.ReplaceAll(b.feed, []byte(renameShowID), []byte("0123456789abcdef"))
	other = bytes.ReplaceAll(other, []byte(`"Example Show/`), []byte(`"Other/`))
	// Its own episode files (their names carry the episode's id).
	other = bytes.ReplaceAll(other, []byte(`/2026-`), []byte(`/2025-`))
	other = bytes.ReplaceAll(other, []byte(`/undated-`), []byte(`/undated-other-`))
	if err := os.MkdirAll(filepath.Join(b.root, "Other"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.root, "Other", "feed.json"), other, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.Scan(b.ctx, *b.lib); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.FollowPodcast(b.ctx, b.user, b.lib.ID, "Other", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	// Swap the names, then the Bridge takes in both folders.
	for _, mv := range [][2]string{{"Example Show", "tmp"}, {"Other", "Example Show"}, {"tmp", "Other"}} {
		if err := os.Rename(filepath.Join(b.root, mv[0]), filepath.Join(b.root, mv[1])); err != nil {
			t.Fatal(err)
		}
	}
	b.writeShow(t, "Other", "Other")
	if err := os.WriteFile(filepath.Join(b.root, "Example Show", "feed.json"), bytes.ReplaceAll(other, []byte(`"Other/`), []byte(`"Example Show/`)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.s.IndexPodcastPath(b.ctx, *b.lib, "Other"); err != nil {
		t.Fatal(err)
	}
	b.checkFollowed(t, "Other")
	got, err := b.cat.PodcastShow(b.ctx, b.lib.ID, "Example Show", b.user)
	if err != nil || !got.Following || got.ShowID != "0123456789abcdef" || got.Retention.Mode != "keep" {
		t.Fatalf("the other show: %+v %v", got, err)
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

// A chain of renames by hand: the other show to a new name, then this one to
// the other's old name. The Bridge's rescan of this show's new folder comes
// first, the other's new folder not indexed yet: it indexes that one too, so
// both move in one call and the listener following both keeps both follows
// (the move doesn't put this show's follow over the other's) and places.
func TestPodcastRescanMovesAChainOfRenames(t *testing.T) {
	b := newRenameBed(t)
	named := b.addOther(t)
	b.renameEach(t, [2]string{"Other", "Third"}, [2]string{"Example Show", "Other"})
	b.writeShow(t, "Other", "Other")
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

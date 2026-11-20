package library

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func podcastDiskFixture(t testing.TB) (context.Context, *catalog.Catalog, *catalog.Library, *Scanner, *feed.File, string) {
	t.Helper()
	ctx := context.Background()
	cat := catalog.New(storetest.Open(t), time.Now)
	// Resolved, as the looks name it (ContentJoin): a test injecting a share's
	// errors by path then names the very path looked at (macOS's temp folder
	// is under a link, /var to /private/var).
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "Fixture Show")
	if err := os.MkdirAll(filepath.Join(dir, ".audiosilo"), 0755); err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/phase2c2", "../podcast/testdata/phase2c2/feed-fetch-v1.json", "Fixture Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/feed-fetch-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "feed.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Pods", Root: root, Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner(cat, "must-not-probe", slog.Default())
	return ctx, cat, lib, scanner, f, dir
}
func writePodcastFixture(t testing.TB, dest, name string) {
	t.Helper()
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dest, raw, 0644); err != nil {
		t.Fatal(err)
	}
}
func TestPodcastCommittedCHAPAndEviction(t *testing.T) {
	for _, golden := range []string{"chap-v23", "chap-v24-reverse"} {
		t.Run(golden, func(t *testing.T) {
			ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
			ep := &f.Episodes[0]
			// The goldens give exact bytes and timing, while feed duration is misleading.
			var chapters struct {
				GeneratedAt string `json:"generated_at"`
				Files       []struct {
					Size int64 `json:"size"`
				}
			}
			raw, err := os.ReadFile("../podcast/testdata/phase2c2/" + golden + ".chapters.json")
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &chapters); err != nil {
				t.Fatal(err)
			}
			ep.Fetch.Bytes = chapters.Files[0].Size
			ep.Fetch.FetchedAt = chapters.GeneratedAt
			if golden == "chap-v23" {
				ep.Fetch.SHA256 = "b9e2206c3fd1f9a9e9c78e9b467d888926e0396ec49760736f78705920ba8dda"
			}
			raw, _ = json.Marshal(f)
			if err = os.WriteFile(filepath.Join(dir, "feed.json"), raw, 0644); err != nil {
				t.Fatal(err)
			}
			audio := filepath.Join(lib.Root, filepath.FromSlash(ep.Path))
			writePodcastFixture(t, audio, golden+".mp3")
			sidecar := filepath.Join(dir, ".audiosilo", stem(path.Base(ep.Path))+".chapters.json")
			writePodcastFixture(t, sidecar, golden+".chapters.json")
			if _, err = s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			b, err := cat.GetBookByPath(ctx, lib.ID, ep.Path)
			if err != nil || b.Duration != 4.056 || b.Title != "Current generation" || b.Codec != "mp3" || b.ChaptersSource != "id3_chap" || len(b.Chapters) != 3 || b.Chapters[1].Title != "Chapter 2" || b.Chapters[1].Start != 1 || len(b.Files) != 1 || b.Generated {
				t.Fatalf("CHAP: %+v %v", b, err)
			}
			// Removing only the sidecar is noticed with feed.json untouched.
			if err = os.Remove(sidecar); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			b, err = cat.GetBookByPath(ctx, lib.ID, ep.Path)
			if err != nil || len(b.Chapters) != 1 || b.ChaptersSource != "" {
				t.Fatalf("fallback %+v %v", b, err)
			}
			if err = os.Remove(audio); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			b, err = cat.GetBookByPath(ctx, lib.ID, ep.Path)
			if err != nil || b.Episode.Downloaded || b.Size != 0 || b.Duration != 999 || len(b.Files) != 0 || len(b.Chapters) != 0 || b.Episode.Generation != 2 {
				t.Fatalf("eviction %+v %v", b, err)
			}
		})
	}
}
func stem(base string) string { return base[:len(base)-len(path.Ext(base))] }

func TestPodcastPublicationBoundariesFailClosed(t *testing.T) {
	// The Bridge owns journal recovery. These are the server-visible snapshots
	// after faults at each publication boundary, including failed targeted rescan.
	for _, boundary := range []string{"journal", "journal_symlink", "audio", "sidecar", "feed", "remove", "rescan_failed", "committed"} {
		t.Run(boundary, func(t *testing.T) {
			ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
			ep := f.Episodes[0]
			feedPath := filepath.Join(dir, "feed.json")
			newFeed, _ := os.ReadFile(feedPath)
			ep0 := f.Episodes[0]
			ep0.Fetch = nil
			f.Episodes[0] = ep0
			oldFeed, _ := json.Marshal(f)
			os.WriteFile(feedPath, oldFeed, 0644)
			if _, err := s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(dir, ".audiosilo", stem(path.Base(ep.Path))+".publish.json")
			writePodcastFixture(t, journal, "publish-v1.json")
			if boundary == "journal_symlink" {
				if err := os.Rename(journal, journal+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(journal+".target", journal); err != nil {
					t.Fatal(err)
				}
			}
			if boundary != "journal" {
				writePodcastFixture(t, filepath.Join(lib.Root, filepath.FromSlash(ep.Path)), "chap-v24-reverse.mp3")
			}
			if boundary != "journal" && boundary != "audio" {
				writePodcastFixture(t, filepath.Join(dir, ".audiosilo", stem(path.Base(ep.Path))+".chapters.json"), "chap-v24-reverse.chapters.json")
			}
			if boundary == "feed" || boundary == "remove" || boundary == "rescan_failed" || boundary == "committed" {
				os.WriteFile(feedPath, newFeed, 0644)
			}
			if boundary == "remove" || boundary == "rescan_failed" || boundary == "committed" {
				os.Remove(journal)
			}
			if boundary != "rescan_failed" && boundary != "remove" {
				if _, _, err := s.IndexPodcastPath(ctx, *lib, ep.Path); err != nil {
					t.Fatal(err)
				}
			}
			opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
			if boundary == "committed" {
				if err != nil {
					t.Fatal(err)
				}
				opened.Close()
			} else {
				if opened != nil {
					opened.Close()
					t.Fatal("uncommitted generation served")
				}
				if err == nil {
					t.Fatal("missing barrier error")
				}
			}
			// An index-only rebuild at any barrier keeps the episode without granting presence.
			b, err := cat.GetBookByPath(ctx, lib.ID, ep.Path)
			if err != nil || (boundary != "committed" && b.Episode.Downloaded) {
				t.Fatalf("snapshot: %+v %v", b, err)
			}
		})
	}
}
func TestPodcastDescriptorSurvivesRenameAndLinksDenied(t *testing.T) {
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	audio := filepath.Join(lib.Root, filepath.FromSlash(ep.Path))
	writePodcastFixture(t, audio, "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	opened, pin, err := OpenPodcast(ctx, cat, *lib, ep.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if pin.Generation != 2 {
		t.Fatal(pin)
	}
	if err = os.Rename(audio, audio+".retired"); err != nil {
		t.Skipf("OS refuses open-file rename: %v", err)
	}
	data, err := io.ReadAll(opened)
	if err != nil || int64(len(data)) != pin.Bytes {
		t.Fatal("opened generation was lost")
	}
	if err = os.Symlink(audio+".retired", audio); err != nil {
		t.Fatal(err)
	}
	if _, _, err = OpenPodcast(ctx, cat, *lib, ep.Path); err == nil {
		t.Fatal("audio symlink served")
	}
	os.Remove(audio)
	os.Rename(audio+".retired", audio)
	os.Rename(filepath.Join(dir, ".audiosilo"), filepath.Join(dir, "attachments"))
	os.Symlink(filepath.Join(dir, "attachments"), filepath.Join(dir, ".audiosilo"))
	if _, _, err = OpenPodcast(ctx, cat, *lib, ep.Path); err == nil {
		t.Fatal("attachment-directory symlink served")
	}
}

// TestPodcastFeedMemo (server#180): an episode opens again from the feed copy
// read before (allowed), and a feed.json replaced by another file at the same
// size and mtime, whose episode no longer names the pinned generation, is read
// again and refused (denied), as is the copy remembered for a show once its
// episode is gone from a new copy.
func TestPodcastFeedMemo(t *testing.T) {
	t.Parallel()
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	writePodcastFixture(t, filepath.Join(lib.Root, filepath.FromSlash(ep.Path)), "chap-v24-reverse.mp3")
	filename := filepath.Join(dir, "feed.json")
	setFeedTime(t, filename, time.Now().Add(-time.Hour)) // past the racy window
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
		if err != nil {
			t.Fatal(err)
		}
		opened.Close()
	}
	real, err := filepath.EvalSymlinks(filename) // remembered under the root's real path
	if err != nil {
		t.Fatal(err)
	}
	remembered, ok := podcastFeeds.feeds.Get(feedMemoGen, real)
	if !ok || remembered.showID != f.ShowID {
		t.Fatal("the feed copy read was not remembered")
	}
	// Another file, same size and mtime (so the pin's feed signature still
	// matches), its episode's digest changed in one hex digit.
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filename + ".tmp"
	if err := os.WriteFile(tmp, regeneratedFeed(t, filename, ep.Fetch.SHA256), 0o644); err != nil {
		t.Fatal(err)
	}
	setFeedTime(t, tmp, info.ModTime())
	if err := os.Rename(tmp, filename); err != nil {
		t.Fatal(err)
	}
	if opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path); err == nil {
		opened.Close()
		t.Fatal("served from the feed copy remembered for another file")
	}
}

// TestPodcastFeedMemoRacy (server#180): a feed.json read within the racy
// window of its mtime is not remembered, so the same file rewritten in place at
// the same size and mtime (a timestamp too coarse to tell the writes apart),
// its episode no longer naming the pinned generation, is read again and refused.
func TestPodcastFeedMemoRacy(t *testing.T) {
	t.Parallel()
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	writePodcastFixture(t, filepath.Join(lib.Root, filepath.FromSlash(ep.Path)), "chap-v24-reverse.mp3")
	filename := filepath.Join(dir, "feed.json")
	mtime := time.Now().Add(time.Hour) // never past the window, however slow the run
	setFeedTime(t, filename, mtime)
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
	before, err := os.Lstat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, regeneratedFeed(t, filename, ep.Fetch.SHA256), 0o644); err != nil {
		t.Fatal(err)
	}
	setFeedTime(t, filename, mtime)
	if after, err := os.Lstat(filename); err != nil || !os.SameFile(before, after) || feedSignature(after) != feedSignature(before) {
		t.Fatalf("the rewrite was not in place at the same signature (%v)", err)
	}
	if opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path); err == nil {
		opened.Close()
		t.Fatal("served from a feed copy read within the racy window")
	}
}

// A show folder renamed by hand lists its downloaded episodes from a feed.json
// naming the old folder (feed.ReadFile re-roots them against the folder's
// listing), so the copy read depends on more than feed.json and is never
// remembered: an episode file added to the folder after a stream read the copy
// (a move by hand still copying) opens once the scan lists it.
func TestPodcastFeedMemoRenamedFolder(t *testing.T) {
	t.Parallel()
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	first, second := f.Episodes[0], f.Episodes[1] // the second's fetch is deleted in the fixture
	filename := filepath.Join(dir, "feed.json")
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(strings.Replace(string(raw), `"2026-10-21T07:00:00Z"`, `""`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(lib.Root, "Renamed")
	if err := os.Rename(dir, renamed); err != nil {
		t.Fatal(err)
	}
	setFeedTime(t, filepath.Join(renamed, "feed.json"), time.Now().Add(-time.Hour)) // past the racy window
	writePodcastFixture(t, filepath.Join(renamed, path.Base(first.Path)), "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	opened, _, err := OpenPodcast(ctx, cat, *lib, "Renamed/"+path.Base(first.Path))
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
	writePodcastFixture(t, filepath.Join(renamed, path.Base(second.Path)), "chap-v23.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	opened, _, err = OpenPodcast(ctx, cat, *lib, "Renamed/"+path.Base(second.Path))
	if err != nil {
		t.Fatalf("an episode added since the copy was read: %v", err)
	}
	opened.Close()
}

// A renamed folder lists its downloaded episode from a feed.json naming the old
// folder; once the old folder holds the show again (a copy put back, its
// folder unchanged) the next scan reads the renamed one again, and its listing
// and a stream agree: the episode is not there, never listed as present while
// every stream of it is refused.
func TestPodcastRenamedFolderListingFollowsItsOldFolder(t *testing.T) {
	t.Parallel()
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	rel := "Renamed/" + path.Base(f.Episodes[0].Path)
	writePodcastFixture(t, filepath.Join(dir, path.Base(f.Episodes[0].Path)), "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(lib.Root, "Renamed")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	listedAndPlays := func() (listed, plays bool) {
		t.Helper()
		pin, err := cat.PodcastPin(ctx, lib.ID, rel)
		listed = err == nil && pin.Present
		opened, _, err := OpenPodcast(ctx, cat, *lib, rel)
		if err == nil {
			opened.Close()
		}
		return listed, err == nil
	}
	if listed, plays := listedAndPlays(); !listed || !plays {
		t.Fatalf("renamed: listed %v, plays %v", listed, plays)
	}
	// Read again while nothing changed, it indexes nothing.
	if res, err := s.Scan(ctx, *lib); err != nil || res.Added+res.Updated != 0 {
		t.Fatalf("an unchanged scan: %+v %v", res, err)
	}
	raw, err := os.ReadFile(filepath.Join(lib.Root, "Renamed", "feed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feed.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if listed, plays := listedAndPlays(); listed || plays {
		t.Fatalf("beside its old folder again: listed %v, plays %v", listed, plays)
	}
	// Once the old folder goes again, the next scan lists it once more.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if listed, plays := listedAndPlays(); !listed || !plays {
		t.Fatalf("its old folder gone again: listed %v, plays %v", listed, plays)
	}
}

// A show folder renamed by hand to its other Unicode spelling (NFC to NFD; a
// disk may keep both) is the same folder: the scan lists every episode its
// feed.json names in the first spelling, downloaded or not (so a new one is
// asked for), at the spelling on disk, and the downloaded one streams.
func TestPodcastFolderInAnotherSpellingListsAndStreams(t *testing.T) {
	t.Parallel()
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	nfc, nfd := "Caf\u00e9 Show", "Cafe\u0301 Show"
	raw, err := os.ReadFile(filepath.Join(dir, "feed.json"))
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(lib.Root, nfd)
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if names, err := os.ReadDir(lib.Root); err != nil || len(names) != 1 || names[0].Name() != nfd {
		t.Skipf("the disk does not keep an NFD name as given: %v %v", names, err)
	}
	if err := os.WriteFile(filepath.Join(moved, "feed.json"), []byte(strings.ReplaceAll(string(raw), `"Fixture Show/`, `"`+nfc+`/`)), 0o644); err != nil {
		t.Fatal(err)
	}
	base := path.Base(f.Episodes[0].Path)
	writePodcastFixture(t, filepath.Join(moved, base), "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if n, err := cat.PodcastEpisodeRows(ctx, lib.ID, nfd); err != nil || n != len(f.Episodes) {
		t.Fatalf("episodes %d of %d, %v", n, len(f.Episodes), err)
	}
	opened, _, err := OpenPodcast(ctx, cat, *lib, nfd+"/"+base)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
}

// A show folder renamed by hand only in case on a disk that ignores case
// (APFS, SMB) is the same folder: the next scan lists every episode its
// feed.json names at the old spelling, downloaded or not, at the spelling on
// disk, and the downloaded one streams at once, before the Bridge rewrites
// feed.json. (A disk that keeps both names apart reads the old one as another
// folder: TestReadFeedReadsACaseOnlyRename.)
func TestPodcastCaseOnlyRenameListsAndStreams(t *testing.T) {
	t.Parallel()
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	base := path.Base(f.Episodes[0].Path)
	writePodcastFixture(t, filepath.Join(dir, base), "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(lib.Root, "fixture show")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if names, err := os.ReadDir(lib.Root); err != nil || len(names) != 1 || names[0].Name() != "fixture show" {
		t.Fatalf("the rename: %v %v", names, err)
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		t.Skipf("the disk keeps names apart by case: %v", err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if n, err := cat.PodcastEpisodeRows(ctx, lib.ID, "fixture show"); err != nil || n != len(f.Episodes) {
		t.Fatalf("episodes %d of %d, %v", n, len(f.Episodes), err)
	}
	opened, _, err := OpenPodcast(ctx, cat, *lib, "fixture show/"+base)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
}

// Denied: a copy of a show folder made beside it in the other Unicode spelling
// (a disk that keeps the NFC and NFD names apart) is a copy, as one under any
// other name: it lists none of the show's episodes twice, so none is asked for
// in a folder the Bridge takes for a copy.
func TestPodcastCopyInAnotherSpellingListsNoEpisode(t *testing.T) {
	t.Parallel()
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	nfc, nfd := "Caf\u00e9 Show", "Cafe\u0301 Show"
	raw, err := os.ReadFile(filepath.Join(dir, "feed.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), `"Fixture Show/`, `"`+nfc+`/`))
	base := path.Base(f.Episodes[0].Path)
	for _, name := range []string{nfc, nfd} {
		folder := filepath.Join(lib.Root, name)
		if err := os.Mkdir(folder, 0o755); err != nil {
			t.Skipf("the disk does not keep NFC and NFD names apart: %v", err)
		}
		if err := os.WriteFile(filepath.Join(folder, "feed.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		writePodcastFixture(t, filepath.Join(folder, base), "chap-v24-reverse.mp3")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if n, err := cat.PodcastEpisodeRows(ctx, lib.ID, nfc); err != nil || n != len(f.Episodes) {
		t.Fatalf("the original: episodes %d of %d, %v", n, len(f.Episodes), err)
	}
	if n, err := cat.PodcastEpisodeRows(ctx, lib.ID, nfd); err != nil || n != 0 {
		t.Fatalf("the copy: episodes %d, %v", n, err)
	}
}

// regeneratedFeed is filename's feed.json with digest, an episode's fetch
// digest, changed in its last hex digit: the same size, another generation.
func regeneratedFeed(t *testing.T, filename, digest string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	last := "0"
	if strings.HasSuffix(digest, "0") {
		last = "1"
	}
	changed := strings.Replace(string(raw), digest, digest[:len(digest)-1]+last, 1)
	if changed == string(raw) {
		t.Fatal("the fixture's digest is not in its feed")
	}
	return []byte(changed)
}

// setFeedTime sets filename's access and modification times to at.
func setFeedTime(t testing.TB, filename string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(filename, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestOpenPodcastReadsAShareFault: an episode whose look (its show's folder,
// feed.json, the publish barrier, the episode itself) fails as a share does
// that a retry may get past (EIO, ESTALE) is a look given up on (the stream's
// 503 share_not_answering), never the episode absent (ErrNotIndexable), which
// would ask the Bridge to fetch an episode the library holds. Denied the 503:
// a permission is ErrEpisodeRefused (the stream's 404, no fetch), the file
// gone ErrNotIndexable and no refusal; a permission above the file (the
// folder, feed.json, the barrier) is ErrShowRefused, the file's own not.
// Allowed: with every look answering the episode opens. Not parallel: it
// swaps podcastLstat.
func TestOpenPodcastReadsAShareFault(t *testing.T) {
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	audio := filepath.Join(lib.Root, filepath.FromSlash(ep.Path))
	writePodcastFixture(t, audio, "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	was := podcastLstat
	t.Cleanup(func() { podcastLstat = was })
	for _, at := range []string{dir, filepath.Join(dir, "feed.json"), filepath.Join(dir, ".audiosilo"), audio} {
		for _, c := range []struct {
			errno  syscall.Errno
			gaveUp bool
		}{{syscall.EIO, true}, {syscall.ESTALE, true}, {syscall.EACCES, false}, {syscall.ENOENT, false}} {
			if at == filepath.Join(dir, ".audiosilo") && c.errno == syscall.ENOENT {
				continue // no barrier folder: published
			}
			podcastLstat = func(p string) (os.FileInfo, error) {
				if p == at {
					return nil, &fs.PathError{Op: "lstat", Path: p, Err: c.errno}
				}
				return was(p)
			}
			opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
			if opened != nil {
				opened.Close()
				t.Errorf("%s failing %v: opened", at, c.errno)
			}
			if c.gaveUp && (!metadata.GaveUp(err) || errors.Is(err, ErrNotIndexable)) {
				t.Errorf("%s failing %v: %v, want a look given up on", at, c.errno, err)
			}
			if !c.gaveUp && !errors.Is(err, ErrNotIndexable) {
				t.Errorf("%s failing %v: %v, want ErrNotIndexable", at, c.errno, err)
			}
			if refused := errors.Is(err, ErrEpisodeRefused); refused != (c.errno == syscall.EACCES) {
				t.Errorf("%s failing %v: %v, refused for good %v, want it only for the permission", at, c.errno, err, refused)
			}
			// Above the file (the folder, feed.json, the barrier) a refusal
			// is the show's, which no new download mends; the file's own is
			// not.
			if show := errors.Is(err, ErrShowRefused); show != (c.errno == syscall.EACCES && at != audio) {
				t.Errorf("%s failing %v: %v, the show refused %v, want it for a permission above the file only", at, c.errno, err, show)
			}
		}
	}
	podcastLstat = was
	opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
	if err != nil {
		t.Fatalf("every look answering: %v", err)
	}
	opened.Close()
}

// TestShowRefusedForAnEpisodeNotDownloaded: ShowRefused makes OpenPodcast's
// looks above the episode's file for an episode the library does not hold
// yet, which OpenPodcast answers absent before it looks: a permission on the
// show's folder, its feed.json or the publish barrier is ErrShowRefused, and
// the folder unreadable on the disk too (chmod 000, read through the root).
// Denied the refusal: a share's fault a retry may get past, the folder not
// there, a readable show. Not parallel: it swaps podcastLstat.
func TestShowRefusedForAnEpisodeNotDownloaded(t *testing.T) {
	ctx, _, lib, _, f, dir := podcastDiskFixture(t)
	rel := f.Episodes[2].Path
	if err := ShowRefused(ctx, *lib, rel, time.Second); err != nil {
		t.Fatalf("a readable show: %v", err)
	}
	was := podcastLstat
	t.Cleanup(func() { podcastLstat = was })
	for _, at := range []string{dir, filepath.Join(dir, "feed.json"), filepath.Join(dir, ".audiosilo")} {
		for _, errno := range []syscall.Errno{syscall.EACCES, syscall.EIO, syscall.ENOENT} {
			podcastLstat = func(p string) (os.FileInfo, error) {
				if p == at {
					return nil, &fs.PathError{Op: "lstat", Path: p, Err: errno}
				}
				return was(p)
			}
			err := ShowRefused(ctx, *lib, rel, time.Second)
			if refused := errors.Is(err, ErrShowRefused); refused != (errno == syscall.EACCES) || (err != nil) != refused {
				t.Errorf("%s failing %v: %v, want ErrShowRefused for the permission alone", at, errno, err)
			}
		}
	}
	podcastLstat = was
	if err := ShowRefused(ctx, *lib, "Another Show/2026-10-17-c0d697452a.mp3", time.Second); err != nil {
		t.Errorf("a show folder not there yet: %v, want nil (a fetch makes it)", err)
	}
	if os.Geteuid() == 0 {
		return // root reads a folder whatever its mode
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := ShowRefused(ctx, *lib, rel, time.Second); !errors.Is(err, ErrShowRefused) {
		t.Errorf("a show folder chmod 000: %v, want ErrShowRefused", err)
	}
}

// TestPodcastScanKeepsAShowOnAShareFault: a podcast scan whose look at a show
// (its folder, an episode's publish barrier) fails as a share does that a
// retry may get past is partial, the show and its episodes kept as indexed,
// never pruned or unpublished; one whose root listing is given up on (a share
// that stopped answering) is the library unavailable at once, nothing pruned.
// Allowed: the same scan with every look answering keeps the show. Not
// parallel: it swaps podcastLstat and the mount lookup.
func TestPodcastScanKeepsAShowOnAShareFault(t *testing.T) {
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	writePodcastFixture(t, filepath.Join(lib.Root, filepath.FromSlash(ep.Path)), "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	downloaded := func() bool {
		t.Helper()
		b, err := cat.GetBookByPath(ctx, lib.ID, ep.Path)
		return err == nil && b.Episode != nil && b.Episode.Downloaded
	}
	if !downloaded() {
		t.Fatal("the fixture's episode is not indexed as downloaded")
	}
	was := podcastLstat
	t.Cleanup(func() { podcastLstat = was })
	for _, at := range []string{dir, filepath.Join(dir, ".audiosilo")} {
		podcastLstat = func(p string) (os.FileInfo, error) {
			if p == at {
				return nil, &fs.PathError{Op: "lstat", Path: p, Err: syscall.EIO}
			}
			return was(p)
		}
		// force: a show whose feed.json is unchanged is skipped before its barrier.
		if _, _, err := s.IndexPodcastPath(ctx, *lib, "Fixture Show"); !metadata.GaveUp(err) {
			t.Errorf("the Bridge's rescan with %s failing EIO: %v, want a look given up on", at, err)
		}
		res, err := s.Scan(ctx, *lib)
		if at == dir && (err != nil || !res.Partial) {
			t.Errorf("a scan with the show's folder failing EIO: %+v %v, want a partial run", res, err)
		}
		if !downloaded() {
			t.Errorf("with %s failing EIO the episode is no longer downloaded", at)
		}
	}
	podcastLstat = was

	release := metadata.HangMountForTests(lib.Root)
	t.Cleanup(release)
	began := time.Now()
	if _, err := s.Scan(ctx, *lib); !errors.Is(err, ErrLibraryUnavailable) || time.Since(began) > 10*time.Second {
		t.Errorf("a scan of a root on a share that stopped answering: %v after %v, want ErrLibraryUnavailable at once", err, time.Since(began))
	}
	release()
	if !downloaded() {
		t.Error("the episode is gone after a scan of a hung root")
	}
	if _, err := s.Scan(ctx, *lib); err != nil || !downloaded() {
		t.Errorf("a scan with every look answering: %v, downloaded %v", err, downloaded())
	}
}

// TestPodcastScanBoundsAShowsLooks: a podcast scan's looks at a show (its
// folder, feed.json, its listing) are bounded as the root's listing is: with
// the show's folder on a share that stopped answering after the root listed,
// the scan gives each look up, ends partial with the show and its episode
// kept as indexed, and the Bridge's rescan hears a look given up on; neither
// is held. Allowed: once the share answers, the same scan reads the show.
// Not parallel: it swaps the mount lookup.
func TestPodcastScanBoundsAShowsLooks(t *testing.T) {
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	writePodcastFixture(t, filepath.Join(lib.Root, filepath.FromSlash(ep.Path)), "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	downloaded := func() bool {
		t.Helper()
		b, err := cat.GetBookByPath(ctx, lib.ID, ep.Path)
		return err == nil && b.Episode != nil && b.Episode.Downloaded
	}
	if !downloaded() {
		t.Fatal("the fixture's episode is not indexed as downloaded")
	}
	s.probeTimeout = 200 * time.Millisecond
	release := metadata.HoldOpensForTests(dir)
	t.Cleanup(release)
	began := time.Now()
	res, err := s.Scan(ctx, *lib)
	if err != nil || !res.Partial || time.Since(began) > 10*time.Second {
		t.Errorf("a scan with the show's folder not answering: %+v %v after %v, want a partial run, not held", res, err, time.Since(began))
	}
	began = time.Now()
	if _, _, err := s.IndexPodcastPath(ctx, *lib, "Fixture Show"); !metadata.GaveUp(err) || time.Since(began) > 10*time.Second {
		t.Errorf("the Bridge's rescan with the show's folder not answering: %v after %v, want a look given up on", err, time.Since(began))
	}
	if !downloaded() {
		t.Error("the episode is no longer downloaded after a scan that could not look at its show")
	}
	release()
	if res, err := s.Scan(ctx, *lib); err != nil || res.Partial || !downloaded() {
		t.Errorf("a scan with every look answering: %+v %v, downloaded %v", res, err, downloaded())
	}
}

// TestOpenPodcastThroughTheRootAfterASwap (security): an episode's feed.json
// and file are read through the library's root, so a show folder swapped,
// after the folder's own look, for a link to a copy out of the library (its
// feed.json and episode at the size and times the pin names, so every look by
// path, which follows the link, matches) is never read: the episode is not
// opened. Plain reads by path followed the link and opened the copy's file.
// Allowed: in place the episode opens to its own bytes. Not parallel: it
// swaps podcastLstat.
func TestOpenPodcastThroughTheRootAfterASwap(t *testing.T) {
	ctx, cat, lib, s, f, dir := podcastDiskFixture(t)
	ep := f.Episodes[0]
	audio := filepath.Join(lib.Root, filepath.FromSlash(ep.Path))
	writePodcastFixture(t, audio, "chap-v24-reverse.mp3")
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	inside, err := os.ReadFile(audio)
	if err != nil {
		t.Fatal(err)
	}
	read := func() ([]byte, error) {
		t.Helper()
		opened, _, err := OpenPodcast(ctx, cat, *lib, ep.Path)
		if err != nil {
			return nil, err
		}
		defer opened.Close()
		return io.ReadAll(opened)
	}
	if got, err := read(); err != nil || !bytes.Equal(got, inside) {
		t.Fatalf("the episode in place = %v, want its own bytes", err)
	}

	// A copy out of the library: the same feed.json, an episode of the same
	// size whose bytes are not the library's, each at the original's times.
	copyDir := filepath.Join(t.TempDir(), "Fixture Show")
	if err := os.MkdirAll(filepath.Join(copyDir, ".audiosilo"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"feed.json":          nil,
		filepath.Base(audio): bytes.Repeat([]byte("x"), len(inside)),
	} {
		src := filepath.Join(dir, name)
		if body == nil {
			if body, err = os.ReadFile(src); err != nil {
				t.Fatal(err)
			}
		}
		st, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(copyDir, name)
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			t.Fatal(err)
		}
		setFeedTime(t, dst, st.ModTime())
	}
	was := podcastLstat
	t.Cleanup(func() { podcastLstat = was })
	swapped := false
	podcastLstat = func(p string) (os.FileInfo, error) {
		if p == filepath.Join(dir, "feed.json") && !swapped {
			// The folder's look answered: now it becomes the link.
			swapped = true
			swapForLink(t, dir, copyDir)
		}
		return was(p)
	}
	got, err := read()
	if !swapped {
		t.Fatal("the swap never ran")
	}
	if err == nil || bytes.Equal(got, bytes.Repeat([]byte("x"), len(inside))) {
		t.Fatalf("the episode after its show folder became a link out of the library = %d bytes, %v; want it refused", len(got), err)
	}
}

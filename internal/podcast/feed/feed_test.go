package feed

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadFeedFixture(t *testing.T) {
	before := time.Now()
	f, err := ReadFile("../testdata", "../testdata/feed-v1.json", "Example Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.ReadAt.Before(before) || f.ReadAt.After(time.Now()) {
		t.Fatalf("read at %v, not during the read", f.ReadAt)
	}
	if len(f.Episodes) != 3 || f.Episodes[2].Title != "Untitled episode" || f.Episodes[1].Fetch.DurationMS != 1512345 {
		t.Fatalf("fixture: %+v", f)
	}
	// Unknown keys remain forward-compatible; bad paths never become books.
	raw, err := os.ReadFile("../testdata/feed-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*File)
		want   int
		denied bool
	}{
		{"valid", func(f *File) {}, 3, false},
		{"version", func(f *File) { f.Version = 2 }, 0, true},
		{"paths", func(f *File) {
			for i, p := range []string{"../outside.mp3", "Example Show/.secret.mp3", "Example Show/a/../b.mp3"} {
				f.Episodes[i].Path = p
			}
		}, 0, false},
		{"duplicate", func(f *File) { f.Episodes[1].Path = f.Episodes[0].Path }, 2, false},
		{"private", func(f *File) { f.Private = true }, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f File
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&f)
			data, _ := json.Marshal(f)
			filename := filepath.Join(t.TempDir(), "feed.json")
			if err := os.WriteFile(filename, data, 0600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			got, err := ReadFile(filepath.Dir(filename), filename, "Example Show", slog.New(slog.NewTextHandler(&logs, nil)))
			if tc.denied {
				if err == nil {
					t.Fatal("accepted unsupported feed")
				}
				return
			}
			if err != nil || len(got.Episodes) != tc.want {
				t.Fatalf("%v %v", got, err)
			}
			if got.Private && (got.FeedURL != "" || got.Show.Link != "" || got.Episodes[0].Enclosure.URL != "" || len(got.Episodes[0].Transcripts) != 0) {
				t.Fatal("private URLs retained")
			}
			if strings.Contains(logs.String(), "https://") {
				t.Fatal("feed data in log")
			}
		})
	}
	filename := filepath.Join(t.TempDir(), "linked.json")
	abs, _ := filepath.Abs("../testdata/feed-v1.json")
	if err := os.Symlink(abs, filename); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(filepath.Dir(filename), filename, "Example Show", nil); err == nil {
		t.Fatal("symlink accepted")
	}
}

// A show folder renamed by hand keeps its feed.json, whose episode paths name
// the old folder until the Bridge rewrites it: a downloaded episode whose file
// the show's own folder holds at its fetch's size is read there; one not
// downloaded, one with no file there or a file of another size (still being
// copied in), a link, a deeper path or a traversal is still skipped.
func TestReadFeedRerootsARenamedFolder(t *testing.T) {
	parent, dir, f, ep := rerootBed(t)
	for _, name := range []string{"a.mp3", "deep.mp3", "unfetched.mp3", "deleted.mp3"} {
		writeSized(t, filepath.Join(dir, name), ep.Fetch.Bytes)
	}
	writeSized(t, filepath.Join(dir, "short.mp3"), ep.Fetch.Bytes-1)
	if err := os.WriteFile(filepath.Join(parent, "outside.mp3"), []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parent, "outside.mp3"), filepath.Join(dir, "linked.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "folder.mp3"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.Episodes = nil
	for _, p := range []string{
		"Example Show/a.mp3",          // read as Renamed/a.mp3
		"Example Show/a.mp3",          // the same file again: once
		"Example Show/missing.mp3",    // no file in the folder
		"Example Show/short.mp3",      // a file of another size than its fetch's
		"Example Show/linked.mp3",     // a link, no regular file
		"Example Show/folder.mp3",     // a folder, no regular file
		"Example Show/x/deep.mp3",     // more than one leading segment
		"../outside.mp3",              // a traversal
		".hidden/a.mp3",               // no show folder name
		"Example Show/.a.mp3",         // a hidden file name
		"Example Show/../outside.mp3", // a traversal in the file name
		"Example Show/unfetched.mp3",  // not downloaded: no fetch
		"Example Show/deleted.mp3",    // not downloaded: its fetch deleted
	} {
		e := ep
		e.Path = p
		switch p {
		case "Example Show/unfetched.mp3":
			e.Fetch = nil
		case "Example Show/deleted.mp3":
			deleted := *e.Fetch
			deleted.DeletedAt = "2026-10-21T07:00:00Z"
			e.Fetch = &deleted
		}
		f.Episodes = append(f.Episodes, e)
	}
	got := readRerooted(t, dir, "Renamed", f)
	if len(got.Episodes) != 1 || got.Episodes[0].Path != "Renamed/a.mp3" || !got.Rerooted {
		t.Fatalf("episodes %+v", got.Episodes)
	}
}

// Denied: a copy beside its original (the folder its feed.json names holds the
// same show) lists none of the original's episodes, nor does one beside a
// folder whose feed.json can't be told; a folder the name now holds another
// show in (names swapped by hand) is no original.
func TestReadFeedRerootsNoCopy(t *testing.T) {
	parent, dir, f, ep := rerootBed(t)
	writeSized(t, filepath.Join(dir, "a.mp3"), ep.Fetch.Bytes)
	ep.Path = "Example Show/a.mp3"
	f.Episodes = []Episode{ep}
	original := filepath.Join(parent, "Example Show")
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		feed string
		want int
	}{
		{`{"version":1,"show_id":"` + f.ShowID + `"}`, 0}, // the original
		{`{"version":1,`, 0},                              // can't be told
		{`{"version":1,"show_id":"0123456789abcdef"}`, 1}, // another show
	} {
		if err := os.WriteFile(filepath.Join(original, "feed.json"), []byte(c.feed), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readRerooted(t, dir, "Renamed", f); len(got.Episodes) != c.want {
			t.Fatalf("%s: episodes %+v", c.feed, got.Episodes)
		}
	}
}

// A show folder spelled in the other Unicode normal form than its feed.json's
// paths (renamed by hand from NFC to NFD, or kept so by the disk) is the same
// folder: every episode is read at the folder's spelling, a download or not.
// A disk that opens the NFC spelling too (APFS) answers it as this folder,
// with nothing else to look at; on one that keeps the two apart the NFC name
// is gone, which a later read may find otherwise (Rerooted). Denied: a name
// that differs in more than its normal form, and a deeper path.
func TestReadFeedReadsAnotherSpellingOfItsFolder(t *testing.T) {
	nfc, nfd := "Caf\u00e9 Show", "Cafe\u0301 Show"
	parent, dir, f, ep := rerootBed(t)
	moved := filepath.Join(parent, nfd)
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	_, err := os.Lstat(filepath.Join(parent, nfc))
	normalizing := err == nil
	f.Episodes = nil
	for _, c := range []struct {
		path  string
		fetch bool
	}{
		{nfc + "/fetched.mp3", true},
		{nfc + "/unfetched.mp3", false},
		{"Cafe Show/other.mp3", false},
		{nfc + "/x/deep.mp3", false},
	} {
		e := ep
		e.Path = c.path
		if !c.fetch {
			e.Fetch = nil
		}
		f.Episodes = append(f.Episodes, e)
	}
	got := readRerooted(t, moved, nfd, f)
	if len(got.Episodes) != 2 || got.Episodes[0].Path != nfd+"/fetched.mp3" || got.Episodes[1].Path != nfd+"/unfetched.mp3" || got.Rerooted == normalizing {
		t.Fatalf("episodes %+v rerooted %v", got.Episodes, got.Rerooted)
	}
}

// A show folder renamed by hand only in case is the same folder where the
// disk ignores case (APFS): every episode is read at the folder's spelling, a
// download or not, with nothing else looked at. Where the disk keeps the two
// apart the old name is another folder, never merged: only the downloaded
// episode is read in the show's folder, as for any other name (rerooter).
func TestReadFeedReadsACaseOnlyRename(t *testing.T) {
	parent, dir, f, ep := rerootBed(t)
	moved := filepath.Join(parent, "renamed")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	_, err := os.Lstat(dir)
	insensitive := err == nil
	writeSized(t, filepath.Join(moved, "a.mp3"), ep.Fetch.Bytes)
	unfetched := ep
	ep.Path, unfetched.Path, unfetched.Fetch = "Renamed/a.mp3", "Renamed/b.mp3", nil
	f.Episodes = []Episode{ep, unfetched}
	got := readRerooted(t, moved, "renamed", f)
	want := []string{"renamed/a.mp3"}
	if insensitive {
		want = append(want, "renamed/b.mp3")
	}
	if len(got.Episodes) != len(want) || got.Rerooted == insensitive {
		t.Fatalf("insensitive %v: episodes %+v rerooted %v", insensitive, got.Episodes, got.Rerooted)
	}
	for i, e := range got.Episodes {
		if e.Path != want[i] {
			t.Fatalf("insensitive %v: episode %d at %q", insensitive, i, e.Path)
		}
	}
	if insensitive {
		return
	}
	// Denied, where the two are apart: the other case holding the show too is
	// the original this folder is a copy of.
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feed.json"), []byte(`{"version":1,"show_id":"`+f.ShowID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readRerooted(t, moved, "renamed", f); len(got.Episodes) != 0 {
		t.Fatalf("beside its original: %+v", got.Episodes)
	}
}

// Denied: a folder in the other Unicode normal form beside the show's own (a
// disk that keeps the two apart) holding the same show, or one whose
// feed.json can't be told, is the original the show's folder is a copy of: no
// episode is read in the copy, downloaded or not. One holding another show is
// no original.
func TestReadFeedReadsNoCopyInAnotherSpelling(t *testing.T) {
	nfc, nfd := "Caf\u00e9 Show", "Cafe\u0301 Show"
	parent, dir, f, ep := rerootBed(t)
	moved := filepath.Join(parent, nfd)
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(parent, nfc)
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Skipf("the disk does not keep NFC and NFD names apart: %v", err)
	}
	writeSized(t, filepath.Join(moved, "a.mp3"), ep.Fetch.Bytes)
	unfetched := ep
	ep.Path, unfetched.Path, unfetched.Fetch = nfc+"/a.mp3", nfc+"/b.mp3", nil
	f.Episodes = []Episode{ep, unfetched}
	for _, c := range []struct {
		feed string
		want int
	}{
		{`{"version":1,"show_id":"` + f.ShowID + `"}`, 0}, // the original
		{`{"version":1,`, 0},                              // can't be told
		{`{"version":1,"show_id":"0123456789abcdef"}`, 2}, // another show
	} {
		if err := os.WriteFile(filepath.Join(original, "feed.json"), []byte(c.feed), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readRerooted(t, moved, nfd, f); len(got.Episodes) != c.want || !got.Rerooted {
			t.Fatalf("%s: episodes %+v rerooted %v", c.feed, got.Episodes, got.Rerooted)
		}
	}
}

// A look at the folder an episode path names that fails as a share does when
// a retry may get past it (EIO, ESTALE) is no answer: the episode is not read
// in the show's folder this time, and the read is Unsure (so its caller keeps
// what it knows). Denied: a refusal that stays (EACCES) is an answer, the
// folder counted as holding the show, never Unsure. Read again once the fault
// is gone, the episode is listed. The same holds for a name in the other
// Unicode form (inShow's look).
func TestReadFeedTellsAFaultFromAHeldFolder(t *testing.T) {
	nfc, nfd := "Caf\u00e9 Show", "Cafe\u0301 Show"
	parent, dir, f, ep := rerootBed(t)
	moved := filepath.Join(parent, nfd)
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(moved, "a.mp3"), ep.Fetch.Bytes)
	unfetched := ep
	ep.Path, unfetched.Path, unfetched.Fetch = "Example Show/a.mp3", nfc+"/b.mp3", nil
	f.Episodes = []Episode{ep, unfetched}
	folders := []string{filepath.Join(parent, "Example Show"), filepath.Join(parent, nfc)}
	t.Cleanup(func() {
		for _, folder := range folders {
			FaultForTests(folder, nil)
		}
	})
	for _, c := range []struct {
		name   string
		err    error
		want   []string
		unsure bool
	}{
		{"EIO", syscall.EIO, nil, true},
		{"ESTALE", syscall.ESTALE, nil, true},
		// Windows' ERROR_BAD_NETPATH (a share whose server is down), which Go
		// reads as fs.ErrNotExist and metadata.TransientFault as a fault for
		// now: no folder gone (metadata.NotExist), so no episode is read in
		// the show's folder as if it had been renamed away.
		{"not there, from a dropped share", errors.Join(fs.ErrNotExist, syscall.ESTALE), nil, true},
		{"EACCES", syscall.EACCES, nil, false},
		// Allowed: no fault, the folders gone (a real deletion: ENOENT).
		{"none", nil, []string{nfd + "/a.mp3", nfd + "/b.mp3"}, false},
	} {
		for _, folder := range folders {
			FaultForTests(folder, c.err)
		}
		got := readRerooted(t, moved, nfd, f)
		var paths []string
		for _, e := range got.Episodes {
			paths = append(paths, e.Path)
		}
		if !slices.Equal(paths, c.want) || got.Unsure != c.unsure || !got.Rerooted {
			t.Errorf("%s: episodes %v unsure %v rerooted %v", c.name, paths, got.Unsure, got.Rerooted)
		}
	}
}

// rerootBed is a show folder "Renamed" in parent, and the fixture feed with
// its downloaded episode (ep, a template) for the caller to list.
func rerootBed(t *testing.T) (parent, dir string, f File, ep Episode) {
	t.Helper()
	raw, err := os.ReadFile("../testdata/feed-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	parent = t.TempDir()
	dir = filepath.Join(parent, "Renamed")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ep = f.Episodes[1]
	if !ValidFetch(ep.Fetch) || ep.Fetch.DeletedAt != "" {
		t.Fatal("the fixture's second episode is not downloaded")
	}
	return parent, dir, f, ep
}

// writeSized writes a file of size bytes (a hole: no audio is read).
func writeSized(t *testing.T, name string, size int64) {
	t.Helper()
	if err := os.WriteFile(name, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(name, size); err != nil {
		t.Fatal(err)
	}
}

// readRerooted writes f as dir's feed.json and reads it as show, through the
// library's root (the folder holding dir), as a scan reads it.
func readRerooted(t *testing.T, dir, show string, f File) *File {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "feed.json")
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(filepath.Dir(dir), filename, show, nil)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestReadFileThroughTheRoot (security): a show's feed.json is read through
// the library's root (metadata.ReadRegularFile), so a show folder that is a
// link out of the library (one swapped in since the scan's look at it) is
// never read, where a look and an open by path, both following the folder's
// link, found one regular file and read it. Allowed: the feed in place, and
// through a link that stays in the library.
func TestReadFileThroughTheRoot(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../testdata/feed-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	root, outside := t.TempDir(), t.TempDir()
	for _, dir := range []string{filepath.Join(root, "Example Show"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "feed.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadFile(root, filepath.Join(root, "Example Show", "feed.json"), "Example Show", nil); err != nil {
		t.Fatalf("a feed.json in place = %v, want it read", err)
	}
	if err := os.Symlink("Example Show", filepath.Join(root, "Again")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if _, err := ReadFile(root, filepath.Join(root, "Again", "feed.json"), "Example Show", nil); err != nil {
		t.Fatalf("a feed.json through a link inside the library = %v, want it read", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "Away")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(root, filepath.Join(root, "Away", "feed.json"), "Example Show", nil); err == nil {
		t.Fatal("a feed.json through a show folder linked out of the library was read")
	}
}

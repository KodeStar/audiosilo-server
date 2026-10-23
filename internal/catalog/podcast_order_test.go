package catalog

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

// undatedSerial indexes a LibriVox-like serial: ten undated episodes numbered
// S1E1..S1E5 then S2E1..S2E5 in the feed, stored under the Bridge's hash names
// (which sort in no useful order) and listed in feed.json shuffled. It returns
// each episode's path in feed order.
func undatedSerial(t *testing.T) (*Catalog, *Library, []string, *feed.File) {
	t.Helper()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Podcasts", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	f, err := feed.ReadFile("../podcast/testdata/phase2c2/feed-fetch-v1.json", "Fixture Show", nil)
	if err != nil {
		t.Fatal(err)
	}
	template := f.Episodes[0]
	paths := make([]string, 10)
	var eps []feed.Episode
	for i := range 10 {
		sum := sha256.Sum256([]byte(fmt.Sprint("episode", i)))
		e := template
		e.ID = hex.EncodeToString(sum[:8])
		e.GUID = e.ID
		e.Path = "Fixture Show/undated-" + hex.EncodeToString(sum[8:13]) + ".mp3"
		e.Title = fmt.Sprintf("E%d", i+1)
		e.Published = ""
		season, number := 1+i/5, 1+i%5
		e.Season, e.Episode = &season, &number
		e.Fetch = nil
		paths[i] = e.Path
		eps = append(eps, e)
	}
	// The Bridge's order is not the feed's.
	f.Episodes = nil
	for _, i := range []int{6, 2, 3, 1, 0, 8, 4, 7, 5, 9} {
		f.Episodes = append(f.Episodes, eps[i])
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Fixture Show", "s", "", "", f, nil, true); err != nil {
		t.Fatal(err)
	}
	return c, lib, paths, f
}

func TestPodcastEpisodeOrderUndatedSerial(t *testing.T) {
	c, lib, paths, _ := undatedSerial(t)
	ctx := t.Context()
	all := Scope{LibraryID: lib.ID, AllowAll: true}
	list := func(order string) []string {
		t.Helper()
		var got []string
		cursor := ""
		for {
			page, err := c.PodcastEpisodesWithin(ctx, lib.ID, "Fixture Show", order, 3, cursor, all)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range page.Episodes {
				got = append(got, e.Path)
			}
			if page.NextCursor == nil {
				return got
			}
			cursor = *page.NextCursor
		}
	}
	if got := list("oldest"); strings.Join(got, ",") != strings.Join(paths, ",") {
		t.Fatalf("oldest first:\n%v\nwant\n%v", got, paths)
	}
	newest := list("newest")
	for i, p := range newest {
		if p != paths[len(paths)-1-i] {
			t.Fatalf("newest first: %v", newest)
		}
	}
	// A cursor from before this order (four strings) is refused, not misread.
	legacy := base64.RawURLEncoding.EncodeToString([]byte(`["Fixture Show","oldest","","` + paths[2] + `"]`))
	if _, err := c.PodcastEpisodesWithin(ctx, lib.ID, "Fixture Show", "oldest", 3, legacy, all); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("legacy cursor = %v", err)
	}
	// Keep 3 downloads the last three of the feed.
	show, err := c.PodcastShow(ctx, lib.ID, "Fixture Show", 0)
	if err != nil {
		t.Fatal(err)
	}
	cands, err := c.PodcastFetchCandidates(ctx, PodcastWorkShow{Ref: Ref{LibraryID: lib.ID, Path: "Fixture Show"}, ShowID: show.ShowID, Policy: Policy{Mode: "keep", Keep: 3}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range cands {
		got = append(got, e.Path)
	}
	if strings.Join(got, ",") != strings.Join([]string{paths[9], paths[8], paths[7]}, ",") {
		t.Fatalf("keep 3 fetches %v", got)
	}
	// Archive starts at the first.
	cands, err = c.PodcastFetchCandidates(ctx, PodcastWorkShow{Ref: Ref{LibraryID: lib.ID, Path: "Fixture Show"}, ShowID: show.ShowID, Policy: Policy{Mode: "archive"}})
	if err != nil || len(cands) != 10 || cands[0].Path != paths[0] || cands[9].Path != paths[9] {
		t.Fatalf("archive %+v %v", cands, err)
	}
}

func TestNextPodcastEpisode(t *testing.T) {
	c, lib, paths, _ := undatedSerial(t)
	ctx := t.Context()
	all := Scope{LibraryID: lib.ID, AllowAll: true}
	for i, p := range paths[:9] {
		next, err := c.NextPodcastEpisode(ctx, lib.ID, p, all)
		if err != nil || next == nil || next.RelPath != paths[i+1] {
			t.Fatalf("after E%d: %+v %v", i+1, next, err)
		}
	}
	if next, err := c.NextPodcastEpisode(ctx, lib.ID, paths[9], all); err != nil || next != nil {
		t.Fatalf("after the last: %+v %v", next, err)
	}
	// Out of scope is passed over; a path that is no episode has no next.
	narrow := Scope{LibraryID: lib.ID, Paths: []string{paths[0], paths[2]}}
	if next, err := c.NextPodcastEpisode(ctx, lib.ID, paths[0], narrow); err != nil || next == nil || next.RelPath != paths[2] {
		t.Fatalf("narrow scope: %+v %v", next, err)
	}
	if next, err := c.NextPodcastEpisode(ctx, lib.ID, "Fixture Show/none.mp3", all); err != nil || next != nil {
		t.Fatalf("no episode: %+v %v", next, err)
	}
}

// The keep window (Keep 3) of an undated serial, every episode on disk, is
// its last three episodes by number: E8..E10 are inside, E7 and earlier out.
func TestPodcastKeepWindowUndatedSerial(t *testing.T) {
	c, lib, paths, f := undatedSerial(t)
	ctx := t.Context()
	present := map[string]PodcastFile{}
	for i := range f.Episodes {
		fetch := feed.Fetch{Generation: 1, SHA256: strings.Repeat("ab", 32), Bytes: 4, DurationMS: 1000, ContentType: "audio/mpeg", FetchedAt: "2026-10-21T08:00:00Z", Codec: "mp3"}
		f.Episodes[i].Fetch = &fetch
		present[path.Base(f.Episodes[i].Path)] = PodcastFile{Size: fetch.Bytes}
	}
	if _, err := c.IndexPodcastShow(ctx, lib.ID, "Fixture Show", "fetched", "", "", f, present, true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPodcastPolicy(ctx, lib.ID, "Fixture Show", f.ShowID, &Policy{Mode: "keep", Keep: 3, Days: 7}); err != nil {
		t.Fatal(err)
	}
	for i, p := range paths {
		check, err := c.PodcastRetentionCheck(ctx, lib.ID, RetentionRequest{ShowID: f.ShowID, Path: p, Generation: 1, SHA256: strings.Repeat("ab", 32), Bytes: 4})
		if err != nil {
			t.Fatalf("E%d: %v", i+1, err)
		}
		if want := i < 7; check.OutsideKeepWindow != want {
			t.Fatalf("E%d outside the keep window = %t", i+1, check.OutsideKeepWindow)
		}
	}
}

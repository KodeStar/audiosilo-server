package catalog

import (
	"fmt"
	"testing"
)

// BenchmarkPodcastWorkShows times the podcast scheduler's list of shows to
// fetch and prune for (PodcastWorkShows, at every pass): 100 shows in one
// library, each followed by the same 20 listeners, of whom only the last
// still holds the library (the others' share was taken back), so each show's
// followers are checked for access one by one (server#180).
//
//	go test ./internal/catalog -run '^$' -bench BenchmarkPodcastWorkShows
func BenchmarkPodcastWorkShows(b *testing.B) {
	c, ctx := newTestCatalog(b)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: b.TempDir(), Kind: KindPodcasts})
	if err != nil {
		b.Fatal(err)
	}
	const shows, followers = 100, 20
	users := make([]int64, followers)
	for i := range users {
		users[i] = seedUserNamed(b, c, ctx, fmt.Sprintf("listener%02d", i))
	}
	if err := c.GrantWholeLibrary(ctx, users[followers-1], lib.ID); err != nil {
		b.Fatal(err)
	}
	for i := range shows {
		show := fmt.Sprintf("Show %03d", i)
		f := indexShowAt(b, c, ctx, lib.ID, show, fmt.Sprintf("%016x", i))
		for _, u := range users {
			if err := c.FollowPodcast(ctx, u, lib.ID, show, f.ShowID); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		got, err := c.PodcastWorkShows(ctx)
		if err != nil || len(got) != shows {
			b.Fatalf("%d shows: %v", len(got), err)
		}
	}
}

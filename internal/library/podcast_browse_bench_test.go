package library

import (
	"fmt"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// BenchmarkBrowsePodcasts times one page (200 entries) of a show's folder
// listing (GET /fs in a podcast library) for a show of 5,000 indexed
// episodes, its first page and one deep in the list (server#180).
//
//	go test ./internal/library -run '^$' -bench BenchmarkBrowsePodcasts
func BenchmarkBrowsePodcasts(b *testing.B) {
	ctx, cat, lib, s, f, _ := podcastDiskFixture(b)
	const episodes = 5000
	tmpl := f.Episodes[0]
	f.Episodes = f.Episodes[:0]
	for i := range episodes {
		ep := tmpl
		ep.ID = fmt.Sprintf("%016x", 0xabc0000+i)
		ep.GUID = "bench-" + ep.ID
		ep.Path = fmt.Sprintf("Fixture Show/2020-01-01-%s.mp3", ep.ID)
		ep.Title = fmt.Sprintf("Episode %05d", i)
		ep.Published = fmt.Sprintf("2020-01-01T%02d:%02d:%02dZ", i/3600%24, i/60%60, i%60)
		ep.Fetch = nil
		f.Episodes = append(f.Episodes, ep)
	}
	if _, err := cat.IndexPodcastShow(ctx, lib.ID, "Fixture Show", "bench", "", "", f, nil, true, true); err != nil {
		b.Fatal(err)
	}
	scope := catalog.Scope{LibraryID: lib.ID, AllowAll: true}
	for _, tc := range []struct {
		name   string
		offset int
	}{{"first", 0}, {"deep", 4000}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				out, err := s.BrowsePodcasts(ctx, *lib, scope, 0, "Fixture Show", tc.offset, 200)
				if err != nil || out.Total != episodes || len(out.Entries) != 200 {
					b.Fatalf("listing: %v %+v", err, out)
				}
			}
		})
	}
}

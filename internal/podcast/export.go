package podcast

import (
	"cmp"
	"slices"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/podcast/opml"
)

// Export is what the OPML export writes and its omission counts.
type Export struct {
	Entries            []opml.Entry
	Exported           int // unique public feeds written
	OmittedPrivate     int // followed, visible shows left out as private
	OmittedUnavailable int // followed, visible shows with no usable public URL
}

// BuildExport decides the export from the caller's followed, visible shows: a
// private show is left out whoever asks (no name, hash or host of it is
// written), and so is a public one whose URL now looks private; a show with no
// URL, or one that doesn't normalize stably, is unavailable. Omissions count
// every such follow, before duplicates are merged. One public URL followed in
// several libraries is written once, its title from the lowest library id then
// show path (bytes); entries are sorted by URL bytes.
func BuildExport(rows []catalog.PodcastExportRow) Export {
	rows = slices.Clone(rows)
	slices.SortFunc(rows, func(a, b catalog.PodcastExportRow) int {
		return cmp.Or(cmp.Compare(a.LibraryID, b.LibraryID), cmp.Compare(a.Path, b.Path))
	})
	out := Export{Entries: []opml.Entry{}}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Private {
			out.OmittedPrivate++
			continue
		}
		if r.FeedURL == "" {
			out.OmittedUnavailable++
			continue
		}
		norm, err := NormalizeFollowURL(r.FeedURL)
		if err != nil {
			out.OmittedUnavailable++
			continue
		}
		if feed.IsPrivateFeedURL(norm) {
			out.OmittedPrivate++
			continue
		}
		if seen[norm] {
			continue
		}
		seen[norm] = true
		out.Entries = append(out.Entries, opml.Entry{Title: r.Title, URL: norm})
	}
	slices.SortFunc(out.Entries, func(a, b opml.Entry) int { return cmp.Compare(a.URL, b.URL) })
	out.Exported = len(out.Entries)
	return out
}

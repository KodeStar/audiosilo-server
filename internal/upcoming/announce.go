package upcoming

import (
	"context"

	"github.com/kodestar/audiosilo-server/internal/notify"
)

// Announce is the job's book_released, sent to n: one event naming the first
// few releases as "Title (Series #position)" (Release.Label), the rest counted.
// A nil n or no release sends nothing. The server hands it to Options.Announce
// with its live notifications (api.go).
func Announce(ctx context.Context, n *notify.Service, releases []Release) {
	if n == nil || len(releases) == 0 {
		return
	}
	titles := make([]string, 0, maxReleaseTitles)
	ids := make([]string, 0, len(releases))
	for i, rel := range releases {
		ids = append(ids, rel.WorkID)
		if i < maxReleaseTitles {
			titles = append(titles, rel.Label())
		}
	}
	n.BooksReleased(ctx, len(releases), titles, "released:"+notify.SetDigest(ids))
}

// maxReleaseTitles is how many releases a book_released event names (the rest
// are counted). Its dedupe key is a digest of the run's work ids.
const maxReleaseTitles = 5

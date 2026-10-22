package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

// insertExportShow indexes a bare show row (no episodes): what the export
// reads.
func insertExportShow(t testing.TB, c *Catalog, ctx context.Context, lib int64, path, showID, title, url string, private bool) {
	t.Helper()
	if _, err := c.db.ExecContext(ctx, `INSERT INTO podcast_shows(library_id,show_path,show_id,title,author,description,link,language,show_type,explicit,private,feed_url,feed_host,feed_url_sha256,podcast_guid,cover_path,episode_count,latest_published,feed_sig,indexed_at)
		VALUES(?,?,?,?,'','','','','episodic',0,?,?,'','','','',0,NULL,'','t')`, lib, path, showID, title, private, url); err != nil {
		t.Fatal(err)
	}
}

func TestPodcastExport(t *testing.T) {
	c, ctx := newTestCatalog(t)
	user := seedUserNamed(t, c, ctx, "listener")
	other := seedUserNamed(t, c, ctx, "other")
	pods, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	more, err := c.CreateLibrary(ctx, Library{Name: "More", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	ungranted, err := c.CreateLibrary(ctx, Library{Name: "Elsewhere", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	insertExportShow(t, c, ctx, pods.ID, "B Show", "b", "B", "https://example.org/b.xml", false)
	insertExportShow(t, c, ctx, pods.ID, "A Show", "a", "A", "https://example.org/a.xml", false)
	insertExportShow(t, c, ctx, pods.ID, "Stale", "new-id", "Stale", "https://example.org/stale.xml", false)
	insertExportShow(t, c, ctx, pods.ID, "Unfollowed", "u", "Unfollowed", "https://example.org/u.xml", false)
	insertExportShow(t, c, ctx, pods.ID, "Theirs", "t", "Theirs", "https://example.org/t.xml", false)
	insertExportShow(t, c, ctx, pods.ID, "Private", "p", "Private", "", true)
	insertExportShow(t, c, ctx, more.ID, "Hidden", "h", "Hidden", "https://example.org/h.xml", false)
	insertExportShow(t, c, ctx, more.ID, "Shown", "s", "Shown", "https://example.org/s.xml", false)
	insertExportShow(t, c, ctx, ungranted.ID, "Revoked", "r", "Revoked", "https://example.org/r.xml", false)
	for _, f := range []struct {
		user      int64
		lib       int64
		path, sid string
	}{
		{user, pods.ID, "B Show", "b"}, {user, pods.ID, "A Show", "a"},
		{user, pods.ID, "Stale", "old-id"}, // a replaced show at the same path
		{other, pods.ID, "Theirs", "t"},
		{user, pods.ID, "Private", "p"},
		{user, more.ID, "Hidden", "h"}, {user, more.ID, "Shown", "s"},
		{user, ungranted.ID, "Revoked", "r"},
	} {
		if err := c.FollowPodcast(ctx, f.user, f.lib, f.path, f.sid); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.GrantWholeLibrary(ctx, user, pods.ID); err != nil {
		t.Fatal(err)
	}
	sh, err := c.CreateShare(ctx, Share{Name: "Shown only", Paths: []PathRule{{LibraryID: more.ID, Path: "Shown"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.GrantShare(ctx, user, sh.ID); err != nil {
		t.Fatal(err)
	}
	who := Caller{ID: user, Podcasts: true}
	rows, err := c.PodcastExport(ctx, who)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%d/%s/%v", r.LibraryID, r.Path, r.Private))
	}
	want := fmt.Sprintf("[%d/A Show/false %d/B Show/false %d/Private/true %d/Shown/false]", pods.ID, pods.ID, pods.ID, more.ID)
	if fmt.Sprint(got) != want {
		t.Fatalf("export rows %v, want %s", got, want)
	}
	// An old client sees no podcasts at all, admin or not.
	if rows, err := c.PodcastExport(ctx, Caller{ID: user, Admin: true}); err != nil || len(rows) != 0 {
		t.Fatalf("old client %v %v", rows, err)
	}
	// The other user exports only their own follow.
	rows, err = c.PodcastExport(ctx, Caller{ID: other, Admin: true, Podcasts: true})
	if err != nil || len(rows) != 1 || rows[0].Path != "Theirs" {
		t.Fatalf("other %+v %v", rows, err)
	}
}

// The export is never cut at PodcastShows' 2,000 rows, and past
// MaxPodcastExportRows it is refused rather than truncated.
func TestPodcastExportBounds(t *testing.T) {
	c, ctx := newTestCatalog(t)
	user := seedUser(t, c, ctx)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.GrantWholeLibrary(ctx, user, lib.ID); err != nil {
		t.Fatal(err)
	}
	add := func(from, to int) {
		t.Helper()
		err := c.db.WithTx(ctx, "seed", func(tx *sql.Tx) error {
			for i := from; i < to; i++ {
				p, sid := fmt.Sprintf("Show %05d", i), fmt.Sprintf("id%d", i)
				if _, err := tx.ExecContext(ctx, `INSERT INTO podcast_shows(library_id,show_path,show_id,title,author,description,link,language,show_type,explicit,private,feed_url,feed_host,feed_url_sha256,podcast_guid,cover_path,episode_count,latest_published,feed_sig,indexed_at)
					VALUES(?,?,?,?,'','','','','episodic',0,0,?,'','','','',0,NULL,'','t')`, lib.ID, p, sid, p, "https://example.org/"+sid+".xml"); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO podcast_follows(user_id,library_id,show_path,show_id,created_at) VALUES(?,?,?,?,'t')`, user, lib.ID, p, sid); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	who := Caller{ID: user, Podcasts: true}
	add(0, 2500)
	rows, err := c.PodcastExport(ctx, who)
	if err != nil || len(rows) != 2500 {
		t.Fatalf("2,500 follows: %d %v", len(rows), err)
	}
	add(2500, MaxPodcastExportRows)
	if rows, err = c.PodcastExport(ctx, who); err != nil || len(rows) != MaxPodcastExportRows {
		t.Fatalf("at the cap: %d %v", len(rows), err)
	}
	add(MaxPodcastExportRows, MaxPodcastExportRows+1)
	if _, err := c.PodcastExport(ctx, who); !errors.Is(err, ErrPodcastExportTooLarge) {
		t.Fatalf("past the cap: %v", err)
	}
	if _, err := c.PodcastExport(context.Background(), Caller{ID: user + 99, Podcasts: true}); err != nil {
		t.Fatalf("a stranger: %v", err)
	}
}

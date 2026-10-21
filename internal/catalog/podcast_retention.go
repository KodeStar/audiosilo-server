package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

type Policy struct {
	Mode string `json:"mode"`
	Keep int    `json:"keep"`
	Days int    `json:"days"`
}

func DefaultPodcastPolicy() Policy { return Policy{Mode: "keep", Keep: 3, Days: 7} }
func (p Policy) Valid() bool {
	return (p.Mode == "keep" || p.Mode == "on_demand" || p.Mode == "archive") && p.Keep >= 1 && p.Keep <= 1000 && p.Days >= 0 && p.Days <= 365
}

var ErrPodcastIdentity = errors.New("podcast identity changed")
var ErrInvalidPolicy = errors.New("invalid podcast policy")

func (c *Catalog) PodcastPolicy(ctx context.Context, lib int64, show, id string) (Policy, error) {
	p := DefaultPodcastPolicy()
	var current string
	err := c.db.QueryRowContext(ctx, `SELECT s.show_id,COALESCE(p.mode,'keep'),COALESCE(p.keep,3),COALESCE(p.days,7) FROM podcast_shows s LEFT JOIN podcast_policies p ON p.library_id=s.library_id AND p.show_path=s.show_path AND p.show_id=s.show_id WHERE s.library_id=? AND s.show_path=?`, lib, show).Scan(&current, &p.Mode, &p.Keep, &p.Days)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if current != id {
		return p, ErrPodcastIdentity
	}
	return p, nil
}
func (c *Catalog) SetPodcastPolicy(ctx context.Context, lib int64, show, id string, p *Policy) error {
	if p != nil && !p.Valid() {
		return ErrInvalidPolicy
	}
	return c.db.WithTx(ctx, "SetPodcastPolicy", func(tx *sql.Tx) error {
		var current string
		err := tx.QueryRowContext(ctx, `SELECT show_id FROM podcast_shows WHERE library_id=? AND show_path=?`, lib, show).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if current != id {
			return ErrPodcastIdentity
		}
		if p == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM podcast_policies WHERE library_id=? AND show_path=? AND show_id=?`, lib, show, id)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO podcast_policies(library_id,show_path,show_id,mode,keep,days,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(library_id,show_path,show_id) DO UPDATE SET mode=excluded.mode,keep=excluded.keep,days=excluded.days,updated_at=excluded.updated_at`, lib, show, id, p.Mode, p.Keep, p.Days, c.ts())
		}
		return err
	})
}

type PodcastPin struct {
	ShowID    string
	EpisodeID string
	FeedSig   string
	Present   bool
	Fetch     *feed.Fetch
}

func (c *Catalog) PodcastPin(ctx context.Context, lib int64, p string) (PodcastPin, error) {
	var pin PodcastPin
	var raw sql.NullString
	err := c.db.QueryRowContext(ctx, `SELECT s.show_id,e.episode_id,e.feed_sig,e.present,e.fetch_json FROM podcast_episodes e JOIN books b ON b.id=e.book_id JOIN podcast_shows s ON s.library_id=e.library_id AND s.show_path=e.show_path WHERE b.library_id=? AND b.rel_path=?`, lib, p).Scan(&pin.ShowID, &pin.EpisodeID, &pin.FeedSig, &pin.Present, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return pin, ErrNotFound
	}
	if err != nil {
		return pin, err
	}
	if raw.Valid {
		_ = json.Unmarshal([]byte(raw.String), &pin.Fetch)
	}
	return pin, nil
}

// MarkPodcastStream covers the entire open/serve/transcode lifetime, including
// failed opens. The release is safe to call once on every exit path.
func (c *Catalog) MarkPodcastStream(ref Ref) func() {
	c.podcastStreamsMu.Lock()
	if c.podcastStreams == nil {
		c.podcastStreams = map[Ref]int{}
	}
	c.podcastStreams[ref]++
	c.podcastStreamsMu.Unlock()
	return func() {
		c.podcastStreamsMu.Lock()
		defer c.podcastStreamsMu.Unlock()
		c.podcastStreams[ref]--
		if c.podcastStreams[ref] == 0 {
			delete(c.podcastStreams, ref)
		}
	}
}

type RetentionRequest struct {
	ShowID     string `json:"show_id"`
	Path       string `json:"path"`
	Generation int    `json:"generation"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
}
type RetentionCheck struct {
	LibraryID int64  `json:"library_id"`
	ShowPath  string `json:"show_path"`
	RetentionRequest
	CheckedAt         string  `json:"checked_at"`
	Policy            Policy  `json:"policy"`
	Followers         int     `json:"followers"`
	AllFinished       bool    `json:"all_finished"`
	AllFinishedAt     *string `json:"all_finished_at"`
	Starred           bool    `json:"starred"`
	Queued            bool    `json:"queued"`
	ActiveStream      bool    `json:"active_stream"`
	FetchOpen         bool    `json:"fetch_open"`
	OutsideKeepWindow bool    `json:"outside_keep_window"`
	DeleteAfter       *string `json:"delete_after"`
	Eligible          bool    `json:"eligible"`
}

// snapshotDB makes the existing access resolver read the same transaction as
// the listening facts. No HTTP or filesystem work occurs under this snapshot.
type snapshotDB struct {
	catalogDB
	tx *sql.Tx
}

func (s snapshotDB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.tx.QueryContext(ctx, q, args...)
}
func (s snapshotDB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return s.tx.QueryRowContext(ctx, q, args...)
}

func (c *Catalog) PodcastRetentionCheck(ctx context.Context, lib int64, req RetentionRequest) (RetentionCheck, error) {
	out := RetentionCheck{LibraryID: lib, ShowPath: path.Dir(req.Path), RetentionRequest: req}
	err := c.db.WithTx(ctx, "PodcastRetentionCheck", func(tx *sql.Tx) error {
		snapshot := &Catalog{db: snapshotDB{catalogDB: c.db, tx: tx}, now: c.now}
		pin, err := snapshot.PodcastPin(ctx, lib, req.Path)
		if err != nil {
			return err
		}
		if pin.ShowID != req.ShowID || !pin.Present || !feed.ValidFetch(pin.Fetch) || pin.Fetch.DeletedAt != "" || pin.Fetch.Generation != req.Generation || pin.Fetch.SHA256 != req.SHA256 || pin.Fetch.Bytes != req.Bytes {
			return ErrPodcastIdentity
		}
		out.Policy, err = snapshot.PodcastPolicy(ctx, lib, out.ShowPath, req.ShowID)
		if err != nil {
			return err
		}
		now := c.now().UTC()
		out.CheckedAt = now.Format(time.RFC3339)
		if err = snapshot.retentionFollowers(ctx, lib, out.ShowPath, req.ShowID, req.Path, &out); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM favourites WHERE library_id=?1 AND rel_path=?2),EXISTS(SELECT 1 FROM up_next WHERE library_id=?1 AND rel_path=?2),EXISTS(SELECT 1 FROM bridge_jobs WHERE library_id=?1 AND rel_path=?2 AND kind='podcast.fetch' AND state IN `+bridge.OpenStatesSQL+`)`, lib, req.Path).Scan(&out.Starred, &out.Queued, &out.FetchOpen); err != nil {
			return err
		}
		var inside bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT b.rel_path FROM podcast_episodes e JOIN books b ON b.id=e.book_id WHERE e.library_id=? AND e.show_path=? AND e.removed_at='' ORDER BY e.published IS NULL,COALESCE(e.published,'') DESC,b.rel_path DESC LIMIT ?) WHERE rel_path=?)`, lib, out.ShowPath, out.Policy.Keep, req.Path).Scan(&inside)
		if err != nil {
			return err
		}
		out.OutsideKeepWindow = !inside
		if out.AllFinished && out.AllFinishedAt != nil {
			finished, _ := time.Parse(time.RFC3339, *out.AllFinishedAt)
			fetched, _ := time.Parse(time.RFC3339, pin.Fetch.FetchedAt)
			if fetched.After(finished) {
				finished = fetched
			}
			deadline := finished.Add(time.Duration(out.Policy.Days) * 24 * time.Hour).UTC().Format(time.RFC3339)
			out.DeleteAfter = &deadline
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	c.podcastStreamsMu.Lock()
	out.ActiveStream = c.podcastStreams[Ref{LibraryID: lib, Path: req.Path}] > 0
	c.podcastStreamsMu.Unlock()
	if out.DeleteAfter != nil {
		deadline, _ := time.Parse(time.RFC3339, *out.DeleteAfter)
		out.Eligible = out.Policy.Mode != "archive" && out.Followers > 0 && out.AllFinished && !c.now().Before(deadline) && !out.Starred && !out.Queued && !out.ActiveStream && !out.FetchOpen && (out.Policy.Mode != "keep" || out.OutsideKeepWindow)
	}
	return out, nil
}
func (c *Catalog) retentionFollowers(ctx context.Context, lib int64, show, id, p string, out *RetentionCheck) error {
	type follower struct {
		id       int64
		admin    bool
		finished bool
		at       sql.NullString
	}
	followers, err := queryRows(ctx, c.db, func(r *sql.Rows, f *follower) error { return r.Scan(&f.id, &f.admin, &f.finished, &f.at) }, `SELECT u.id,u.role='admin',COALESCE(p.finished,0),p.finished_at FROM podcast_follows f JOIN users u ON u.id=f.user_id LEFT JOIN progress p ON p.user_id=u.id AND p.library_id=f.library_id AND p.rel_path=? WHERE f.library_id=? AND f.show_path=? AND f.show_id=? AND u.disabled=0 AND u.is_demo=0`, p, lib, show, id)
	if err != nil {
		return err
	}
	out.AllFinished = true
	latest := time.Time{}
	for _, f := range followers {
		who, err := c.callerByID(ctx, f.id, f.admin)
		if err != nil {
			return err
		}
		who.Podcasts = true
		scope, err := c.CallerScope(ctx, who, lib)
		if err != nil {
			return err
		}
		if !scope.Allows(p) {
			continue
		}
		out.Followers++
		at, e := time.Parse(time.RFC3339, f.at.String)
		if !f.finished || e != nil {
			out.AllFinished = false
			continue
		}
		if at.After(latest) {
			latest = at
		}
	}
	out.AllFinished = out.AllFinished && out.Followers > 0
	if out.AllFinished {
		at := latest.UTC().Format(time.RFC3339)
		out.AllFinishedAt = &at
	}
	return nil
}

func movePodcastState(ctx context.Context, tx *sql.Tx, lib int64, oldPath, newPath string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO podcast_policies(library_id,show_path,show_id,mode,keep,days,updated_at) SELECT library_id,?,show_id,mode,keep,days,updated_at FROM podcast_policies WHERE library_id=? AND show_path=? ON CONFLICT(library_id,show_path,show_id) DO UPDATE SET mode=excluded.mode,keep=excluded.keep,days=excluded.days,updated_at=excluded.updated_at WHERE excluded.updated_at>podcast_policies.updated_at`, newPath, lib, oldPath); err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM podcast_policies WHERE library_id=?1 AND show_path=?2 AND ?3 IS NOT NULL`,
		`DELETE FROM podcast_feed_state WHERE library_id=?1 AND (show_path=?2 OR show_path=?3)`,
		`DELETE FROM podcast_fetch_state WHERE library_id=?1 AND (rel_path=?2 OR rel_path=?3 OR substr(rel_path,1,length(?2)+1)=?2||'/' OR substr(rel_path,1,length(?3)+1)=?3||'/')`,
	} {
		if _, err := tx.ExecContext(ctx, q, lib, oldPath, newPath); err != nil {
			return err
		}
	}
	return nil
}

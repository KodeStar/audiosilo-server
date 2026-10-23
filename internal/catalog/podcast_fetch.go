package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

// foldPodcastFetch never touches feed polling state, validators or secrets.
func (c *Catalog) foldPodcastFetch(ctx context.Context, tx *sql.Tx, j bridge.Job) error {
	if j.State == bridge.StateCancelled {
		return nil
	}
	var params bridge.EpisodeParams
	if json.Unmarshal(j.Params, &params) != nil {
		return nil
	}
	var current, episode string
	err := tx.QueryRowContext(ctx, `SELECT s.show_id,e.episode_id FROM podcast_episodes e JOIN books b ON b.id=e.book_id JOIN podcast_shows s ON s.library_id=e.library_id AND s.show_path=e.show_path WHERE b.library_id=? AND b.rel_path=?`, j.LibraryID, j.Path).Scan(&current, &episode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != params.ShowID || episode != params.EpisodeID {
		return nil
	}
	var result struct {
		ShowID    string `json:"show_id"`
		EpisodeID string `json:"episode_id"`
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retry     *int   `json:"retry_after_seconds"`
	}
	_ = json.Unmarshal(j.Result, &result)
	if result.ShowID != "" && result.ShowID != current || result.EpisodeID != "" && result.EpisodeID != episode || result.Code == "identity_changed" {
		return nil
	}
	var failures int
	var last sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT failures,last_job_id FROM podcast_fetch_state WHERE library_id=? AND rel_path=? AND show_id=?`, j.LibraryID, j.Path, current).Scan(&failures, &last)
	if last.Valid && last.Int64 >= j.ID {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var next any
	if j.State == bridge.StateDone {
		failures = 0
		result.Code = ""
		result.Message = ""
	} else {
		seconds := min(1800<<min(failures, 6), 86400)
		if result.Retry != nil {
			seconds = min(max(seconds, *result.Retry), 86400)
		}
		failures++
		next = c.now().UTC().Add(time.Duration(seconds) * time.Second).Format(time.RFC3339)
		if result.Code == "" {
			result.Code = "bridge_lost"
		}
		result.Message = bridge.FetchErrorFor(result.Code).Message
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO podcast_fetch_state(library_id,rel_path,show_id,last_job_id,failures,next_attempt_at,error_code,error) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(library_id,rel_path,show_id) DO UPDATE SET last_job_id=excluded.last_job_id,failures=excluded.failures,next_attempt_at=excluded.next_attempt_at,error_code=excluded.error_code,error=excluded.error`, j.LibraryID, j.Path, current, j.ID, failures, next, result.Code, result.Message)
	return err
}

type PodcastWorkShow struct {
	Ref
	ShowID    string
	Policy    Policy
	Present   bool
	RetainDue bool
}

// PodcastWorkShows includes only shows with a live, matching follower. Access
// is resolved again for retention itself; this is only a scheduling hint.
func (c *Catalog) PodcastWorkShows(ctx context.Context) ([]PodcastWorkShow, error) {
	shows, err := queryRows(ctx, c.db, func(r *sql.Rows, s *PodcastWorkShow) error {
		return r.Scan(&s.LibraryID, &s.Path, &s.ShowID, &s.Policy.Mode, &s.Policy.Keep, &s.Policy.Days, &s.Present, &s.RetainDue)
	}, `SELECT s.library_id,s.show_path,s.show_id,`+podcastPolicySQL+`,EXISTS(SELECT 1 FROM podcast_episodes e WHERE e.library_id=s.library_id AND e.show_path=s.show_path AND e.present=1),NOT EXISTS(SELECT 1 FROM bridge_jobs j WHERE j.library_id=s.library_id AND j.rel_path=s.show_path AND j.kind='podcast.retain' AND j.created_at>?) FROM podcast_shows s JOIN libraries l ON l.id=s.library_id LEFT JOIN podcast_policies p ON p.library_id=s.library_id AND p.show_path=s.show_path AND p.show_id=s.show_id WHERE l.kind='podcasts' AND EXISTS(SELECT 1 FROM podcast_follows f JOIN users u ON u.id=f.user_id WHERE f.library_id=s.library_id AND f.show_path=s.show_path AND f.show_id=s.show_id AND `+liveFollowerSQL+`) ORDER BY s.library_id,s.show_path`, c.now().UTC().Add(-time.Hour).Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	filtered := shows[:0]
	for _, show := range shows {
		allowed, err := c.podcastHasFollowers(ctx, show)
		if err != nil {
			return nil, err
		}
		if allowed {
			filtered = append(filtered, show)
		}
	}
	shows = filtered
	c.podcastScheduleMu.Lock()
	last := c.podcastVisit
	c.podcastScheduleMu.Unlock()
	i := slices.IndexFunc(shows, func(s PodcastWorkShow) bool {
		return s.LibraryID > last.LibraryID || s.LibraryID == last.LibraryID && s.Path > last.Path
	})
	if i > 0 {
		shows = append(shows[i:], shows[:i]...)
	}
	return shows, err
}
func (c *Catalog) PodcastVisited(ref Ref) {
	c.podcastScheduleMu.Lock()
	c.podcastVisit = ref
	c.podcastScheduleMu.Unlock()
}

type PodcastFetchCandidate struct {
	Path      string
	EpisodeID string
}

func (c *Catalog) PodcastFetchCandidates(ctx context.Context, s PodcastWorkShow) ([]PodcastFetchCandidate, error) {
	if s.Policy.Mode == "on_demand" {
		return nil, nil
	}
	order, limit := "DESC", 20000
	if s.Policy.Mode == "keep" {
		limit = s.Policy.Keep
	} else {
		order = "ASC"
	}
	// An open fetch already naming this identity is skipped here rather than
	// handed to Enqueue, which would only return it: otherwise every pass costs
	// one writer transaction per open job, and Archive's open jobs keep growing.
	return queryRows(ctx, c.db, func(r *sql.Rows, e *PodcastFetchCandidate) error { return r.Scan(&e.Path, &e.EpisodeID) }, `SELECT w.rel_path,w.episode_id FROM (SELECT b.rel_path,e.episode_id,e.present,e.published,e.season,e.episode FROM podcast_episodes e JOIN books b ON b.id=e.book_id WHERE e.library_id=?1 AND e.show_path=?2 AND e.removed_at='' ORDER BY e.published IS NULL,`+episodeOrderSQL(order)+` LIMIT ?3) w LEFT JOIN podcast_fetch_state f ON f.library_id=?1 AND f.rel_path=w.rel_path AND f.show_id=?4 WHERE w.present=0 AND (f.next_attempt_at IS NULL OR f.next_attempt_at<=?5) AND NOT EXISTS(SELECT 1 FROM bridge_jobs j WHERE j.library_id=?1 AND j.rel_path=w.rel_path AND j.kind='podcast.fetch' AND j.state IN `+bridge.OpenStatesSQL+` AND json_extract(j.params,'$.show_id')=?4 AND json_extract(j.params,'$.episode_id')=w.episode_id) ORDER BY w.published IS NULL,COALESCE(w.published,'') `+order+`,COALESCE(w.season,-1) `+order+`,COALESCE(w.episode,-1) `+order+`,w.rel_path `+order, s.LibraryID, s.Path, limit, s.ShowID, c.ts())
}

func (c *Catalog) podcastHasFollowers(ctx context.Context, show PodcastWorkShow) (bool, error) {
	type follower struct {
		id    int64
		admin bool
	}
	followers, err := queryRows(ctx, c.db, func(r *sql.Rows, f *follower) error { return r.Scan(&f.id, &f.admin) }, `SELECT u.id,u.role='admin' FROM podcast_follows f JOIN users u ON u.id=f.user_id WHERE f.library_id=? AND f.show_path=? AND f.show_id=? AND `+liveFollowerSQL, show.LibraryID, show.Path, show.ShowID)
	if err != nil {
		return false, err
	}
	for _, f := range followers {
		scope, err := c.followerScope(ctx, f.id, f.admin, show.LibraryID)
		if err != nil {
			return false, err
		}
		if scope.VisibleInBrowse(show.Path) {
			return true, nil
		}
	}
	return false, nil
}

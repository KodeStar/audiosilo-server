package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

type PodcastError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type PodcastStatus struct {
	LastChecked *string       `json:"last_checked"`
	LastSuccess *string       `json:"last_success"`
	NextCheck   *string       `json:"next_check"`
	Error       *PodcastError `json:"error"`
	// SecretMissing: a check found the Bridge no longer holds this private
	// feed's address (feed_secret_missing), and none has succeeded since, so
	// following the show again by its address is what restores it
	// (podcast_feed_state.secret_missing; server#219).
	SecretMissing bool `json:"secret_missing"`
}
type PodcastShow struct {
	LibraryID       int64         `json:"library_id"`
	Path            string        `json:"path"`
	Title           string        `json:"title"`
	Author          string        `json:"author"`
	Description     string        `json:"description"`
	Link            string        `json:"link"`
	Language        string        `json:"language"`
	Type            string        `json:"type"`
	Explicit        bool          `json:"explicit"`
	Private         bool          `json:"private"`
	FeedURL         *string       `json:"feed_url"`
	FeedHost        string        `json:"feed_host"`
	PodcastGUID     string        `json:"podcast_guid"`
	EpisodeCount    int           `json:"episode_count"`
	LatestPublished *string       `json:"latest_published"`
	CoverVersion    string        `json:"cover_version"`
	Following       bool          `json:"following"`
	Status          PodcastStatus `json:"status"`
	Retention       Policy        `json:"retention"`
	CoverPath       string        `json:"-"`
	FeedSig         string        `json:"-"`
	ShowID          string        `json:"-"`
}
type PodcastGeneration struct {
	Fetch           *bridge.FetchStatus `json:"fetch,omitempty"`
	Generation      int                 `json:"generation,omitempty"`
	SHA256          string              `json:"sha256,omitempty"`
	TimelineChanged *bool               `json:"timeline_changed,omitempty"`
}
type PodcastEpisodeInfo struct {
	PodcastGeneration
	ShowPath       string  `json:"show_path"`
	Published      *string `json:"published"`
	Season         *int    `json:"season"`
	Episode        *int    `json:"episode"`
	EpisodeType    string  `json:"episode_type"`
	Explicit       bool    `json:"explicit"`
	Downloaded     bool    `json:"downloaded"`
	DurationSource string  `json:"duration_source"`
	Removed        bool    `json:"removed"`
}
type PodcastEpisode struct {
	PodcastGeneration
	Path           string  `json:"path"`
	Title          string  `json:"title"`
	Published      *string `json:"published"`
	Duration       float64 `json:"duration"`
	DurationSource string  `json:"duration_source"`
	Season         *int    `json:"season"`
	Episode        *int    `json:"episode"`
	EpisodeType    string  `json:"episode_type"`
	Explicit       bool    `json:"explicit"`
	Description    string  `json:"description"`
	Downloaded     bool    `json:"downloaded"`
	Size           int64   `json:"size"`
	Removed        bool    `json:"removed"`
}
type PodcastEpisodePage struct {
	Episodes   []PodcastEpisode `json:"episodes"`
	NextCursor *string          `json:"next_cursor"`
}

// PodcastFile is a presence observation from one directory listing, never a probe.
type PodcastFile struct {
	Size     int64
	MTime    int64
	Chapters *metadata.ChapterList
	FeedSig  string
}

func stringOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// IndexPodcastShow commits a show's entire derived index together. It never
// deduplicates GUIDs or touches durable follows and listening state.
//
// f.ReadAt is when the show's feed.json was read (zero: no check), kept as
// podcast_shows.feed_read_at. A feed.json read before the one the index was
// folded from is not folded over it, whatever either's show identity: a scan
// that read the file just before the Bridge rewrote it and asked for its own
// rescan would otherwise commit after that rescan and put the show back as it
// was (server#403). The order is the reads', on this server's clock: the
// Bridge's updated_at is whole seconds and its own clock's, so it can't order
// two writes in one second. A feed.json restored from an older backup is
// still indexed, as it is read after the index it replaces.
//
// It records the SetPodcastNotice listener's event for the episodes new to a
// show indexed before under the same identity, in the same transaction, and
// delivers it after the commit: an episode not indexed before (by path, GUID or
// id), not dated before the show's latest episode then (the latest not dated
// ahead of the clock, so one mistyped date hides nothing; one with the same
// date or none counts) and first seen by the Bridge within NewEpisodeWindow. So
// a show's first index, a forced rescan of a feed already indexed, a rebuilt
// index, a show whose missing episodes are indexed again, a folder renamed by
// hand the Bridge has not taken in (feed.File.Rerooted), a quiet re-read (the
// feed.json its row was read from, read again: nothing it lists is the
// Bridge's news), and an index restored from an old backup or written after a
// long outage (episodes the Bridge found days ago) announce nothing (the
// Bridge's forced rescan after a poll that grew the feed does announce).
func (c *Catalog) IndexPodcastShow(ctx context.Context, lib int64, show, sig, cover, coverVersion string, f *feed.File, present map[string]PodcastFile, force, quiet bool) (bool, error) {
	changed := false
	var event *ServerEvent
	err := c.db.WithTx(ctx, "IndexPodcastShow", func(tx *sql.Tx) error {
		var kind, library string
		if err := tx.QueryRowContext(ctx, `SELECT kind,name FROM libraries WHERE id=?`, lib).Scan(&kind, &library); err != nil {
			return err
		}
		if kind != KindPodcasts {
			return ErrInvalidKind
		}
		var old, oldShowID, oldRead string
		var oldLatest sql.NullString
		var oldCount int
		err := tx.QueryRowContext(ctx, `SELECT feed_sig,show_id,latest_published,episode_count,feed_read_at FROM podcast_shows WHERE library_id=? AND show_path=?`, lib, show).Scan(&old, &oldShowID, &oldLatest, &oldCount, &oldRead)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && old == sig && !force {
			return nil
		}
		// Whatever its identity: a show replaced at the path (followed again
		// by a new address) must not be put back by a scan that read the
		// old feed.json before the replacement was indexed.
		if err == nil && staleFeed(f.ReadAt, oldRead, c.now()) {
			return nil
		}
		// News is told only of a show indexed before as this same show, and
		// never from a feed.json read in a folder its paths don't name
		// (feed.File.Rerooted): the Bridge has not written it since the
		// folder was renamed by hand, so what it lists was news at the old
		// folder, however late its files turn up here (a move still copying,
		// a copy whose original went).
		announce := err == nil && oldShowID == f.ShowID && !f.Rerooted && !quiet
		type oldEpisode struct {
			book                  int64
			path, guid, episodeID string
		}
		oldEpisodes, err := queryRows(ctx, tx, func(r *sql.Rows, o *oldEpisode) error { return r.Scan(&o.book, &o.path, &o.guid, &o.episodeID) }, `SELECT b.id,b.rel_path,e.guid,e.episode_id FROM books b JOIN podcast_episodes e ON e.book_id=b.id WHERE e.library_id=? AND e.show_path=?`, lib, show)
		if err != nil {
			return err
		}
		// An index missing some of the episodes it listed (an older server's
		// scan removed their rows) can't tell which are new.
		announce = announce && len(oldEpisodes) >= oldCount
		// The episodes indexed before, by path, GUID and id (read only for
		// news).
		paths, guids, ids := map[string]bool{}, map[string]bool{}, map[string]bool{}
		if announce {
			for _, o := range oldEpisodes {
				paths[o.path], ids[o.episodeID] = true, true
				if o.guid != "" {
					guids[o.guid] = true
				}
			}
		}
		// A latest episode dated ahead of the clock (a mistyped date in the
		// feed) would hide every episode after it: the newest indexed episode
		// not ahead of the clock stands in, when there is one.
		newerThan := oldLatest.String
		if now := c.now().UTC().Format(time.RFC3339); announce && newerThan > now {
			var past sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT MAX(published) FROM podcast_episodes WHERE library_id=? AND show_path=? AND published<=?`, lib, show, now).Scan(&past); err != nil {
				return err
			}
			if past.Valid {
				newerThan = past.String
			}
		}
		recentFrom := c.now().Add(-NewEpisodeWindow).UTC().Format(time.RFC3339)
		var news []feed.Episode
		latest := ""
		for _, e := range f.Episodes {
			if e.Published > latest {
				latest = e.Published
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO podcast_shows(library_id,show_path,show_id,title,author,description,link,language,show_type,explicit,private,feed_url,feed_host,feed_url_sha256,podcast_guid,cover_path,episode_count,latest_published,feed_sig,indexed_at,feed_read_at)
   VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(library_id,show_path) DO UPDATE SET show_id=excluded.show_id,title=excluded.title,author=excluded.author,description=excluded.description,link=excluded.link,language=excluded.language,show_type=excluded.show_type,explicit=excluded.explicit,private=excluded.private,feed_url=excluded.feed_url,feed_host=excluded.feed_host,feed_url_sha256=excluded.feed_url_sha256,podcast_guid=excluded.podcast_guid,cover_path=excluded.cover_path,episode_count=excluded.episode_count,latest_published=excluded.latest_published,feed_sig=excluded.feed_sig,indexed_at=excluded.indexed_at,feed_read_at=excluded.feed_read_at`, lib, show, f.ShowID, f.Show.Title, f.Show.Author, f.Show.Description, f.Show.Link, f.Show.Language, f.Show.Type, f.Show.Explicit, f.Private, f.FeedURL, f.FeedHost, f.FeedURLSHA256, f.Show.PodcastGUID, cover, len(f.Episodes), stringOrNil(latest), sig, c.ts(), feedReadStamp(f.ReadAt))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM podcast_feed_hashes WHERE library_id=? AND show_path=?`, lib, show); err != nil {
			return err
		}
		for _, hash := range append([]string{f.FeedURLSHA256}, f.PreviousFeedURLSHA256...) {
			if !feed.ValidHex(hash, 64) {
				continue
			}
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO podcast_feed_hashes(library_id,show_path,sha256) VALUES(?,?,?)`, lib, show, hash); err != nil {
				return err
			}
		}
		keep := map[string]bool{}
		for _, e := range f.Episodes {
			if !feed.EpisodePath(show, e.Path) {
				continue
			}
			keep[e.Path] = true
			if announce && !paths[e.Path] && !ids[e.ID] && !guids[e.GUID] && e.RemovedAt == "" &&
				(e.Published == "" || e.Published >= newerThan) && seenSince(e, recentFrom) {
				news = append(news, e)
			}
			info, exists := present[path.Base(e.Path)]
			exists = exists && feed.ValidFetch(e.Fetch) && e.Fetch.DeletedAt == "" && info.Size == e.Fetch.Bytes
			if !exists {
				info = PodcastFile{}
			}
			feedDuration := durationFromFeed(e.DurationSeconds)
			duration, source := feedDuration, "none"
			if feedDuration > 0 {
				source = "feed"
			}
			gen := 0
			if e.Fetch != nil {
				gen = e.Fetch.Generation
				if exists {
					duration = float64(e.Fetch.DurationMS) / 1000
					source = "file"
				}
			}
			var fetchJSON any
			if e.Fetch != nil {
				raw, _ := json.Marshal(e.Fetch)
				fetchJSON = string(raw)
			}
			hash, codec := "", ""
			timeline := false
			if feed.ValidFetch(e.Fetch) {
				hash = e.Fetch.SHA256
				timeline = e.Fetch.TimelineChanged
				if exists {
					codec = e.Fetch.Codec
				}
			}
			chapterSource := ""
			if exists && info.Chapters != nil {
				chapterSource = "id3_chap"
			}
			var id int64
			indexedAt := c.ts() // the books row's, and its episode row's stamp of it
			err = tx.QueryRowContext(ctx, `INSERT INTO books(library_id,rel_path,title,author,duration,cover_path,cover_art,format,size,mtime,indexed_at,added_at,description,has_cover,codec,chapters_source,genres_checked,released_checked,path_key)
    VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,1,?) ON CONFLICT(library_id,rel_path) DO UPDATE SET path_key=excluded.path_key,title=excluded.title,author=excluded.author,duration=excluded.duration,cover_path=excluded.cover_path,cover_art=excluded.cover_art,format=excluded.format,size=excluded.size,mtime=excluded.mtime,indexed_at=excluded.indexed_at,description=excluded.description,has_cover=excluded.has_cover,codec=excluded.codec,chapters_source=excluded.chapters_source RETURNING id`, lib, e.Path, e.Title, f.Show.Author, duration, cover, coverVersion, strings.TrimPrefix(strings.ToLower(path.Ext(e.Path)), "."), info.Size, info.MTime, indexedAt, e.FirstSeen, e.Description, cover != "", codec, chapterSource, pathKey(e.Path)).Scan(&id)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO podcast_episodes(book_id,library_id,show_path,episode_id,guid,published,season,episode,episode_type,explicit,feed_duration,present,generation,removed_at,duration_source,sha256,timeline_changed,fetch_json,feed_sig,indexed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(book_id) DO UPDATE SET show_path=excluded.show_path,episode_id=excluded.episode_id,guid=excluded.guid,published=excluded.published,season=excluded.season,episode=excluded.episode,episode_type=excluded.episode_type,explicit=excluded.explicit,feed_duration=excluded.feed_duration,present=excluded.present,generation=excluded.generation,removed_at=excluded.removed_at,duration_source=excluded.duration_source,sha256=excluded.sha256,timeline_changed=excluded.timeline_changed,fetch_json=excluded.fetch_json,feed_sig=excluded.feed_sig,indexed_at=excluded.indexed_at`, id, lib, show, e.ID, e.GUID, stringOrNil(e.Published), e.Season, e.Episode, e.EpisodeType, e.Explicit, feedDuration, exists, gen, e.RemovedAt, source, hash, timeline, fetchJSON, info.FeedSig, indexedAt)
			if err != nil {
				return err
			}
			chapters := []metadata.Chapter{}
			switch {
			case chapterSource != "":
				for i, ch := range info.Chapters.Chapters {
					chapters = append(chapters, metadata.Chapter{Index: i, Title: ch.Title, FilePath: e.Path, Start: float64(ch.StartMS) / 1000, End: float64(ch.StartMS+ch.LengthMS) / 1000})
				}
			case exists:
				chapters = append(chapters, metadata.Chapter{Index: 0, Title: e.Title, FilePath: e.Path, Start: 0, End: duration})
			}
			if err = replaceChapters(ctx, tx, id, chapters); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM book_files WHERE book_id=?`, id); err != nil {
				return err
			}
		}
		// The episodes indexed before (their ids stay as the upserts above
		// keep them) that the feed no longer lists.
		for _, o := range oldEpisodes {
			if !keep[o.path] {
				if err := deleteBook(ctx, tx, o.book); err != nil {
					return err
				}
			}
		}
		changed = true
		event, err = c.recordPodcastNotice(ctx, tx, newEpisodesNotice(lib, library, show, f, news))
		return err
	})
	if notify := c.podcastChanged.Load(); err == nil && changed && notify != nil {
		for _, ep := range f.Episodes {
			(*notify)(lib, ep.Path)
		}
	}
	if err == nil {
		c.deliverPodcastNotice(ctx, event)
	}
	return changed, err
}

// staleFeed reports whether a feed.json read at readAt was read before the one
// the show's index was folded from (indexedRead, podcast_shows.feed_read_at):
// folding it would put the show back. A File not read from disk (no readAt)
// and a show indexed before the stamp was kept are never stale. Nor is a read
// when the stamp is ahead of the clock (the clock was set back since): the
// refusal would hold until the clock caught up, so the order is given up
// instead.
func staleFeed(readAt time.Time, indexedRead string, now time.Time) bool {
	if readAt.IsZero() || indexedRead == "" {
		return false
	}
	indexed, err := time.Parse(time.RFC3339Nano, indexedRead)
	return err == nil && readAt.Before(indexed) && !indexed.After(now)
}

// feedReadStamp is a feed.json's read time as podcast_shows.feed_read_at keeps
// it: UTC to the nanosecond, "" for a File not read from disk.
func feedReadStamp(readAt time.Time) string {
	if readAt.IsZero() {
		return ""
	}
	return readAt.UTC().Format(time.RFC3339Nano)
}

// seenSince reports whether the Bridge first saw episode e at or after from
// (its first_seen, else its published date; one with neither may be new). The
// stamps compare as text: feed.ReadFile writes each as UTC RFC 3339 or "".
func seenSince(e feed.Episode, from string) bool {
	seen := cmp.Or(e.FirstSeen, e.Published)
	return seen == "" || seen >= from
}

// PodcastShowIDs is lib's indexed show folders, each with its row's show_id.
func (c *Catalog) PodcastShowIDs(ctx context.Context, lib int64) (map[string]string, error) {
	rows, err := queryRows(ctx, c.db, func(r *sql.Rows, v *[2]string) error { return r.Scan(&v[0], &v[1]) }, `SELECT show_path,show_id FROM podcast_shows WHERE library_id=?`, lib)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r[0]] = r[1]
	}
	return out, nil
}

// PrunePodcastShows removes the named shows of a library with their episodes:
// a scan names only shows it knew at its start, so one indexed meanwhile (a new
// subscription's) is never taken for a show that went.
func (c *Catalog) PrunePodcastShows(ctx context.Context, lib int64, gone []string) error {
	if len(gone) == 0 {
		return nil
	}
	return c.db.WithTx(ctx, "PrunePodcastShows", func(tx *sql.Tx) error {
		for _, show := range gone {
			if err := pruneShow(ctx, tx, lib, show); err != nil {
				return err
			}
		}
		return nil
	})
}

// pruneShow removes one show's index (its row, feed hashes and episodes),
// never its durable follows, feed state or policy.
func pruneShow(ctx context.Context, tx *sql.Tx, lib int64, show string) error {
	ids, err := queryRows(ctx, tx, func(r *sql.Rows, id *int64) error { return r.Scan(id) }, `SELECT book_id FROM podcast_episodes WHERE library_id=? AND show_path=?`, lib, show)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := deleteBook(ctx, tx, id); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM podcast_shows WHERE library_id=? AND show_path=?`, lib, show)
	return err
}

// nonEpisodeBooks is a podcast library's books rows that are not episodes:
// only an older server's books scan (a rollback) writes them, as
// IndexPodcastShow writes an episode's books and podcast_episodes rows together.
const nonEpisodeBooks = `FROM books b JOIN libraries l ON l.id=b.library_id WHERE b.library_id=? AND l.kind='podcasts' AND NOT EXISTS(SELECT 1 FROM podcast_episodes e WHERE e.book_id=b.id)`

// PruneNonEpisodeBooks removes nonEpisodeBooks (a folder book at a show path,
// say, which would answer for the show and for an episode gone from the
// index). The rows are chosen inside the write, never from an older read: an
// episode indexed meanwhile can take over such a row's id.
func (c *Catalog) PruneNonEpisodeBooks(ctx context.Context, lib int64) error {
	var found bool
	if err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 `+nonEpisodeBooks+`)`, lib).Scan(&found); err != nil || !found {
		return err
	}
	return c.db.WithTx(ctx, "PruneNonEpisodeBooks", func(tx *sql.Tx) error {
		ids, err := queryRows(ctx, tx, func(r *sql.Rows, id *int64) error { return r.Scan(id) }, `SELECT b.id `+nonEpisodeBooks, lib)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := deleteBook(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *Catalog) privatePodcastScope(ctx context.Context, user int64, s Scope) (Scope, error) {
	scanPath := func(r *sql.Rows, p *string) error { return r.Scan(p) }
	// The common case (no private show this caller doesn't follow) is one
	// indexed query that returns nothing and leaves s as it is.
	denied, err := queryRows(ctx, c.db, scanPath, `SELECT s.show_path FROM podcast_shows s WHERE s.library_id=? AND s.private=1 AND NOT EXISTS(`+followMatchSQL+` AND f.user_id=?)`, s.LibraryID, user)
	if err != nil {
		return Scope{}, err
	}
	if len(denied) == 0 {
		return s, nil
	}
	hidden := Scope{LibraryID: s.LibraryID, Paths: denied}
	// A whole-library grant becomes the visible shows: it must never retain a
	// private child.
	var visible []string
	if s.AllowAll || slices.Contains(s.Paths, "") {
		all, err := queryRows(ctx, c.db, scanPath, `SELECT show_path FROM podcast_shows WHERE library_id=?`, s.LibraryID)
		if err != nil {
			return Scope{}, err
		}
		visible = slices.DeleteFunc(all, hidden.Allows)
	}
	if s.AllowAll {
		s.AllowAll = false
		s.Paths = visible
		return s, nil
	}
	// Drop grants inside a denied show and expand a root grant into the visible
	// shows.
	out := []string{}
	for _, p := range s.Paths {
		switch {
		case hidden.Allows(p):
		case p == "":
			out = append(out, visible...)
		default:
			out = append(out, p)
		}
	}
	s.Paths = out
	return s, nil
}

// followMatchSQL selects the follows of show s: a follow counts only for the show
// identity it was made for, so a show reusing a folder does not inherit it.
const followMatchSQL = `SELECT 1 FROM podcast_follows f WHERE f.library_id=s.library_id AND f.show_path=s.show_path AND f.show_id=s.show_id`

const podcastShowSelect = `SELECT s.library_id,s.show_path,s.title,s.author,s.description,s.link,s.language,s.show_type,s.explicit,s.private,NULLIF(s.feed_url,''),s.feed_host,s.podcast_guid,s.cover_path,s.episode_count,s.latest_published,s.feed_sig,s.show_id,
 EXISTS(` + followMatchSQL + ` AND f.user_id=?),
 st.last_checked_at,st.last_success_at,st.next_poll_at,COALESCE(st.error_code,''),COALESCE(st.error,''),COALESCE(st.secret_missing,0),` + podcastPolicySQL + `
 FROM podcast_shows s LEFT JOIN podcast_feed_state st ON st.library_id=s.library_id AND st.show_path=s.show_path AND st.show_id=s.show_id LEFT JOIN podcast_policies p ON p.library_id=s.library_id AND p.show_path=s.show_path AND p.show_id=s.show_id`

func scanPodcastShow(r interface{ Scan(...any) error }, s *PodcastShow) error {
	var code, msg string
	err := r.Scan(&s.LibraryID, &s.Path, &s.Title, &s.Author, &s.Description, &s.Link, &s.Language, &s.Type, &s.Explicit, &s.Private, &s.FeedURL, &s.FeedHost, &s.PodcastGUID, &s.CoverPath, &s.EpisodeCount, &s.LatestPublished, &s.FeedSig, &s.ShowID, &s.Following, &s.Status.LastChecked, &s.Status.LastSuccess, &s.Status.NextCheck, &code, &msg, &s.Status.SecretMissing, &s.Retention.Mode, &s.Retention.Keep, &s.Retention.Days)
	if code != "" {
		s.Status.Error = &PodcastError{code, msg}
	}
	if s.Private {
		s.FeedURL = nil
		s.Link = ""
	}
	s.CoverVersion = CoverVersion(s.CoverPath + "|" + s.FeedSig)
	return err
}

// podcastShowFilter is the SQL (over podcast_shows s) selecting the shows scope
// s shows: those it allows, and those holding an episode it grants (as /fs).
func podcastShowFilter(s Scope) (string, []any, error) {
	filter, args := pathFilterSQL("s.show_path", s)
	if !s.AllowAll && len(s.Paths) > 0 {
		paths, err := json.Marshal(s.Paths)
		if err != nil {
			return "", nil, err
		}
		// Shows also appear as ancestors of an episode grant, just as /fs does.
		filter = "(" + filter + " OR EXISTS(SELECT 1 FROM json_each(?) grant_path WHERE substr(grant_path.value,1,length(s.show_path)+1)=s.show_path||'/'))"
		args = append(args, string(paths))
	}
	return filter, args, nil
}

func (c *Catalog) PodcastShows(ctx context.Context, s Scope, user int64, followed bool) ([]PodcastShow, error) {
	filter, args, err := podcastShowFilter(s)
	if err != nil {
		return nil, err
	}
	q := podcastShowSelect + ` WHERE s.library_id=? AND ` + filter
	vals := append([]any{user, s.LibraryID}, args...)
	if followed {
		q += ` AND EXISTS(` + followMatchSQL + ` AND f.user_id=?)`
		vals = append(vals, user)
	}
	q += ` ORDER BY s.title COLLATE NOCASE,s.show_path LIMIT 2000`
	return queryRows(ctx, c.db, func(r *sql.Rows, s *PodcastShow) error { return scanPodcastShow(r, s) }, q, vals...)
}

// PodcastExportRow is one show the OPML export considers: followed by the
// caller (for its current identity) and visible to them now.
type PodcastExportRow struct {
	LibraryID int64
	Path      string
	Title     string
	FeedURL   string // "" for a private show
	Private   bool
}

// MaxPodcastExportRows bounds the shows one export reads.
const MaxPodcastExportRows = 10000

// ErrPodcastExportTooLarge is an export over MaxPodcastExportRows shows.
var ErrPodcastExportTooLarge = errors.New("too many followed shows to export")

// PodcastExport returns the shows who follows, across their podcast libraries,
// as their current scopes show them (the client gate, grants, a member's rule
// and private-show narrowing all apply, admins included), in library id then
// show path order. A follow of a replaced show (another identity at its path)
// or a pruned one counts for nothing. Past MaxPodcastExportRows it is
// ErrPodcastExportTooLarge: never a truncated list.
func (c *Catalog) PodcastExport(ctx context.Context, who Caller) ([]PodcastExportRow, error) {
	if !who.Podcasts {
		return []PodcastExportRow{}, nil
	}
	scopes, err := c.CallerScopes(ctx, who)
	if err != nil {
		return nil, err
	}
	podcasts, err := c.podcastLibraries(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(scopes, func(a, b Scope) int { return cmp.Compare(a.LibraryID, b.LibraryID) })
	out := []PodcastExportRow{}
	for _, s := range scopes {
		if !podcasts[s.LibraryID] {
			continue
		}
		filter, args, err := podcastShowFilter(s)
		if err != nil {
			return nil, err
		}
		vals := append(append([]any{s.LibraryID}, args...), who.ID, MaxPodcastExportRows+1-len(out))
		rows, err := queryRows(ctx, c.db, func(r *sql.Rows, e *PodcastExportRow) error {
			return r.Scan(&e.LibraryID, &e.Path, &e.Title, &e.FeedURL, &e.Private)
		}, `SELECT s.library_id,s.show_path,s.title,s.feed_url,s.private FROM podcast_shows s WHERE s.library_id=? AND `+filter+` AND EXISTS(`+followMatchSQL+` AND f.user_id=?) ORDER BY s.show_path LIMIT ?`, vals...)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(out) > MaxPodcastExportRows {
			return nil, ErrPodcastExportTooLarge
		}
	}
	return out, nil
}

func (c *Catalog) PodcastShow(ctx context.Context, lib int64, p string, user int64) (*PodcastShow, error) {
	var s PodcastShow
	err := scanPodcastShow(c.db.QueryRowContext(ctx, podcastShowSelect+` WHERE s.library_id=? AND s.show_path=?`, user, lib, p), &s)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

// PodcastShowSpelling is the index's spelling of p, a show path a client sent,
// by Spelling's rule for a book (server#210) over podcast_shows: the one show
// whose NFC form is p's that allow admits (nil: every one), so a show folder
// renamed by hand to its other spelling (server#495) is still found by the
// old one. p as given when a show allow admits is indexed there, when none or
// two read as it (it never guesses), and when p is an entry of its own on the
// disk (ownEntry: a folder in that spelling no scan has indexed yet). Only
// Unicode form, never case, as for a book.
//
// A p allow refuses is looked for as a path with no show is, whether or not a
// show is indexed there: such a show counts as none, as every spelling allow
// refuses does (the same reads, a look at the disk only when a show it admits
// reads as p), so neither the answer nor how long it takes tells a caller a
// show they can't see from none. The caller still checks its scope on p.
func (c *Catalog) PodcastShowSpelling(ctx context.Context, lib int64, p string, allow func(string) bool) (string, error) {
	if !HasOtherSpelling(p) {
		return p, nil
	}
	if allow == nil || allow(p) {
		var one int
		err := c.db.QueryRowContext(ctx, `SELECT 1 FROM podcast_shows WHERE library_id=? AND show_path=?`, lib, p).Scan(&one)
		if !errors.Is(err, sql.ErrNoRows) {
			return p, err // the show at p, or a failed lookup
		}
	}
	// A library's shows are one row per top-level folder, read only on a
	// miss of a name with another spelling: no key column to keep.
	shows, err := queryRows(ctx, c.db, func(r *sql.Rows, s *string) error { return r.Scan(s) },
		`SELECT show_path FROM podcast_shows WHERE library_id=?`, lib)
	if err != nil {
		return "", err
	}
	m := &spellMatch{allow: allow}
	m.addReadingAs(p, shows)
	if len(m.in) != 1 || c.ownEntry(ctx, lib, p) {
		return p, nil
	}
	return m.in[0], nil
}

func (c *Catalog) PodcastShowByHash(ctx context.Context, lib int64, hash string, user int64) (*PodcastShow, error) {
	var p string
	err := c.db.QueryRowContext(ctx, `SELECT show_path FROM podcast_feed_hashes WHERE library_id=? AND sha256=? ORDER BY show_path LIMIT 1`, lib, hash).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c.PodcastShow(ctx, lib, p, user)
}

// A follow remains path-keyed and survives pruning, but authorizes only the show
// whose identity the caller proved. Reusing a folder cannot inherit that proof.
const followPodcastSQL = `INSERT INTO podcast_follows(user_id,library_id,show_path,show_id,created_at) VALUES(?,?,?,?,?) ON CONFLICT(user_id,library_id,show_path) DO UPDATE SET show_id=excluded.show_id,created_at=excluded.created_at WHERE podcast_follows.show_id<>excluded.show_id`

func (c *Catalog) FollowPodcast(ctx context.Context, user, lib int64, p, showID string) error {
	_, err := c.db.ExecContext(ctx, followPodcastSQL, user, lib, p, showID, c.ts())
	return err
}

// UnfollowPodcast ends user's follow of the show at p, a show path a client
// sent, in lib, in one statement: their rows at p, at each of spellings (the
// show's spelling the caller's scope found for p: PodcastShowSpelling) and,
// when none of those is a follow of theirs, at the one follow of theirs whose
// path reads as p in another Unicode spelling (never two, and not when p is
// an entry of its own, a show indexed at it or a folder on the disk, as
// PodcastShowSpelling's rule). allow is the caller's scope (nil: every path),
// as PodcastShowSpelling's: a show or folder at p it refuses counts as none,
// so whether a show the caller can't see sits at p changes nothing they can
// tell. A follow outlives the caller's access to its show (a share removed,
// the show's identity replaced), and their own rows still find it by the
// other spelling. Their rows are read first, and p is looked up only when one
// of them reads as it, so the work done for a show they never followed is
// that of a made-up name.
func (c *Catalog) UnfollowPodcast(ctx context.Context, user, lib int64, p string, allow func(string) bool, spellings ...string) error {
	at := append([]string{p}, spellings...)
	if HasOtherSpelling(p) {
		mine, err := queryRows(ctx, c.db, func(r *sql.Rows, s *string) error { return r.Scan(s) },
			`SELECT show_path FROM podcast_follows WHERE user_id=? AND library_id=?`, user, lib)
		if err != nil {
			return err
		}
		m := &spellMatch{}
		m.addReadingAs(p, mine)
		// p and each of spellings read as p, so a lone row reading as p that
		// is none of them means none of them is a follow of theirs. A show
		// indexed at p is an entry of its own, as one on the disk is, so
		// the follow beside it stays when the disk can't be read; one allow
		// refuses is no entry.
		if len(m.in) == 1 && !slices.Contains(at, m.in[0]) {
			own, err := c.showEntryAt(ctx, lib, p, allow)
			if err != nil {
				return err
			}
			if !own {
				at = append(at, m.in[0])
			}
		}
	}
	args := append([]any{user, lib}, anyOf(at)...)
	_, err := c.db.ExecContext(ctx, `DELETE FROM podcast_follows WHERE user_id=? AND library_id=? AND show_path IN (`+placeholders(len(at))+`)`, args...)
	return err
}

// showEntryAt reports whether p is an entry of its own in lib that allow
// admits (nil: every path): a show indexed at p, or a folder at p on the disk.
// One allow refuses is none, and is not looked for.
func (c *Catalog) showEntryAt(ctx context.Context, lib int64, p string, allow func(string) bool) (bool, error) {
	if allow != nil && !allow(p) {
		return false, nil
	}
	var one int
	err := c.db.QueryRowContext(ctx, `SELECT 1 FROM podcast_shows WHERE library_id=? AND show_path=?`, lib, p).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return c.ownEntry(ctx, lib, p), nil
	}
	return err == nil, err
}

// PodcastWholeGrant tests grants before private-show narrowing, which is not a
// loss of permission to add a new show by URL.
func (c *Catalog) PodcastWholeGrant(ctx context.Context, who Caller, lib int64) (bool, error) {
	if !who.Podcasts {
		return false, nil
	}
	if who.Admin {
		return true, nil
	}
	scopes, err := c.userScopes(ctx, who, lib)
	if err != nil {
		return false, err
	}
	s := scopes[lib]
	return s != nil && s.AllowAll, nil
}
func (c *Catalog) PodcastEpisodesWithin(ctx context.Context, lib int64, show, order string, limit int, cursor string, scope Scope) (*PodcastEpisodePage, error) {
	if order != "newest" && order != "oldest" {
		return nil, ErrInvalidCursor
	}
	limit = min(max(limit, 1), 200)
	q := `SELECT b.rel_path,b.title,e.published,b.duration,e.duration_source,e.season,e.episode,e.episode_type,e.explicit,b.description,e.present,b.size,e.removed_at<>'',e.generation,e.sha256,` + podcastTimelineSQL("e.") + ` FROM podcast_episodes e JOIN books b ON b.id=e.book_id WHERE e.library_id=? AND e.show_path=?`
	args := []any{lib, show}
	filter, filterArgs := pathFilterSQL("b.rel_path", scope)
	q += " AND " + filter
	args = append(args, filterArgs...)
	dir, op := "DESC", "<"
	if order == "oldest" {
		dir, op = "ASC", ">"
	}
	if cursor != "" {
		k, ok := decodeEpisodeCursor(cursor, show, order)
		if !ok {
			return nil, ErrInvalidCursor
		}
		q += ` AND (` + episodeKeySQL + `) ` + op + ` (?,?,?,?)`
		args = append(args, k.Published, k.Season, k.Episode, k.Path)
	}
	q += ` ORDER BY ` + episodeOrderSQL(dir) + ` LIMIT ?`
	args = append(args, limit+1)
	eps, err := queryRows(ctx, c.db, func(r *sql.Rows, e *PodcastEpisode) error {
		return r.Scan(&e.Path, &e.Title, &e.Published, &e.Duration, &e.DurationSource, &e.Season, &e.Episode, &e.EpisodeType, &e.Explicit, &e.Description, &e.Downloaded, &e.Size, &e.Removed, &e.Generation, &e.SHA256, &e.TimelineChanged)
	}, q, args...)
	if err != nil {
		return nil, err
	}
	out := &PodcastEpisodePage{Episodes: eps}
	if len(eps) > limit {
		out.Episodes = eps[:limit]
		last := out.Episodes[limit-1]
		v := encodeEpisodeCursor(show, order, episodeKeyOf(last.Published, last.Season, last.Episode, last.Path))
		out.NextCursor = &v
	}
	return out, nil
}

// PodcastEpisodeRows counts the show's indexed episodes (podcast_episodes
// rows), whatever their files.
func (c *Catalog) PodcastEpisodeRows(ctx context.Context, lib int64, show string) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM podcast_episodes e JOIN books b ON b.id=e.book_id
	  WHERE e.library_id=? AND e.show_path=? AND (e.indexed_at IS NULL OR e.indexed_at=b.indexed_at)`, lib, show).Scan(&n)
	return n, err
}

// NextPodcastEpisode is the episode after the one at path in its show, in the
// episode order oldest first (what plays next, whether the show lists newest
// or oldest first), among the episodes in scope that can still play (in the
// feed, or kept on disk). nil at the show's end, or when path is no episode.
func (c *Catalog) NextPodcastEpisode(ctx context.Context, lib int64, path string, scope Scope) (*Book, error) {
	var show string
	var published *string
	var season, episode *int
	err := c.db.QueryRowContext(ctx, `SELECT e.show_path,e.published,e.season,e.episode FROM podcast_episodes e JOIN books b ON b.id=e.book_id WHERE b.library_id=? AND b.rel_path=?`, lib, path).Scan(&show, &published, &season, &episode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k := episodeKeyOf(published, season, episode, path)
	filter, filterArgs := pathFilterSQL("b.rel_path", scope)
	b, err := scanBook(c.db.QueryRowContext(ctx, `SELECT `+prefixCols("b.")+` FROM podcast_episodes e JOIN books b ON b.id=e.book_id WHERE e.library_id=? AND e.show_path=? AND (e.removed_at='' OR e.present=1) AND (`+episodeKeySQL+`) > (?,?,?,?) AND `+filter+` ORDER BY `+episodeOrderSQL("ASC")+` LIMIT 1`,
		append([]any{lib, show, k.Published, k.Season, k.Episode, k.Path}, filterArgs...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// An episode's place in its show: when it was published (undated as empty),
// then its season (none as noSeason) and episode number (none as noEpisode:
// newest first, numbered before unnumbered), then its path, as the Bridge's
// feed.json orders newest first (podcast.NewestFirst, but for the path's
// direction). The numbers order an undated serial (a LibriVox reading, whose
// episodes the Bridge names by a hash) as its feed numbers it, and same-day
// episodes too. episodeKeyCols is that key over aliases (ep for
// podcast_episodes' columns, bk for books'): episodeKeySQL over e and b for a
// keyset comparison, episodeOrderSQL sorting by it in dir (ASC is oldest
// first), episodeOrderOn the same over other aliases.
const noSeason, noEpisode = 0, -1

func episodeKeyCols(ep, bk string) []string {
	return []string{
		`COALESCE(` + ep + `published,'')`,
		`COALESCE(` + ep + `season,` + strconv.Itoa(noSeason) + `)`,
		`COALESCE(` + ep + `episode,` + strconv.Itoa(noEpisode) + `)`,
		bk + `rel_path`,
	}
}

var episodeKeySQL = strings.Join(episodeKeyCols("e.", "b."), ",")

func episodeOrderSQL(dir string) string { return episodeOrderOn("e.", "b.", dir) }

func episodeOrderOn(ep, bk, dir string) string {
	return strings.Join(episodeKeyCols(ep, bk), ` `+dir+`,`) + ` ` + dir
}

// episodeKey is one episode's values of episodeKeySQL (its JSON is the
// cursor's).
type episodeKey struct {
	Published string `json:"p"`
	Season    int    `json:"n"`
	Episode   int    `json:"e"`
	Path      string `json:"r"`
}

func episodeKeyOf(published *string, season, episode *int, path string) episodeKey {
	k := episodeKey{Season: noSeason, Episode: noEpisode, Path: path}
	if published != nil {
		k.Published = *published
	}
	if season != nil {
		k.Season = *season
	}
	if episode != nil {
		k.Episode = *episode
	}
	return k
}

// episodeCursor is an episode page's opaque cursor: the show, the order and
// the page's last row's key. A cursor from before the key's season and
// episode (a four-string array) does not decode, so it is refused.
type episodeCursor struct {
	Show  string `json:"s"`
	Order string `json:"o"`
	episodeKey
}

func encodeEpisodeCursor(show, order string, k episodeKey) string {
	b, _ := json.Marshal(episodeCursor{show, order, k})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeEpisodeCursor(cursor, show, order string) (episodeKey, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	var v episodeCursor
	if err != nil || json.Unmarshal(raw, &v) != nil || v.Show != show || v.Order != order {
		return episodeKey{}, false
	}
	return v.episodeKey, true
}

// podcastTimelineSQL selects an episode's timeline_changed flag, NULL (omitted
// on the wire) while it has never been fetched. prefix is the table alias.
func podcastTimelineSQL(prefix string) string {
	return "CASE WHEN " + prefix + "generation=0 THEN NULL ELSE " + prefix + "timeline_changed END"
}

func (c *Catalog) loadPodcastEpisode(ctx context.Context, b *Book) error {
	var e PodcastEpisodeInfo
	err := c.db.QueryRowContext(ctx, `SELECT show_path,published,season,episode,episode_type,explicit,present,duration_source,removed_at<>'',generation,sha256,`+podcastTimelineSQL("")+` FROM podcast_episodes WHERE book_id=?`, b.ID).Scan(&e.ShowPath, &e.Published, &e.Season, &e.Episode, &e.EpisodeType, &e.Explicit, &e.Downloaded, &e.DurationSource, &e.Removed, &e.Generation, &e.SHA256, &e.TimelineChanged)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err == nil {
		b.Episode = &e
	}
	return err
}

type PodcastFeedState struct {
	ShowID        string
	ETag          string
	LastModified  string
	LastChecked   *string
	ErrorCode     string
	SecretMissing bool
}

// PollParams are a podcast.poll's params for this state: its conditional-GET
// validators, and whether the scheduler queued it (a follower's Check now
// isn't).
func (s PodcastFeedState) PollParams(scheduled bool) json.RawMessage {
	raw, _ := json.Marshal(bridge.PollParams{ShowID: s.ShowID, ETag: s.ETag, LastModified: s.LastModified, Scheduled: scheduled}) // strings and a bool: never fails
	return raw
}

func (c *Catalog) PodcastState(ctx context.Context, lib int64, show string) (PodcastFeedState, error) {
	var s PodcastFeedState
	err := c.db.QueryRowContext(ctx, `SELECT s.show_id,COALESCE(st.etag,''),COALESCE(st.last_modified,''),st.last_checked_at,COALESCE(st.error_code,''),COALESCE(st.secret_missing,0) FROM podcast_shows s LEFT JOIN podcast_feed_state st ON st.library_id=s.library_id AND st.show_path=s.show_path AND st.show_id=s.show_id WHERE s.library_id=? AND s.show_path=?`, lib, show).Scan(&s.ShowID, &s.ETag, &s.LastModified, &s.LastChecked, &s.ErrorCode, &s.SecretMissing)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return s, err
}

// pollInHandWindow is how long DuePodcastPolls counts an ended poll whose fold
// has not landed as the show's poll in hand. It is the scheduler's own window,
// far shorter than bridge.PollFoldWindow (the sweep's): a fold that keeps
// failing pauses that show's polls for at most this long, while the sweep goes
// on retrying the fold for the whole day.
const pollInHandWindow = 15 * time.Minute

// DuePodcastPolls are the followed shows whose feed check is due and has no
// poll in hand: none open, and none ended but not folded yet within
// pollInHandWindow (its fold moves next_poll_at). A poll
// that ends between a tick's sweep and this read is the second case: without
// it the same show is queued again within the second (server#403).
func (c *Catalog) DuePodcastPolls(ctx context.Context, limit int) ([]Ref, error) {
	now := c.now().UTC()
	return queryRows(ctx, c.db, func(r *sql.Rows, p *Ref) error { return r.Scan(&p.LibraryID, &p.Path) }, `SELECT s.library_id,s.show_path FROM podcast_shows s JOIN libraries l ON l.id=s.library_id LEFT JOIN podcast_feed_state st ON st.library_id=s.library_id AND st.show_path=s.show_path AND st.show_id=s.show_id WHERE l.kind='podcasts' AND EXISTS(`+followMatchSQL+`) AND COALESCE(st.error_code,'')<>'gone' AND (st.next_poll_at IS NULL OR st.next_poll_at<=?) AND NOT EXISTS(SELECT 1 FROM bridge_jobs j WHERE j.library_id=s.library_id AND j.rel_path=s.show_path AND j.kind='podcast.poll' AND (j.state IN `+bridge.OpenStatesSQL+` OR j.state IN `+bridge.EndedStatesSQL+` AND j.finished_at>=? AND `+bridge.UnfoldedSQL+`)) ORDER BY st.next_poll_at,s.library_id,s.show_path LIMIT ?`, now.Format(time.RFC3339), now.Add(-pollInHandWindow).Format(time.RFC3339), limit)
}
func (c *Catalog) PodcastNotWritable(ctx context.Context, ref Ref) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO podcast_feed_state(library_id,show_path,show_id,error_code,error,next_poll_at) SELECT library_id,show_path,show_id,'library_not_writable','The Bridge cannot write to this library.',? FROM podcast_shows WHERE library_id=? AND show_path=? ON CONFLICT(library_id,show_path,show_id) DO UPDATE SET error_code=excluded.error_code,error=excluded.error,next_poll_at=excluded.next_poll_at`, c.now().Add(30*time.Minute).UTC().Format(time.RFC3339), ref.LibraryID, ref.Path)
	return err
}

// FoldPodcastJob is idempotent even if an unfollow happens before the recovery
// sweep. Durable follow and status writes share the folded marker's transaction.
// A podcast.poll's fold also keeps the feed's run of failures for the admin's
// notifications (feedRun.next) and records, in the same transaction, the
// SetPodcastNotice listener's event that the feed is failing or back, which is
// delivered after the commit; a job folded before tells nothing again.
func (c *Catalog) FoldPodcastJob(ctx context.Context, j bridge.Job) error {
	if !bridge.IsPodcast(j.Kind) || j.LibraryID == nil || !slices.Contains([]string{bridge.StateDone, bridge.StateFailed, bridge.StateCancelled}, j.State) {
		return nil
	}
	var event *ServerEvent
	err := c.db.WithTx(ctx, "FoldPodcastJob", func(tx *sql.Tx) error {
		now := c.now().UTC()
		stamp := now.Format(time.RFC3339)
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO podcast_folded(job_id,at) VALUES(?,?)`, j.ID, stamp)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		if j.Kind == bridge.KindPodcastFetch {
			return c.foldPodcastFetch(ctx, tx, j)
		}
		// A cancel tells nothing of the feed; a failure stored cancelled (one
		// reported once the job was asked to stop, bridge.PodcastFailed) does.
		if j.Kind == bridge.KindPodcastRetain || j.State == bridge.StateCancelled && !bridge.PodcastFailed(j) {
			return nil
		}
		// A field of the wrong type stays at its zero value rather than failing
		// the fold, which the recovery sweep would retry on every tick; so do a
		// subscribe's params that aren't JSON (a damaged row its claim ended),
		// naming no show.
		result, _ := bridge.PodcastOutcomeOf(j)
		// From here such a failure folds as any other: its code, its
		// Retry-After and the run's count. (Read first: the outcome of a job
		// stored failed takes the job's error as its message, and this one's
		// error is the cancel's.)
		if j.State == bridge.StateCancelled {
			j.State = bridge.StateFailed
		}
		show := j.Target()
		if j.Kind == bridge.KindPodcastSubscribe {
			var p bridge.SubscribeParams
			_ = json.Unmarshal(j.Params, &p)
			show = p.ShowPath
			if j.State == bridge.StateDone {
				show = result.ShowPath
			}
		}
		if !feed.ShowPath(show) {
			return nil
		}
		var expected string
		var scheduled bool
		if j.Kind == bridge.KindPodcastPoll {
			var p bridge.PollParams
			_ = json.Unmarshal(j.Params, &p)
			expected, scheduled = p.ShowID, p.Scheduled
		} else {
			var p bridge.SubscribeParams
			_ = json.Unmarshal(j.Params, &p)
			expected = p.ShowID
			if j.State == bridge.StateDone && expected == "" {
				expected = result.ShowID
			}
		}
		if !feed.ValidHex(expected, 16) {
			return nil
		}
		var current, title, library string
		var private bool
		lookup := func(show string) error {
			err := tx.QueryRowContext(ctx, `SELECT s.show_id,s.title,s.private,l.name FROM podcast_shows s JOIN libraries l ON l.id=s.library_id WHERE s.library_id=? AND s.show_path=?`, *j.LibraryID, show).Scan(&current, &title, &private, &library)
			if errors.Is(err, sql.ErrNoRows) {
				current = ""
				return nil
			}
			return err
		}
		if err := lookup(show); err != nil {
			return err
		}
		// A poll of a show folder renamed by hand ran where the folder is now
		// (the outcome's show_path). Once the index has moved the show there
		// (MovePodcastShows) the queued folder holds it no more, and the outcome
		// is the new folder's. A copy still holding the show keeps its own
		// outcome: the Bridge answers a copy's poll with the folder it copies.
		if j.Kind == bridge.KindPodcastPoll && current != expected && result.ShowPath != show && feed.ShowPath(result.ShowPath) {
			if err := lookup(result.ShowPath); err != nil {
				return err
			}
			if current == expected {
				show = result.ShowPath
			}
		}
		if current == "" {
			return nil
		}
		// A poll whose fold kept failing past pollInHandWindow can land after
		// the next poll of its show has folded: its outcome is stale and would
		// undo the newer one's (feed state, run, notices). Folding at the
		// folder the show was renamed to, the next poll may be queued there.
		if j.Kind == bridge.KindPodcastPoll {
			var newer bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM bridge_jobs n JOIN podcast_folded f ON f.job_id=n.id WHERE n.library_id=? AND n.rel_path IN (?,?) AND n.kind=? AND n.id>?)`, *j.LibraryID, j.Target(), show, bridge.KindPodcastPoll, j.ID).Scan(&newer); err != nil || newer {
				return err
			}
		}
		if current != expected || result.ShowID != "" && result.ShowID != expected {
			return nil
		}
		// Without a valid show identity the follow could never match its show.
		if j.Kind == bridge.KindPodcastSubscribe && j.State == bridge.StateDone && j.RequestedBy != nil && feed.ValidHex(result.ShowID, 16) {
			if _, err := tx.ExecContext(ctx, followPodcastSQL, j.RequestedBy.ID, *j.LibraryID, show, result.ShowID, stamp); err != nil {
				return err
			}
		}
		var failures int
		var run feedRun
		var due, checked sql.NullString // the next scheduled poll and the last check, as stored
		var stored string               // the last check's error code
		err = tx.QueryRowContext(ctx, `SELECT failures,feed_failures,feed_network_failures,failing_announced,failing_code,next_poll_at,last_checked_at,error_code FROM podcast_feed_state WHERE library_id=? AND show_path=? AND show_id=?`, *j.LibraryID, show, expected).Scan(&failures, &run.failures, &run.network, &run.announced, &run.code, &due, &checked, &stored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// Only a poll's outcome moves the run: a subscribe tells nothing of a
		// feed already followed. The scheduler marks its polls (Scheduled); a
		// follower's Check now is unmarked.
		if j.Kind == bridge.KindPodcastPoll {
			p := poll{scheduled: scheduled}
			if j.State == bridge.StateFailed && scheduled && bridge.NetworkFailure(result.Code) {
				if p.reachable, err = bridgeReachedFeeds(ctx, tx, *j.LibraryID, show, checked.String); err != nil {
					return err
				}
			}
			var news string
			if run, news = run.next(j.State, result.Code, p); news != "" {
				notice := &PodcastNotice{Kind: news, Library: library, LibraryID: *j.LibraryID, ShowPath: show, ShowID: expected, Show: title, Private: private}
				if news == PodcastFeedFailing {
					notice.Code = result.Code
					// A lost secret isn't the feed's failure: no run to count.
					if bridge.FeedFailure(result.Code) {
						notice.Failures = run.failures
					}
				}
				if event, err = c.recordPodcastNotice(ctx, tx, notice); err != nil {
					return err
				}
			}
		}
		if j.State == bridge.StateDone {
			// The validators go back out as a poll's params, which refuse one over
			// bridge.MaxPollValidator bytes: drop it (the next poll asks without it)
			// rather than store a value no poll could ever be queued with.
			for _, v := range []*string{&result.ETag, &result.LastModified} {
				if len(*v) > bridge.MaxPollValidator {
					*v = ""
				}
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO podcast_feed_state(library_id,show_path,show_id,etag,last_modified,last_checked_at,last_success_at,last_status,error_code,error,failures,next_poll_at,feed_failures,feed_network_failures,failing_announced,failing_code) VALUES(?,?,?,?,?,?,?,?,'','',0,?,?,?,?,?) ON CONFLICT(library_id,show_path,show_id) DO UPDATE SET etag=CASE WHEN excluded.last_status=304 THEN podcast_feed_state.etag ELSE excluded.etag END,last_modified=CASE WHEN excluded.last_status=304 THEN podcast_feed_state.last_modified ELSE excluded.last_modified END,last_checked_at=excluded.last_checked_at,last_success_at=excluded.last_success_at,last_status=excluded.last_status,error_code='',error='',secret_missing=0,failures=0,next_poll_at=excluded.next_poll_at,feed_failures=excluded.feed_failures,feed_network_failures=excluded.feed_network_failures,failing_announced=excluded.failing_announced,failing_code=excluded.failing_code`, *j.LibraryID, show, expected, result.ETag, result.LastModified, stamp, stamp, result.Status, now.Add(time.Duration(min(max(result.NextPollSeconds, 900), 86400))*time.Second).Format(time.RFC3339), run.failures, run.network, run.announced, run.code)
		} else if stored == "gone" && result.Code != "gone" {
			// A feed that ended stays ended whatever a later check fails with
			// (a follower's Check now: the scheduler asks no more): `gone`
			// and no next poll are what keep the scheduled polls stopped
			// (DuePodcastPolls), and only a success brings the feed back. The
			// check's time (Check now's one a minute reads it), the run and a
			// lost secret are still kept.
			_, err = tx.ExecContext(ctx, `UPDATE podcast_feed_state SET last_checked_at=?,secret_missing=MAX(secret_missing,?),feed_failures=?,feed_network_failures=?,failing_announced=?,failing_code=? WHERE library_id=? AND show_path=? AND show_id=?`, stamp, result.Code == "feed_secret_missing", run.failures, run.network, run.announced, run.code, *j.LibraryID, show, expected)
		} else {
			seconds := 1800 << min(failures, 6)
			if result.RetryAfterSeconds != nil {
				seconds = min(max(*result.RetryAfterSeconds, 60), 86400)
			}
			seconds = min(seconds, 86400)
			at := now.Add(time.Duration(seconds) * time.Second).Format(time.RFC3339)
			backoff := failures + 1
			// A Check now the run doesn't count leaves the scheduled polls as
			// they were too: backing them off would space the polls that do
			// count a day apart, and the failing notice with them. A
			// Retry-After still holds them off.
			if j.Kind == bridge.KindPodcastPoll && uncountedCheck(result.Code, scheduled) && due.Valid {
				backoff = failures
				if result.RetryAfterSeconds == nil || due.String > at {
					at = due.String
				}
			}
			var next any = at
			if result.Code == "gone" {
				next = nil
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO podcast_feed_state(library_id,show_path,show_id,last_checked_at,last_status,error_code,error,secret_missing,failures,next_poll_at,feed_failures,feed_network_failures,failing_announced,failing_code) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(library_id,show_path,show_id) DO UPDATE SET last_checked_at=excluded.last_checked_at,last_status=excluded.last_status,error_code=excluded.error_code,error=excluded.error,secret_missing=MAX(podcast_feed_state.secret_missing,excluded.secret_missing),failures=excluded.failures,next_poll_at=excluded.next_poll_at,feed_failures=excluded.feed_failures,feed_network_failures=excluded.feed_network_failures,failing_announced=excluded.failing_announced,failing_code=excluded.failing_code`, *j.LibraryID, show, expected, stamp, result.Status, result.Code, result.Message, result.Code == "feed_secret_missing", backoff, next, run.failures, run.network, run.announced, run.code)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM podcast_folded WHERE at<? AND NOT EXISTS(SELECT 1 FROM bridge_jobs j WHERE j.id=podcast_folded.job_id)`, now.Add(-7*24*time.Hour).Format(time.RFC3339))
		return err
	})
	if err == nil {
		c.deliverPodcastNotice(ctx, event)
	}
	return err
}

// bridgeReachedFeeds reports whether a scheduled poll's network-class failure
// of show can be this feed's own: the paired Bridge reached another feed since
// the show's previous check (since, as stored; "" for none), by a success or
// an answer from it (bridge.FeedAnswered: a feed failing with an HTTP error was
// reached all the same), or failed to reach none since, so nothing says its
// internet is down. A Bridge that lost its internet fails every show, and none
// of them counts (server#403). A check that failed at the Bridge (bridge_lost,
// write_failed) says nothing either way.
//
// "Since" includes the previous check's own second: the stamps are whole
// seconds, and the polls of a round can share one (the Bridge runs several
// side by side, and a scheduler pass folds every ended job in a row:
// podcast.SchedulerTick). Read strictly after it, the round's first show would
// see none of the others' checks and count through an outage.
func bridgeReachedFeeds(ctx context.Context, tx *sql.Tx, lib int64, show, since string) (bool, error) {
	type check struct{ code, success string }
	checks, err := queryRows(ctx, tx, func(r *sql.Rows, c *check) error { return r.Scan(&c.code, &c.success) },
		`SELECT error_code,COALESCE(last_success_at,'') FROM podcast_feed_state WHERE NOT (library_id=?1 AND show_path=?2) AND last_checked_at>=?3`, lib, show, since)
	if err != nil {
		return false, err
	}
	unreached := false
	for _, c := range checks {
		switch {
		case c.success != "" && c.success >= since, bridge.FeedAnswered(c.code):
			return true, nil
		case bridge.NetworkFailure(c.code):
			unreached = true
		}
	}
	return !unreached, nil
}

func durationFromFeed(p *int) float64 {
	if p == nil {
		return 0
	}
	return float64(*p)
}

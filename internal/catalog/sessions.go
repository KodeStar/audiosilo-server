package catalog

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Listening sessions are derived on the server from the progress saves players
// already make (every 15 s while playing, and on pause/seek/stop), rather than
// from the listening-history spans players post: those arrive only when playback
// stops and are dropped while the player is offline, so they can't say who is
// listening right now, and they carry no device. A progress save carries the
// token (so the device and app), works for every client already shipped, and
// arrives while playback runs. See RecordHeartbeat.

const (
	// SessionGap is how long a token may go without a progress save on a book
	// before its next save starts a new session ("Sessions with no position update
	// for 10 minutes close automatically").
	SessionGap = 10 * time.Minute
	// livePlaying is how recent the newest save must be for a session to read as
	// playing; past it, until SessionGap, the session reads as paused. Players
	// save every 15 s, so this allows a few missed saves.
	livePlaying = 60 * time.Second
	// resumeWindow bounds how far back a save may reach to continue a session
	// across a gap longer than SessionGap: a player the OS stopped from saving
	// while it kept playing (Android pauses JS timers while the phone is locked)
	// sends its next save with the position advanced by about the time that
	// passed, and that listening belongs to the same session (continuousPlayback).
	resumeWindow = 12 * time.Hour
	// SessionRetention is how long raw sessions (device, app, time of day,
	// playback mode) are kept before PruneSessions sums them into listening_daily.
	// Just over a year, so the Activity page's longest range (a year, or the
	// current calendar year) always reads raw sessions.
	SessionRetention = 400 * 24 * time.Hour
)

// sessionTime is the session timestamp format: UTC with milliseconds, fixed
// width, so stored times compare correctly as strings (RFC3339Nano trims
// trailing zeros and would not).
const sessionTime = "2006-01-02T15:04:05.000Z"

func formatSessionTime(t time.Time) string { return t.UTC().Format(sessionTime) }

func parseSessionTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// Client is the app behind a session's token (auth.ClientInfo's shape; this
// package doesn't import auth). App is empty for a client that never named itself.
type Client struct {
	App      string `json:"app"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
}

// Heartbeat is one progress save, as RecordHeartbeat sees it.
type Heartbeat struct {
	UserID int64
	Ref    Ref
	// TokenID, DeviceName and Client identify the device (the token that saved).
	TokenID    int64
	DeviceName string
	Client     Client
	Position   float64
	Duration   float64
	Speed      float64
	Finished   bool
	// Transcoded: the token streamed this book through the transcoder lately
	// (StreamMarks).
	Transcoded bool
	// At is when the server received the save. Only server time is used: a
	// device's clock can be wrong, and listenedBetween caps any save (a replay
	// from an offline queue included) by the time that really passed.
	At time.Time
}

// continuousPlayback reports whether a save after a long gap looks like playback
// that ran the whole time without saving: the position advanced by about the
// time that passed (at the playback speed), at least 90% of it and not much more.
// The upper bound keeps a jump (a "mark finished" or a seek far ahead hours after
// the last save) from reading as hours of listening.
func continuousPlayback(wall time.Duration, advanced, speed float64) bool {
	if speed <= 0 {
		speed = 1
	}
	played, secs := advanced/speed, wall.Seconds()
	return advanced > 0 && played >= 0.9*secs && played <= 1.1*secs+livePlaying.Seconds()
}

// listenedBetween is the playback time between two saves: the position advance
// converted to wall-clock time at the playback speed, capped by the time that
// actually passed (a seek forward is not listening). A seek back adds nothing.
func listenedBetween(wall time.Duration, advanced, speed float64) float64 {
	if advanced <= 0 || wall <= 0 {
		return 0
	}
	if speed <= 0 {
		speed = 1
	}
	return math.Min(wall.Seconds(), advanced/speed)
}

// RecordHeartbeat folds one progress save into the token's session on the book:
// it extends the session the token last saved on that book when the save comes
// within SessionGap, or later but with the position advanced by about the time
// that passed (continuousPlayback); otherwise it starts a new one. A session that
// has recorded no listening yet (a single save, such as a "mark finished" or a
// sync from another app) is not shown anywhere (listenedSQL). Recording is
// best-effort bookkeeping beside the progress itself; callers log a failure and
// still answer the save.
func (c *Catalog) RecordHeartbeat(ctx context.Context, hb Heartbeat) error {
	if hb.Speed <= 0 {
		hb.Speed = 1
	}
	at := formatSessionTime(hb.At)
	return c.db.WithTx(ctx, "RecordHeartbeat", func(tx *sql.Tx) error {
		var (
			id     int64
			lastAt string
			endPos float64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, last_at, end_pos FROM listening_sessions
			  WHERE user_id = ? AND token_id = ? AND library_id = ? AND rel_path = ? AND last_at >= ?
			  ORDER BY id DESC LIMIT 1`,
			hb.UserID, hb.TokenID, hb.Ref.LibraryID, hb.Ref.Path,
			formatSessionTime(hb.At.Add(-resumeWindow))).Scan(&id, &lastAt, &endPos)
		if err == nil {
			wall := hb.At.Sub(parseSessionTime(lastAt))
			if wall > SessionGap && !continuousPlayback(wall, hb.Position-endPos, hb.Speed) {
				err = sql.ErrNoRows // too long ago: this save starts a new session
			}
		}
		if errors.Is(err, sql.ErrNoRows) {
			var codec string
			if err := tx.QueryRowContext(ctx,
				`SELECT codec FROM books WHERE library_id = ? AND rel_path = ?`,
				hb.Ref.LibraryID, hb.Ref.Path).Scan(&codec); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO listening_sessions(user_id, library_id, rel_path, token_id, device_name,
				     client_app, client_version, client_platform, started_at, last_at, start_pos, end_pos,
				     duration, speed, codec, transcoded, finished)
				 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				hb.UserID, hb.Ref.LibraryID, hb.Ref.Path, hb.TokenID, hb.DeviceName,
				hb.Client.App, hb.Client.Version, hb.Client.Platform, at, at, hb.Position, hb.Position,
				hb.Duration, hb.Speed, codec, hb.Transcoded, hb.Finished)
			return err
		}
		if err != nil {
			return err
		}
		add := listenedBetween(hb.At.Sub(parseSessionTime(lastAt)), hb.Position-endPos, hb.Speed)
		// hb.Client is already the token's newest app (a save without the header
		// carries the stored one), so it is written as is.
		_, err = tx.ExecContext(ctx,
			`UPDATE listening_sessions SET last_at = ?, end_pos = ?, duration = ?, speed = ?,
			        listened = listened + ?, finished = MAX(finished, ?), transcoded = MAX(transcoded, ?),
			        client_app = ?, client_version = ?, client_platform = ?
			  WHERE id = ?`,
			at, hb.Position, hb.Duration, hb.Speed, add, hb.Finished, hb.Transcoded,
			hb.Client.App, hb.Client.Version, hb.Client.Platform, id)
		return err
	})
}

// Session states, from the age of the newest save.
const (
	SessionPlaying = "playing" // a save within livePlaying
	SessionPaused  = "paused"  // still open (within SessionGap)
	SessionEnded   = "ended"
)

// Session is a listening session as the admin console shows it.
type Session struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	// Title and Author come from the index; "" when the book isn't indexed (any
	// more), in which case the path names it.
	Title  string `json:"title"`
	Author string `json:"author"`
	// DeviceID is the token's id (the id GET /admin/devices lists); DeviceName and
	// Client are copied at the time, so they survive the device's sign-out.
	DeviceID   int64   `json:"device_id"`
	DeviceName string  `json:"device_name"`
	Client     *Client `json:"client"`
	StartedAt  string  `json:"started_at"`
	LastAt     string  `json:"last_at"`
	StartPos   float64 `json:"start_position"`
	EndPos     float64 `json:"position"`
	Duration   float64 `json:"duration"`
	Speed      float64 `json:"speed"`
	// Listened is wall-clock seconds of playback.
	Listened   float64 `json:"listened"`
	Codec      string  `json:"codec"`
	Transcoded bool    `json:"transcoded"`
	Finished   bool    `json:"finished"`
	State      string  `json:"state"`
	// Chapter (the chapter at the position) and IP (the device's newest address)
	// are filled for live sessions only.
	Chapter string `json:"chapter,omitempty"`
	IP      string `json:"ip,omitempty"`
}

const sessionColumns = `s.id, s.user_id, u.username, s.library_id, s.rel_path,
	COALESCE(b.title, ''), COALESCE(b.author, ''), s.token_id, s.device_name,
	s.client_app, s.client_version, s.client_platform, s.started_at, s.last_at,
	s.start_pos, s.end_pos, s.duration, s.speed, s.listened, s.codec, s.transcoded, s.finished`

// listenedSQL keeps a session out of every list and total until it has recorded
// some listening: a single save (a player's "mark finished", another app syncing
// positions) opens a session that never accumulates any.
const listenedSQL = `s.listened > 0`

const sessionFrom = ` FROM listening_sessions s
	JOIN users u ON u.id = s.user_id
	LEFT JOIN books b ON b.library_id = s.library_id AND b.rel_path = s.rel_path`

// scanSession scans sessionColumns (plus extra destinations after them) and
// derives the client and the state.
func (c *Catalog) scanSession(rows *sql.Rows, s *Session, extra ...any) error {
	var cl Client
	dest := []any{&s.ID, &s.UserID, &s.Username, &s.LibraryID, &s.Path, &s.Title, &s.Author,
		&s.DeviceID, &s.DeviceName, &cl.App, &cl.Version, &cl.Platform, &s.StartedAt, &s.LastAt,
		&s.StartPos, &s.EndPos, &s.Duration, &s.Speed, &s.Listened, &s.Codec, &s.Transcoded, &s.Finished}
	if err := rows.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	if cl.App != "" {
		s.Client = &cl
	}
	s.State = sessionState(c.now().Sub(parseSessionTime(s.LastAt)))
	return nil
}

func sessionState(age time.Duration) string {
	switch {
	case age <= livePlaying:
		return SessionPlaying
	case age <= SessionGap:
		return SessionPaused
	}
	return SessionEnded
}

// LiveSessions returns the open sessions (a save within SessionGap), newest
// first, at most one per device: a device that moved on to another book shows
// only the book it is on now. Each carries the chapter at its position and the
// device's newest address.
func (c *Catalog) LiveSessions(ctx context.Context) ([]Session, error) {
	type live struct {
		Session
		bookID int64
	}
	all, err := queryRows(ctx, c.db, func(rows *sql.Rows, l *live) error {
		return c.scanSession(rows, &l.Session, &l.IP, &l.bookID)
	}, `SELECT `+sessionColumns+`, COALESCE(t.last_ip, ''), COALESCE(b.id, 0)`+sessionFrom+`
	      LEFT JOIN tokens t ON t.id = s.token_id
	     WHERE s.last_at >= ? ORDER BY s.last_at DESC, s.id DESC`,
		formatSessionTime(c.now().Add(-SessionGap)))
	if err != nil {
		return nil, err
	}
	out := []Session{}
	seen := map[int64]bool{}
	chapters := map[int64][]metadata.Chapter{} // per book, loaded once
	for _, l := range all {
		// Each device's newest session decides what it is on, even one with nothing
		// listened yet (it just moved to another book), so the device's previous book
		// doesn't linger; only then are sessions without listening left out.
		if seen[l.DeviceID] {
			continue
		}
		seen[l.DeviceID] = true
		if l.Listened <= 0 {
			continue
		}
		if l.bookID != 0 {
			chs, ok := chapters[l.bookID]
			if !ok {
				b := Book{ID: l.bookID}
				if err := c.loadChapters(ctx, &b); err != nil {
					return nil, err
				}
				chs, chapters[l.bookID] = b.Chapters, b.Chapters
			}
			if i := chapterAt(chs, l.EndPos); i >= 0 {
				l.Chapter = chs[i].Title
			}
		}
		out = append(out, l.Session)
	}
	return out, nil
}

// chapterAt is the index of the chapter holding pos on the whole-book timeline
// (the last one starting at or before it; the first for a position before
// every chapter), or -1 for a book without chapters.
func chapterAt(chapters []metadata.Chapter, pos float64) int {
	if len(chapters) == 0 {
		return -1
	}
	idx := 0
	for i, ch := range chapters {
		if ch.BookOffset > pos {
			break
		}
		idx = i
	}
	return idx
}

// SessionFilter narrows ListSessions. Zero values mean "any". Before is a keyset
// cursor: only sessions with a smaller id (older) are listed.
type SessionFilter struct {
	UserID    int64
	LibraryID int64
	Path      string // with LibraryID: one book's sessions
	Before    int64
	Limit     int
}

// ListSessions returns sessions newest first (open ones included), and the cursor
// for the next page (0 when this is the last).
func (c *Catalog) ListSessions(ctx context.Context, f SessionFilter) ([]Session, int64, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	out, err := queryRows(ctx, c.db, func(rows *sql.Rows, s *Session) error { return c.scanSession(rows, s) },
		`SELECT `+sessionColumns+sessionFrom+`
		  WHERE `+listenedSQL+` AND (?1 = 0 OR s.user_id = ?1) AND (?2 = 0 OR s.library_id = ?2)
		    AND (?3 = '' OR s.rel_path = ?3) AND (?4 = 0 OR s.id < ?4)
		  ORDER BY s.id DESC LIMIT ?5`,
		f.UserID, f.LibraryID, f.Path, f.Before, f.Limit+1)
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) > f.Limit {
		out = out[:f.Limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

// PruneSessions sums raw sessions whose last save is older than cutoff into
// listening_daily (per local day, listener and book) and deletes them, in
// batches of one transaction each, so what survives past the retention is how
// long someone listened to what on which day. It returns how many sessions it
// rolled up.
func (c *Catalog) PruneSessions(ctx context.Context, cutoff time.Time, loc *time.Location) (int, error) {
	const batch = 2000
	// Sessions that never recorded any listening are shown nowhere; once they can
	// no longer be continued they are just deleted.
	if _, err := c.db.ExecContext(ctx,
		`DELETE FROM listening_sessions WHERE listened = 0 AND last_at < ?`,
		formatSessionTime(c.now().Add(-resumeWindow))); err != nil {
		return 0, err
	}
	total := 0
	for {
		n, err := c.pruneSessionBatch(ctx, formatSessionTime(cutoff), loc, batch)
		total += n
		if err != nil || n < batch {
			return total, err
		}
	}
}

func (c *Catalog) pruneSessionBatch(ctx context.Context, cutoff string, loc *time.Location, limit int) (int, error) {
	n := 0
	err := c.db.WithTx(ctx, "PruneSessions", func(tx *sql.Tx) error {
		count, maxID, sums, err := expiredSessions(ctx, tx, cutoff, loc, limit)
		if err != nil || count == 0 {
			return err
		}
		for k, v := range sums {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO listening_daily(day, user_id, library_id, rel_path, listened, sessions)
				 VALUES(?,?,?,?,?,?)`, k.day, k.user, k.lib, k.path, v.listened, v.sessions); err != nil {
				return err
			}
		}
		// The batch is the lowest ids before the cutoff, so this deletes exactly it.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM listening_sessions WHERE last_at < ? AND id <= ?`, cutoff, maxID); err != nil {
			return err
		}
		n = count
		return nil
	})
	return n, err
}

// dayKey and daySum are one listening_daily row being built.
type dayKey struct {
	day       string
	user, lib int64
	path      string
}

type daySum struct {
	listened float64
	sessions int
}

// expiredSessions reads the up to limit lowest-id sessions whose last save is
// before cutoff and sums them per local day, listener and book (a session counts
// on the day it started; its listening is shared over the days it spans). It
// returns how many it read and the highest id among them.
func expiredSessions(ctx context.Context, tx *sql.Tx, cutoff string, loc *time.Location, limit int) (int, int64, map[dayKey]*daySum, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, user_id, library_id, rel_path, started_at, last_at, listened
		   FROM listening_sessions WHERE last_at < ? ORDER BY id LIMIT ?`, cutoff, limit)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	sums := map[dayKey]*daySum{}
	var (
		count int
		maxID int64
	)
	for rows.Next() {
		var (
			user, lib         int64
			path, start, last string
			listened          float64
		)
		if err := rows.Scan(&maxID, &user, &lib, &path, &start, &last, &listened); err != nil {
			return 0, 0, nil, err
		}
		count++
		first := true
		spreadListening(parseSessionTime(start), parseSessionTime(last), listened, loc, func(hour time.Time, secs float64) {
			k := dayKey{hour.Format(time.DateOnly), user, lib, path}
			if sums[k] == nil {
				sums[k] = &daySum{}
			}
			sums[k].listened += secs
			if first {
				sums[k].sessions++
				first = false
			}
		})
	}
	return count, maxID, sums, rows.Err()
}

// hourOf is the start of the local hour holding t. It subtracts the minutes and
// seconds rather than rebuilding the time with time.Date, which maps the repeated
// hour of a daylight-saving fall-back onto its first occurrence (spreadListening
// would then never get past it).
func hourOf(t time.Time) time.Time {
	return t.Add(-time.Duration(t.Minute())*time.Minute - time.Duration(t.Second())*time.Second -
		time.Duration(t.Nanosecond()))
}

// spreadListening shares a session's listened seconds evenly over the local hours
// its span covers (from its first save to its last), calling fn once per hour with
// the start of that hour in loc. A session with no span puts everything in its
// starting hour.
func spreadListening(start, end time.Time, listened float64, loc *time.Location, fn func(hour time.Time, secs float64)) {
	start, end = start.In(loc), end.In(loc)
	span := end.Sub(start)
	if span <= 0 || listened <= 0 {
		fn(hourOf(start), math.Max(listened, 0))
		return
	}
	for t := start; t.Before(end); {
		h := hourOf(t)
		next := h.Add(time.Hour)
		if next.After(end) {
			next = end
		}
		fn(h, listened*next.Sub(t).Seconds()/span.Seconds())
		t = next
	}
}

// StreamMarks remembers, for a short while, which files each token streamed
// through the transcoder, so the progress saves that follow can mark the session
// as transcoded. The stream request carries a file path; the session is keyed on
// the book, so a mark matches a book when the file is the book or inside its
// folder. In memory only: after a restart the next transcoded stream marks again.
type StreamMarks struct {
	mu    sync.Mutex
	marks map[streamKey][]streamMark
}

type streamKey struct{ token, library int64 }

type streamMark struct {
	path string
	at   time.Time
}

// maxStreamMarks bounds the memory: per token+library, and across all keys.
const (
	maxMarksPerKey = 8
	maxMarkKeys    = 4096
)

// NewStreamMarks returns an empty StreamMarks.
func NewStreamMarks() *StreamMarks { return &StreamMarks{marks: map[streamKey][]streamMark{}} }

// Note records that tokenID streamed filePath (in libraryID) through the
// transcoder at time at.
func (m *StreamMarks) Note(tokenID, libraryID int64, filePath string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.marks) >= maxMarkKeys {
		for k, v := range m.marks {
			if len(v) == 0 || at.Sub(v[len(v)-1].at) > SessionGap {
				delete(m.marks, k)
			}
		}
		if len(m.marks) >= maxMarkKeys {
			clear(m.marks)
		}
	}
	k := streamKey{tokenID, libraryID}
	list := append(fresh(m.marks[k], at), streamMark{filePath, at})
	if len(list) > maxMarksPerKey {
		list = list[len(list)-maxMarksPerKey:]
	}
	m.marks[k] = list
}

// Transcoding reports whether tokenID streamed a file of the book at bookPath
// through the transcoder within SessionGap of at.
func (m *StreamMarks) Transcoding(tokenID, libraryID int64, bookPath string, at time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mk := range fresh(m.marks[streamKey{tokenID, libraryID}], at) {
		if mk.path == bookPath || strings.HasPrefix(mk.path, bookPath+"/") {
			return true
		}
	}
	return false
}

// fresh drops marks older than SessionGap (marks are kept oldest first).
func fresh(list []streamMark, at time.Time) []streamMark {
	for len(list) > 0 && at.Sub(list[0].at) > SessionGap {
		list = list[1:]
	}
	return list
}

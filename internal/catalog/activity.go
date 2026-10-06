package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Activity is the admin console's Activity page for one period: listening
// (from sessions, and from listening_daily for days past the raw retention),
// completion (from progress), the devices in use (from tokens) and the state of
// the collection. Listening is bucketed in server time (loc).
type Activity struct {
	Period
	Totals ActivityTotals `json:"totals"`
	// Estimated is how much of Totals.Listened is an estimate (seconds): listening
	// from before the server recorded sessions that the players' spans didn't
	// cover (migration 0021). It is in the totals and the top lists, never in Days
	// or HourWeekday.
	Estimated float64 `json:"estimated"`
	// Previous is the same length of time just before From, for the deltas.
	Previous ActivityTotals `json:"previous"`
	// Days has one entry per day of the period, oldest first, zeros included.
	Days []ActivityDay `json:"days"`
	// HourWeekday is listened seconds by weekday (0 = Monday) and hour of day,
	// from raw sessions only (a rolled-up day has no hours).
	HourWeekday    [7][24]float64  `json:"hour_weekday"`
	TopBooks       []TopBook       `json:"top_books"`
	TopAuthors     []TopPerson     `json:"top_authors"`
	TopNarrators   []TopPerson     `json:"top_narrators"`
	TopUsers       []TopUser       `json:"top_users"`
	Funnel         Funnel          `json:"funnel"`
	DropOffs       []DropOff       `json:"drop_offs"`
	Playback       []PlaybackShare `json:"playback"`
	PeakConcurrent Peak            `json:"peak_concurrent"`
	Clients        []ClientCount   `json:"clients"`
	Growth         []GrowthPoint   `json:"growth"`
	Storage        Storage         `json:"storage"`
	Coverage       Coverage        `json:"coverage"`
	InactiveUsers  []InactiveUser  `json:"inactive_users"`
}

// Period labels a stats period (the Activity page, a person's stats): the range
// asked for ("7d", "30d", "90d", "1y", or a year, "2025"), its bounds (RFC3339
// UTC) and the server's zone, which days, hours and weekdays are counted in.
type Period struct {
	Range    string `json:"range"`
	From     string `json:"from"`
	To       string `json:"to"`
	Timezone string `json:"timezone"` // the server's zone abbreviation at To
	// UTCOffset is the server's offset from UTC at To, in minutes.
	UTCOffset int `json:"utc_offset"`
}

func periodOf(label string, from, to time.Time, loc *time.Location) Period {
	zone, offset := to.In(loc).Zone()
	return Period{Range: label, From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339),
		Timezone: zone, UTCOffset: offset / 60}
}

// ActivityTotals sums a period. Listened is wall-clock seconds; Sessions counts
// the sessions that started in the period; Finished counts books finished in it.
type ActivityTotals struct {
	Listened  float64 `json:"listened"`
	Sessions  int     `json:"sessions"`
	Listeners int     `json:"listeners"`
	Books     int     `json:"books"`
	Finished  int     `json:"finished"`
}

// ActivityDay is one day's listening, in total and per listener.
type ActivityDay struct {
	Date     string        `json:"date"` // YYYY-MM-DD, server time
	Listened float64       `json:"listened"`
	ByUser   []UserSeconds `json:"by_user"`
}

// UserSeconds is one listener's share of a day.
type UserSeconds struct {
	UserID   int64   `json:"user_id"`
	Listened float64 `json:"listened"`
}

// TopBook is a book by listening time in the period.
type TopBook struct {
	LibraryID int64   `json:"library_id"`
	Path      string  `json:"path"`
	Title     string  `json:"title"`
	Author    string  `json:"author"`
	Listened  float64 `json:"listened"`
	Listeners int     `json:"listeners"`
}

// TopPerson is an author or narrator (the whole field value, as the Library
// aggregates count them) by listening time.
type TopPerson struct {
	Name     string  `json:"name"`
	Listened float64 `json:"listened"`
	Books    int     `json:"books"`
}

// TopUser is a listener's period.
type TopUser struct {
	UserID   int64   `json:"user_id"`
	Username string  `json:"username"`
	Listened float64 `json:"listened"`
	Sessions int     `json:"sessions"`
	Books    int     `json:"books"`
	Finished int     `json:"finished"`
}

// Funnel counts the people x books with a progress save in the period, by how
// far each got (the current position, so a restarted book counts where it is).
type Funnel struct {
	Started   int `json:"started"`
	Reached25 int `json:"reached_25"`
	Reached50 int `json:"reached_50"`
	Reached75 int `json:"reached_75"`
	Finished  int `json:"finished"`
}

// DropOff is a chapter where several people stopped the same book: unfinished,
// with no save for dropOffIdle. ScanError says the book has a read problem the
// Health page lists, which is often why.
type DropOff struct {
	LibraryID    int64  `json:"library_id"`
	Path         string `json:"path"`
	Title        string `json:"title"`
	ChapterIndex int    `json:"chapter_index"`
	Chapter      string `json:"chapter"`
	Listeners    int    `json:"listeners"`
	ScanError    bool   `json:"scan_error"`
}

// PlaybackShare is listening by how it played: direct or through the transcoder,
// per codec ("" = unknown).
type PlaybackShare struct {
	Transcoded bool    `json:"transcoded"`
	Codec      string  `json:"codec"`
	Listened   float64 `json:"listened"`
	Sessions   int     `json:"sessions"`
}

// Peak is the most sessions open at once in the period, and when (null with none).
type Peak struct {
	Streams int     `json:"streams"`
	At      *string `json:"at"`
}

// ClientCount is how many devices listened in the period with one app build (as
// each session recorded it, so a later upgrade doesn't rewrite the past). App ""
// is a client that never named itself (released before the header).
type ClientCount struct {
	App      string `json:"app"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Devices  int    `json:"devices"`
}

// GrowthPoint is the number of books indexed now that had appeared on disk by
// Date (books removed since are not counted back).
type GrowthPoint struct {
	Date  string `json:"date"`
	Books int    `json:"books"`
}

// Storage is the collection's size by library, file format and codec.
type Storage struct {
	Bytes     int64            `json:"bytes"`
	ByLibrary []StorageLibrary `json:"by_library"`
	ByFormat  []StorageGroup   `json:"by_format"`
	ByCodec   []StorageGroup   `json:"by_codec"`
}

// StorageLibrary is one library's share.
type StorageLibrary struct {
	LibraryID int64  `json:"library_id"`
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	Books     int    `json:"books"`
}

// StorageGroup is one format's or codec's share ("" = unknown).
type StorageGroup struct {
	Key   string `json:"key"`
	Bytes int64  `json:"bytes"`
	Books int    `json:"books"`
}

// Coverage counts the books with an identifier (ASIN or ISBN, the matched= rule),
// with chapters (more than one) and with a cover.
type Coverage struct {
	Books        int `json:"books"`
	Identified   int `json:"identified"`
	WithChapters int `json:"with_chapters"`
	WithCover    int `json:"with_cover"`
}

// InactiveUser is an enabled account with no activity for inactiveAfter (or
// never, if it is older than that).
type InactiveUser struct {
	UserID     int64   `json:"user_id"`
	Username   string  `json:"username"`
	LastSeenAt *string `json:"last_seen_at"`
}

const (
	topLimit      = 10
	dropOffIdle   = 30 * 24 * time.Hour
	dropOffLimit  = 5
	inactiveAfter = 60 * 24 * time.Hour
)

// dayList is every day of the period, oldest first, zeros included, with each
// listener's share.
func (a *listenAcc) dayList() []ActivityDay {
	out := []ActivityDay{}
	for d := a.from.In(a.loc); ; d = d.AddDate(0, 0, 1) {
		day := d.Format(time.DateOnly)
		if day > a.lastDay {
			break
		}
		entry := ActivityDay{Date: day, ByUser: []UserSeconds{}}
		for user, secs := range a.days[day] {
			entry.Listened += secs
			entry.ByUser = append(entry.ByUser, UserSeconds{UserID: user, Listened: secs})
		}
		slices.SortFunc(entry.ByUser, func(x, y UserSeconds) int { return cmp.Compare(x.UserID, y.UserID) })
		out = append(out, entry)
	}
	return out
}

// ListeningDays is a period's listening day by day and nothing else: the year
// calendar and a person's listening year, without the rest of the Activity page.
type ListeningDays struct {
	Period
	Days []ActivityDay `json:"days"`
}

// ListeningDaysFor is the listening per day in [from, to) (server time, loc), of
// one user or of everyone (userID 0): the same days ActivityFor reports, without
// the stats around them.
func (c *Catalog) ListeningDaysFor(ctx context.Context, label string, from, to time.Time, loc *time.Location, userID int64) (*ListeningDays, error) {
	acc := newListenAcc(from, to, loc, listenDays)
	acc.onlyUser = userID
	return c.listeningDays(ctx, label, acc)
}

// listeningDays collects acc (a listenDays accumulator) and answers its days.
func (c *Catalog) listeningDays(ctx context.Context, label string, acc *listenAcc) (*ListeningDays, error) {
	if err := c.collectListening(ctx, acc); err != nil {
		return nil, err
	}
	return &ListeningDays{Period: periodOf(label, acc.from, acc.to, acc.loc), Days: acc.dayList()}, nil
}

// ActivityFor computes the Activity page for [from, to), labelled label. loc is
// the server's zone, which days, hours and weekdays are counted in.
func (c *Catalog) ActivityFor(ctx context.Context, label string, from, to time.Time, loc *time.Location) (*Activity, error) {
	out := &Activity{Period: periodOf(label, from, to, loc)}
	cur := newListenAcc(from, to, loc, listenAll)
	prev, err := c.listenPeriods(ctx, cur)
	if err != nil {
		return nil, err
	}
	finished, err := c.finishedByUser(ctx, from, to)
	if err != nil {
		return nil, err
	}
	prevFinished, err := c.finishedByUser(ctx, prev.from, prev.to)
	if err != nil {
		return nil, err
	}
	cur.result(out, finished)
	out.Previous = prev.totals(prevFinished)
	steps := []func(context.Context, *Activity, time.Time, time.Time) error{
		c.activityFunnel, c.activityDropOffs, c.activityGrowth,
		c.activityStorage, c.activityCoverage, c.activityInactive,
	}
	for _, step := range steps {
		if err := step(ctx, out, from, to); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// listenPeriods collects cur (a period's listening, in full) and returns the
// totals of the same length of time just before it, of the same listeners (one
// user's, or everyone's).
func (c *Catalog) listenPeriods(ctx context.Context, cur *listenAcc) (prev *listenAcc, err error) {
	if err := c.collectListening(ctx, cur); err != nil {
		return nil, err
	}
	prev = newListenAcc(cur.from.Add(-cur.to.Sub(cur.from)), cur.from, cur.loc, listenTotals)
	prev.onlyUser = cur.onlyUser
	// The current period takes the whole hour holding from, so the previous one
	// stops before it: that hour is counted once, not in both.
	prev.endHour = cur.firstHour
	if err := c.collectListening(ctx, prev); err != nil {
		return nil, err
	}
	return prev, nil
}

// listenLevel is how much a listenAcc keeps beyond the period's totals.
type listenLevel int

const (
	listenTotals listenLevel = iota // only the totals (the previous period)
	listenDays                      // the totals and each day's listening per user (the calendar)
	listenAll                       // everything the Activity page shows
)

// listenAcc accumulates listening over a period.
type listenAcc struct {
	from, to time.Time
	loc      *time.Location
	level    listenLevel
	// onlyUser keeps only this user's listening (0 = everyone's; a person's own
	// stats build theirs with newUserListenAcc).
	onlyUser int64

	listened  float64
	estimated float64 // the part of listened that is estimated (listening_daily.estimated)
	sessions  int
	listeners map[int64]bool
	books     map[Ref]*bookAcc
	days      map[string]map[int64]float64
	hw        [7][24]float64
	users     map[int64]*userAcc
	playback  map[playKey]*PlaybackShare
	intervals [][2]time.Time
	clients   map[Client]map[int64]bool // app build -> the devices (tokens) that listened with it
	firstHour time.Time
	endHour   time.Time // an hour starting at or after this is outside the period
	firstDay  string
	lastDay   string
}

type bookAcc struct {
	title, author, narrator, series string
	listened                        float64
	listeners                       map[int64]bool
}

type userAcc struct {
	name     string
	listened float64
	sessions int
	books    map[Ref]bool
}

type playKey struct {
	transcoded bool
	codec      string
}

func newListenAcc(from, to time.Time, loc *time.Location, level listenLevel) *listenAcc {
	f := from.In(loc)
	return &listenAcc{
		from: from, to: to, loc: loc, level: level,
		listeners: map[int64]bool{}, books: map[Ref]*bookAcc{}, days: map[string]map[int64]float64{},
		users:    map[int64]*userAcc{},
		playback: map[playKey]*PlaybackShare{}, clients: map[Client]map[int64]bool{},
		firstHour: hourOf(f),
		endHour:   to,
		firstDay:  f.Format(time.DateOnly),
		lastDay:   to.Add(-time.Nanosecond).In(loc).Format(time.DateOnly),
	}
}

// newUserListenAcc is newListenAcc for one person's listening only: the one way
// their own stats build an accumulator, and the one guard that a user id is
// given (onlyUser 0 would collect everyone's).
func newUserListenAcc(from, to time.Time, loc *time.Location, level listenLevel, userID int64) (*listenAcc, error) {
	if userID <= 0 {
		return nil, errNoUser
	}
	a := newListenAcc(from, to, loc, level)
	a.onlyUser = userID
	return a, nil
}

// listenRow is one raw session or one rolled-up day, as collectListening reads it.
type listenRow struct {
	user               int64
	username           string
	ref                Ref
	title, author, nar string
	series             string
}

// add credits secs of listening to a row on a local day (and, for a raw session,
// an hour). An estimate has no day ("") and stays out of the per-day listening.
func (a *listenAcc) add(r listenRow, day string, hour *time.Time, secs float64) {
	a.listened += secs
	a.listeners[r.user] = true
	b := a.books[r.ref]
	if b == nil {
		b = &bookAcc{title: r.title, author: r.author, narrator: r.nar, series: r.series, listeners: map[int64]bool{}}
		a.books[r.ref] = b
	}
	b.listened += secs
	b.listeners[r.user] = true
	if a.level < listenDays {
		return
	}
	if day != "" {
		if a.days[day] == nil {
			a.days[day] = map[int64]float64{}
		}
		a.days[day][r.user] += secs
	}
	if a.level < listenAll {
		return
	}
	if hour != nil {
		a.hw[(int(hour.Weekday())+6)%7][hour.Hour()] += secs
	}
	u := a.user(r)
	u.listened += secs
	u.books[r.ref] = true
}

func (a *listenAcc) user(r listenRow) *userAcc {
	u := a.users[r.user]
	if u == nil {
		u = &userAcc{name: r.username, books: map[Ref]bool{}}
		a.users[r.user] = u
	}
	return u
}

// countSession counts a session that started in the period (n of them for a
// rolled-up day).
func (a *listenAcc) countSession(r listenRow, n int) {
	a.sessions += n
	a.listeners[r.user] = true
	if a.level == listenAll {
		a.user(r).sessions += n
	}
}

// listenRowColumns are the names a listening row shows (listenRowJoins supplies
// them); listenRowBlanks stand in when only totals are wanted.
const (
	listenRowColumns = `u.username, COALESCE(b.title, ''), COALESCE(b.author, ''), COALESCE(b.narrator, ''), COALESCE(b.series, '')`
	listenRowBlanks  = `'', '', '', '', ''`
)

// listenRowJoins joins the user and the book of a listening table aliased t.
func listenRowJoins(t string) string {
	return `JOIN users u ON u.id = ` + t + `.user_id
		   LEFT JOIN books b ON b.library_id = ` + t + `.library_id AND b.rel_path = ` + t + `.rel_path`
}

// collectListening reads the raw sessions overlapping the period and the
// rolled-up days inside it.
func (c *Catalog) collectListening(ctx context.Context, a *listenAcc) error {
	if err := c.collectSessions(ctx, a); err != nil {
		return err
	}
	return c.collectDays(ctx, a)
}

// The periods of collectSessions and collectDays: everyone's, or one user's. Two
// spellings rather than one "(? = 0 OR user_id = ?)", which no index can serve:
// one user's reads by idx_sessions_user_last / idx_daily_user, everyone's by
// idx_sessions_last / idx_daily_day.
const (
	sessionsOfEveryone = ` WHERE s.started_at < ? AND s.last_at >= ? AND ` + listenedSQL
	sessionsOfUser     = ` WHERE s.user_id = ? AND s.started_at < ? AND s.last_at >= ? AND ` + listenedSQL
	daysOfEveryone     = ` WHERE d.day >= ? AND d.day <= ?`
	daysOfUser         = ` WHERE d.user_id = ? AND d.day >= ? AND d.day <= ?`
)

func (c *Catalog) collectSessions(ctx context.Context, a *listenAcc) error {
	cols, joins := listenRowColumns, listenRowJoins("s")
	if a.level < listenAll { // totals or days only: no names needed
		cols, joins = listenRowBlanks, ""
	}
	where, args := sessionsOfEveryone, []any{formatSessionTime(a.to), formatSessionTime(a.from)}
	if a.onlyUser != 0 {
		where, args = sessionsOfUser, append([]any{a.onlyUser}, args...)
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT s.user_id, s.library_id, s.rel_path, `+cols+`,
		        s.started_at, s.last_at, s.listened, s.codec, s.transcoded,
		        s.token_id, s.client_app, s.client_version, s.client_platform, s.backfilled
		   FROM listening_sessions s `+joins+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			r                    listenRow
			startS, lastS, codec string
			listened             float64
			transcoded           bool
			token                int64
			client               Client
			backfilled           bool
		)
		if err := rows.Scan(&r.user, &r.ref.LibraryID, &r.ref.Path, &r.username, &r.title, &r.author, &r.nar, &r.series,
			&startS, &lastS, &listened, &codec, &transcoded, &token, &client.App, &client.Version,
			&client.Platform, &backfilled); err != nil {
			return err
		}
		// A session backfilled from the players' spans never recorded its device,
		// app or playback mode: it counts as listening, not in those breakdowns.
		mode := &playKey{transcoded, codec}
		if backfilled {
			mode = nil
		}
		a.addSession(r, parseSessionTime(startS), parseSessionTime(lastS), listened, mode)
		if a.level == listenAll && !backfilled {
			if a.clients[client] == nil {
				a.clients[client] = map[int64]bool{}
			}
			a.clients[client][token] = true
		}
	}
	return rows.Err()
}

// addSession credits one raw session: its listening spread over the hours it
// covers (only the part inside the period counts), the session itself if it
// started in the period, its playback mode (unless mode is nil: not recorded)
// and its span for the peak.
func (a *listenAcc) addSession(r listenRow, start, last time.Time, listened float64, mode *playKey) {
	var inRange float64
	spreadListening(start, last, listened, a.loc, func(hour time.Time, secs float64) {
		if hour.Before(a.firstHour) || !hour.Before(a.endHour) {
			return
		}
		inRange += secs
		a.add(r, hour.Format(time.DateOnly), &hour, secs)
	})
	startedIn := !start.Before(a.from) && start.Before(a.to)
	if startedIn {
		a.countSession(r, 1)
	}
	if a.level < listenAll {
		return
	}
	if mode != nil && (inRange > 0 || startedIn) {
		p := a.playback[*mode]
		if p == nil {
			p = &PlaybackShare{Transcoded: mode.transcoded, Codec: mode.codec}
			a.playback[*mode] = p
		}
		p.Listened += inRange
		if startedIn {
			p.Sessions++
		}
	}
	s, e := maxTime(start, a.from), minTime(last, a.to)
	if !e.Before(s) {
		a.intervals = append(a.intervals, [2]time.Time{s, e})
	}
}

func (c *Catalog) collectDays(ctx context.Context, a *listenAcc) error {
	cols, joins := listenRowColumns, listenRowJoins("d")
	if a.level < listenAll {
		cols, joins = listenRowBlanks, ""
	}
	where, args := daysOfEveryone, []any{a.firstDay, a.lastDay}
	if a.onlyUser != 0 {
		where, args = daysOfUser, append([]any{a.onlyUser}, args...)
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT d.user_id, d.library_id, d.rel_path, `+cols+`, d.day, d.listened, d.sessions, d.estimated
		   FROM listening_daily d `+joins+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			r         listenRow
			day       string
			listened  float64
			sessions  int
			estimated bool
		)
		if err := rows.Scan(&r.user, &r.ref.LibraryID, &r.ref.Path, &r.username, &r.title, &r.author, &r.nar, &r.series,
			&day, &listened, &sessions, &estimated); err != nil {
			return err
		}
		if estimated {
			a.estimated += listened
			day = "" // no day of its own: in the totals and tops, not the day-by-day
		}
		a.add(r, day, nil, listened)
		a.countSession(r, sessions)
	}
	return rows.Err()
}

func (a *listenAcc) totals(finished map[int64]int) ActivityTotals {
	t := ActivityTotals{Listened: a.listened, Sessions: a.sessions, Listeners: len(a.listeners), Books: len(a.books)}
	for _, n := range finished {
		t.Finished += n
	}
	return t
}

// result writes the accumulated listening into out.
func (a *listenAcc) result(out *Activity, finished map[int64]int) {
	out.Totals = a.totals(finished)
	out.Estimated = a.estimated
	out.HourWeekday = a.hw
	out.Days = a.dayList()
	out.TopBooks = a.topBooks(nil)
	out.TopAuthors = a.topPeople(bookAuthor, nil)
	out.TopNarrators = a.topPeople(bookNarrator, nil)
	out.TopUsers = []TopUser{}
	for id, u := range a.users {
		out.TopUsers = append(out.TopUsers, TopUser{UserID: id, Username: u.name, Listened: u.listened,
			Sessions: u.sessions, Books: len(u.books), Finished: finished[id]})
	}
	out.TopUsers = topN(out.TopUsers, func(x, y TopUser) int {
		return cmp.Or(cmp.Compare(y.Listened, x.Listened), cmp.Compare(x.Username, y.Username))
	})
	out.Playback = a.playbackList()
	out.PeakConcurrent = peakConcurrent(a.intervals)
	out.Clients = a.clientList()
}

// topBooks ranks the books listened to, keeping only those keep allows (nil:
// all of them).
func (a *listenAcc) topBooks(keep func(Ref) bool) []TopBook {
	out := []TopBook{}
	for ref, b := range a.books {
		if keep != nil && !keep(ref) {
			continue
		}
		out = append(out, TopBook{LibraryID: ref.LibraryID, Path: ref.Path, Title: b.title,
			Author: b.author, Listened: b.listened, Listeners: len(b.listeners)})
	}
	return topN(out, func(x, y TopBook) int {
		return cmp.Or(cmp.Compare(y.Listened, x.Listened), cmp.Compare(x.Title, y.Title), cmp.Compare(x.Path, y.Path))
	})
}

func bookAuthor(b *bookAcc) string   { return b.author }
func bookNarrator(b *bookAcc) string { return b.narrator }
func bookSeries(b *bookAcc) string   { return b.series }

// topPeople ranks an author, narrator or series (name picks which; the whole
// field value, as the Library aggregates count them) by the listening of its
// books, counting only the books keep allows (nil: all of them).
func (a *listenAcc) topPeople(name func(*bookAcc) string, keep func(Ref) bool) []TopPerson {
	people := map[string]*TopPerson{}
	for ref, b := range a.books {
		n := name(b)
		if n == "" || (keep != nil && !keep(ref)) {
			continue
		}
		p := people[n]
		if p == nil {
			p = &TopPerson{Name: n}
			people[n] = p
		}
		p.Listened += b.listened
		p.Books++
	}
	out := make([]TopPerson, 0, len(people))
	for _, p := range people {
		out = append(out, *p)
	}
	return topN(out, func(x, y TopPerson) int {
		return cmp.Or(cmp.Compare(y.Listened, x.Listened), cmp.Compare(x.Name, y.Name))
	})
}

// playbackList is the listening by playback mode, most first.
func (a *listenAcc) playbackList() []PlaybackShare {
	out := []PlaybackShare{}
	for _, p := range a.playback {
		out = append(out, *p)
	}
	slices.SortFunc(out, func(x, y PlaybackShare) int {
		return cmp.Or(cmp.Compare(y.Listened, x.Listened), cmp.Compare(x.Codec, y.Codec))
	})
	return out
}

// clientList is the app builds listened with, by how many devices used each.
func (a *listenAcc) clientList() []ClientCount {
	out := []ClientCount{}
	for cl, devices := range a.clients {
		out = append(out, ClientCount{App: cl.App, Version: cl.Version, Platform: cl.Platform, Devices: len(devices)})
	}
	slices.SortFunc(out, func(x, y ClientCount) int {
		return cmp.Or(cmp.Compare(y.Devices, x.Devices), cmp.Compare(x.App, y.App),
			cmp.Compare(x.Version, y.Version), cmp.Compare(x.Platform, y.Platform))
	})
	return out
}

func topN[T any](s []T, order func(a, b T) int) []T {
	slices.SortFunc(s, order)
	if len(s) > topLimit {
		s = s[:topLimit]
	}
	return s
}

// peakConcurrent finds the most intervals open at once. An interval that ends
// at the moment another starts doesn't overlap it (a device moving to the next
// book).
func peakConcurrent(intervals [][2]time.Time) Peak {
	type event struct {
		at    time.Time
		delta int
	}
	events := make([]event, 0, 2*len(intervals))
	for _, iv := range intervals {
		events = append(events, event{iv[0], 1}, event{iv[1], -1})
	}
	slices.SortFunc(events, func(x, y event) int {
		return cmp.Or(x.at.Compare(y.at), cmp.Compare(x.delta, y.delta))
	})
	peak, open := Peak{}, 0
	for _, e := range events {
		open += e.delta
		if open > peak.Streams {
			peak.Streams = open
			at := e.at.UTC().Format(time.RFC3339)
			peak.At = &at
		}
	}
	return peak
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// finishedByUser counts the books each user finished in [from, to).
func (c *Catalog) finishedByUser(ctx context.Context, from, to time.Time) (map[int64]int, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT user_id, COUNT(*) FROM progress
		  WHERE finished = 1 AND finished_at >= ? AND finished_at < ? GROUP BY user_id`,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var (
			id int64
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (c *Catalog) activityFunnel(ctx context.Context, out *Activity, from, to time.Time) error {
	rows, err := c.db.QueryContext(ctx,
		`SELECT position, duration, finished FROM progress WHERE updated_at >= ? AND updated_at < ?`,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			pos, dur float64
			finished bool
		)
		if err := rows.Scan(&pos, &dur, &finished); err != nil {
			return err
		}
		frac := 0.0
		if dur > 0 {
			frac = pos / dur
		}
		if finished {
			frac = 1
		}
		out.Funnel.Started++
		for _, step := range []struct {
			at float64
			n  *int
		}{{0.25, &out.Funnel.Reached25}, {0.5, &out.Funnel.Reached50}, {0.75, &out.Funnel.Reached75}} {
			if frac >= step.at {
				(*step.n)++
			}
		}
		if finished {
			out.Funnel.Finished++
		}
	}
	return rows.Err()
}

// activityDropOffs finds chapters where at least two people stopped the same
// book. It doesn't depend on the period: a drop-off is a book nobody came back to.
func (c *Catalog) activityDropOffs(ctx context.Context, out *Activity, _, _ time.Time) error {
	out.DropOffs = []DropOff{}
	idle := c.now().Add(-dropOffIdle).UTC().Format(time.RFC3339)
	stalled, err := c.stalledBooks(ctx, idle)
	if err != nil || len(stalled) == 0 {
		return err
	}
	chapters, err := c.stalledChapters(ctx, idle)
	if err != nil {
		return err
	}
	for _, s := range stalled {
		chs := chapters[s.bookID]
		counts := map[int]int{}
		for _, pos := range s.positions {
			counts[max(chapterAt(chs, pos), 0)]++
		}
		for idx, n := range counts {
			if n < 2 {
				continue
			}
			d := s.drop
			d.ChapterIndex, d.Listeners = idx, n
			if idx < len(chs) {
				d.Chapter = chs[idx].Title
			}
			out.DropOffs = append(out.DropOffs, d)
		}
	}
	slices.SortFunc(out.DropOffs, func(x, y DropOff) int {
		return cmp.Or(cmp.Compare(y.Listeners, x.Listeners), cmp.Compare(x.Title, y.Title),
			cmp.Compare(x.LibraryID, y.LibraryID), cmp.Compare(x.Path, y.Path),
			cmp.Compare(x.ChapterIndex, y.ChapterIndex))
	})
	if len(out.DropOffs) > dropOffLimit {
		out.DropOffs = out.DropOffs[:dropOffLimit]
	}
	return nil
}

// stalledWhere selects unfinished progress with no save since ?1 (the idle
// cutoff), joined to its book; stalledBookIDs narrows it to books at least two
// people left that way, the only ones a drop-off can be found in.
const (
	stalledWhere = ` FROM progress p JOIN books b ON b.library_id = p.library_id AND b.rel_path = p.rel_path
	  WHERE p.finished = 0 AND p.position > 0 AND p.updated_at < ?1`
	stalledBookIDs = `SELECT b.id` + stalledWhere + ` GROUP BY b.id HAVING COUNT(*) >= 2`
)

// stalledBook is a book with the positions where people left it unfinished.
type stalledBook struct {
	bookID    int64
	drop      DropOff
	positions []float64
}

// stalledBooks reads the stalled positions of books two or more people left
// unfinished, one entry per book.
func (c *Catalog) stalledBooks(ctx context.Context, idle string) ([]*stalledBook, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT b.id, b.library_id, b.rel_path, b.title, b.scan_error <> '', p.position`+stalledWhere+`
		    AND b.id IN (`+stalledBookIDs+`) ORDER BY b.id`, idle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*stalledBook
	for rows.Next() {
		var (
			id  int64
			d   DropOff
			pos float64
		)
		if err := rows.Scan(&id, &d.LibraryID, &d.Path, &d.Title, &d.ScanError, &pos); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].bookID != id {
			out = append(out, &stalledBook{bookID: id, drop: d})
		}
		last := out[len(out)-1]
		last.positions = append(last.positions, pos)
	}
	return out, rows.Err()
}

// stalledChapters loads the chapters (title and start) of the same books, in one
// query, keyed by book id.
func (c *Catalog) stalledChapters(ctx context.Context, idle string) (map[int64][]metadata.Chapter, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT book_id, idx, title, book_offset FROM chapters
		  WHERE book_id IN (`+stalledBookIDs+`) ORDER BY book_id, idx`, idle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]metadata.Chapter{}
	for rows.Next() {
		var (
			id int64
			ch metadata.Chapter
		)
		if err := rows.Scan(&id, &ch.Index, &ch.Title, &ch.BookOffset); err != nil {
			return nil, err
		}
		out[id] = append(out[id], ch)
	}
	return out, rows.Err()
}

// activityGrowth samples the book count across the period: daily up to a month,
// weekly up to a quarter, monthly beyond. One count at the start, then the
// period's additions in order, so the cost doesn't grow with the samples.
func (c *Catalog) activityGrowth(ctx context.Context, out *Activity, from, to time.Time) error {
	step := func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }
	switch span := to.Sub(from); {
	case span <= 31*24*time.Hour:
		step = func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	case span <= 92*24*time.Hour:
		step = func(t time.Time) time.Time { return t.AddDate(0, 0, 7) }
	}
	fromS, toS := from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE added_at <= ?`, fromS).Scan(&n); err != nil {
		return err
	}
	added, err := queryRows(ctx, c.db, func(rows *sql.Rows, v *string) error { return rows.Scan(v) },
		`SELECT added_at FROM books WHERE added_at > ? AND added_at <= ? ORDER BY added_at`, fromS, toS)
	if err != nil {
		return err
	}
	out.Growth = []GrowthPoint{}
	for t := from; ; t = step(t) {
		if t.After(to) {
			t = to
		}
		at := t.UTC().Format(time.RFC3339)
		for len(added) > 0 && added[0] <= at {
			n++
			added = added[1:]
		}
		out.Growth = append(out.Growth, GrowthPoint{Date: at, Books: n})
		if !t.Before(to) {
			return nil
		}
	}
}

// activityStorage sizes the collection by library, format and codec from one
// grouped pass over the books (a library with no books still gets its zero row).
func (c *Catalog) activityStorage(ctx context.Context, out *Activity, _, _ time.Time) error {
	type group struct {
		lib           StorageLibrary
		format, codec string
	}
	groups, err := queryRows(ctx, c.db, func(rows *sql.Rows, g *group) error {
		return rows.Scan(&g.lib.LibraryID, &g.lib.Name, &g.format, &g.codec, &g.lib.Bytes, &g.lib.Books)
	}, `SELECT l.id, l.name, COALESCE(b.format, ''), COALESCE(b.codec, ''), COALESCE(SUM(b.size), 0), COUNT(b.id)
	      FROM libraries l LEFT JOIN books b ON b.library_id = l.id
	     GROUP BY l.id, b.format, b.codec ORDER BY l.sort_order, l.id`)
	if err != nil {
		return err
	}
	out.Storage = Storage{ByLibrary: []StorageLibrary{}}
	formats, codecs := map[string]*StorageGroup{}, map[string]*StorageGroup{}
	add := func(m map[string]*StorageGroup, key string, g group) {
		if m[key] == nil {
			m[key] = &StorageGroup{Key: key}
		}
		m[key].Bytes += g.lib.Bytes
		m[key].Books += g.lib.Books
	}
	for _, g := range groups {
		out.Storage.Bytes += g.lib.Bytes
		if n := len(out.Storage.ByLibrary); n > 0 && out.Storage.ByLibrary[n-1].LibraryID == g.lib.LibraryID {
			out.Storage.ByLibrary[n-1].Bytes += g.lib.Bytes
			out.Storage.ByLibrary[n-1].Books += g.lib.Books
		} else {
			out.Storage.ByLibrary = append(out.Storage.ByLibrary, g.lib)
		}
		if g.lib.Books > 0 {
			add(formats, g.format, g)
			add(codecs, g.codec, g)
		}
	}
	out.Storage.ByFormat, out.Storage.ByCodec = sortedGroups(formats), sortedGroups(codecs)
	return nil
}

// sortedGroups lists storage groups largest first.
func sortedGroups(m map[string]*StorageGroup) []StorageGroup {
	out := make([]StorageGroup, 0, len(m))
	for _, g := range m {
		out = append(out, *g)
	}
	slices.SortFunc(out, func(x, y StorageGroup) int {
		return cmp.Or(cmp.Compare(y.Bytes, x.Bytes), cmp.Compare(x.Key, y.Key))
	})
	return out
}

func (c *Catalog) activityCoverage(ctx context.Context, out *Activity, _, _ time.Time) error {
	return c.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(`+matchedExpr+`), 0), COALESCE(SUM(`+hasChaptersExpr+`), 0),
		        COALESCE(SUM(`+hasCoverExpr+`), 0) FROM books b`).
		Scan(&out.Coverage.Books, &out.Coverage.Identified, &out.Coverage.WithChapters, &out.Coverage.WithCover)
}

// activityInactive lists enabled, non-demo accounts with no authenticated request
// for inactiveAfter (accounts that never made one count once they are that old).
// It doesn't depend on the period.
func (c *Catalog) activityInactive(ctx context.Context, out *Activity, _, _ time.Time) error {
	var err error
	out.InactiveUsers, err = queryRows(ctx, c.db, func(rows *sql.Rows, u *InactiveUser) error {
		return rows.Scan(&u.UserID, &u.Username, &u.LastSeenAt)
	}, `SELECT id, username, last_seen FROM (
	        SELECT u.id, u.username, u.created_at,
	               (SELECT MAX(t.last_seen) FROM tokens t WHERE t.user_id = u.id) AS last_seen
	          FROM users u WHERE u.disabled = 0 AND u.is_demo = 0)
	     WHERE COALESCE(last_seen, created_at) < ?
	     ORDER BY COALESCE(last_seen, created_at), id`, c.now().Add(-inactiveAfter).UTC().Format(time.RFC3339))
	return err
}

// ErrInvalidRange is returned by ParseActivityRange for a range it doesn't know.
var ErrInvalidRange = errors.New("invalid range")

// ParseActivityRange turns the Activity page's period into [from, to): a
// trailing range ("7d", "30d", "90d", "1y") ending now, or a calendar year in
// server time ("2025"; the current year ends now; "year" is the current one, and
// the label names it). An empty value is "30d".
// "Now" is rounded up to the next whole second, so a session recorded in the
// same instant as the request still falls inside the period.
func ParseActivityRange(v string, now time.Time, loc *time.Location) (label string, from, to time.Time, err error) {
	days := map[string]int{"7d": 7, "30d": 30, "90d": 90, "1y": 365}
	now = now.Truncate(time.Second).Add(time.Second)
	if v == "" {
		v = "30d"
	}
	if v == "year" { // this calendar year, in server time (the browser's may differ)
		v = now.In(loc).Format("2006")
	}
	if n, ok := days[v]; ok {
		return v, now.Add(-time.Duration(n) * 24 * time.Hour), now, nil
	}
	if len(v) == 4 {
		if y, perr := time.ParseInLocation("2006", v, loc); perr == nil && y.Year() >= 2000 && !y.After(now) {
			end := y.AddDate(1, 0, 0)
			if end.After(now) {
				end = now
			}
			return v, y, end, nil
		}
	}
	return "", time.Time{}, time.Time{}, ErrInvalidRange
}

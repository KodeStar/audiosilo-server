package catalog

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// A person's own listening stats (player redesign Phase 1b, GET /me/stats,
// /me/listening, /me/goal): the admin Activity page's listening, computed for one
// user only. PRIVACY: nothing here may carry another user. The types have no
// per-listener field (no listeners, by_user, user ids or usernames), every query
// is filtered to the caller, and the rows naming a book (top books, finished
// books, and the authors, narrators and series ranked from books) pass through
// the caller's CURRENT access, so a revoked share's path, title or author never
// echoes back. The totals, days and hours are the caller's own time, all of it.

// errNoUser refuses a person's own stats for no user (newUserListenAcc,
// GoalStatusFor): user 0 means "everyone" to the listening accumulator, which
// must never reach a person's own stats.
var errNoUser = errors.New("catalog: personal stats need a user")

// UserStats is one person's listening over a period.
type UserStats struct {
	Period
	Totals UserTotals `json:"totals"`
	// Previous is the same length of time just before From, for the deltas.
	Previous UserTotals `json:"previous"`
	// Estimated is how much of Totals.Listened is an estimate (seconds), as on the
	// Activity page: in the totals and the tops, never in Days or HourWeekday.
	Estimated float64 `json:"estimated"`
	// Days has one entry per day of the period, oldest first, zeros included.
	Days []ListeningDay `json:"days"`
	// HourWeekday is listened seconds by weekday (0 = Monday) and hour of day,
	// from raw sessions only.
	HourWeekday   [7][24]float64 `json:"hour_weekday"`
	TopBooks      []UserTopBook  `json:"top_books"`
	TopAuthors    []TopPerson    `json:"top_authors"`
	TopNarrators  []TopPerson    `json:"top_narrators"`
	TopSeries     []TopPerson    `json:"top_series"`
	FinishedBooks []FinishedBook `json:"finished_books"`
	// Playback is the person's listening by playback mode; Clients the app builds
	// on their own devices.
	Playback []PlaybackShare `json:"playback"`
	Clients  []ClientCount   `json:"clients"`
}

// UserTotals sums a person's period: Listened is wall-clock seconds, Sessions the
// sessions started in it, Books the books listened to, Finished the books
// finished in it.
type UserTotals struct {
	Listened float64 `json:"listened"`
	Sessions int     `json:"sessions"`
	Books    int     `json:"books"`
	Finished int     `json:"finished"`
}

// ListeningDay is one day's listening (YYYY-MM-DD, server time).
type ListeningDay struct {
	Date     string  `json:"date"`
	Listened float64 `json:"listened"`
}

// UserTopBook is a book by the person's listening time.
type UserTopBook struct {
	LibraryID int64   `json:"library_id"`
	Path      string  `json:"path"`
	Title     string  `json:"title"`
	Author    string  `json:"author"`
	Listened  float64 `json:"listened"`
}

// FinishedBook is a book the person finished in the period.
type FinishedBook struct {
	LibraryID  int64  `json:"library_id"`
	Path       string `json:"path"`
	Title      string `json:"title"`
	Author     string `json:"author"`
	FinishedAt string `json:"finished_at"`
}

// finishedBooksLimit bounds UserStats.FinishedBooks (newest first).
const finishedBooksLimit = 100

// UserStatsFor computes userID's own listening over [from, to), labelled label,
// in the server's zone loc. scopes is the user's current access (UserScopes):
// the rows naming a book keep only the books it grants.
func (c *Catalog) UserStatsFor(ctx context.Context, label string, from, to time.Time, loc *time.Location, userID int64, scopes []Scope) (*UserStats, error) {
	cur, err := newUserListenAcc(from, to, loc, listenAll, userID)
	if err != nil {
		return nil, err
	}
	prev, err := c.listenPeriods(ctx, cur)
	if err != nil {
		return nil, err
	}
	finished, err := c.finishedCount(ctx, userID, from, to)
	if err != nil {
		return nil, err
	}
	prevFinished, err := c.finishedCount(ctx, userID, prev.from, prev.to)
	if err != nil {
		return nil, err
	}
	books, err := c.finishedBooks(ctx, userID, from, to, scopes)
	if err != nil {
		return nil, err
	}
	keep := func(ref Ref) bool { return scopesAllow(scopes, ref) }
	out := &UserStats{
		Period:   periodOf(label, from, to, loc),
		Totals:   userTotals(cur, finished),
		Previous: userTotals(prev, prevFinished), Estimated: cur.estimated,
		Days: listeningDayList(cur.dayList()), HourWeekday: cur.hw,
		TopBooks:      []UserTopBook{},
		TopAuthors:    cur.topPeople(bookAuthor, keep),
		TopNarrators:  cur.topPeople(bookNarrator, keep),
		TopSeries:     cur.topPeople(bookSeries, keep),
		FinishedBooks: books,
		Playback:      cur.playbackList(),
		Clients:       cur.clientList(),
	}
	for _, b := range cur.topBooks(keep) {
		out.TopBooks = append(out.TopBooks, UserTopBook{LibraryID: b.LibraryID, Path: b.Path, Title: b.Title,
			Author: b.Author, Listened: b.Listened})
	}
	return out, nil
}

// userTotals is a one-user accumulator's totals (no listener count) with the
// books they finished in its period.
func userTotals(a *listenAcc, finished int) UserTotals {
	return UserTotals{Listened: a.listened, Sessions: a.sessions, Books: len(a.books), Finished: finished}
}

// listeningDayList is days without the per-listener split.
func listeningDayList(days []ActivityDay) []ListeningDay {
	out := make([]ListeningDay, len(days))
	for i, d := range days {
		out[i] = ListeningDay{Date: d.Date, Listened: d.Listened}
	}
	return out
}

// finishedCount counts the books userID finished in [from, to) (the progress
// primary key serves it: user_id leads).
func (c *Catalog) finishedCount(ctx context.Context, userID int64, from, to time.Time) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM progress WHERE user_id = ? AND finished = 1 AND finished_at >= ? AND finished_at < ?`,
		userID, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)).Scan(&n)
	return n, err
}

// finishedBooks lists the books userID finished in [from, to) that scopes still
// grants, newest first, at most finishedBooksLimit.
func (c *Catalog) finishedBooks(ctx context.Context, userID int64, from, to time.Time, scopes []Scope) ([]FinishedBook, error) {
	filter, fargs := scopesFilterSQL("p.library_id", "p.rel_path", scopes)
	args := append([]any{userID, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)}, fargs...)
	args = append(args, finishedBooksLimit)
	return queryRows(ctx, c.db, func(rows *sql.Rows, b *FinishedBook) error {
		return rows.Scan(&b.LibraryID, &b.Path, &b.Title, &b.Author, &b.FinishedAt)
	}, `SELECT p.library_id, p.rel_path, COALESCE(b.title, ''), COALESCE(b.author, ''), p.finished_at
	      FROM progress p LEFT JOIN books b ON b.library_id = p.library_id AND b.rel_path = p.rel_path
	     WHERE p.user_id = ? AND p.finished = 1 AND p.finished_at >= ? AND p.finished_at < ? AND `+filter+`
	     ORDER BY p.finished_at DESC, p.library_id, p.rel_path LIMIT ?`, args...)
}

// UserListening is one person's listening per day over a period (the client
// works the streaks out from it).
type UserListening struct {
	Period
	Days []ListeningDay `json:"days"`
}

// UserListeningFor is userID's own listening per day in [from, to) (server time,
// loc): the days of ListeningDaysFor for that user, without the per-listener split.
func (c *Catalog) UserListeningFor(ctx context.Context, label string, from, to time.Time, loc *time.Location, userID int64) (*UserListening, error) {
	acc, err := newUserListenAcc(from, to, loc, listenDays, userID)
	if err != nil {
		return nil, err
	}
	days, err := c.listeningDays(ctx, label, acc)
	if err != nil {
		return nil, err
	}
	return &UserListening{Period: days.Period, Days: listeningDayList(days.Days)}, nil
}

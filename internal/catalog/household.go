package catalog

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"time"
)

// Listening in the house (player redesign Phase 8, capability household): people
// on one server who choose to share it see what the others who share are
// listening to, on their Home page. Privacy rules, all enforced here:
//
//   - opt-in: off until a person turns it on (household_sharing, no row = off);
//   - reciprocal: a person who doesn't share sees nobody (Listening is empty);
//   - only other people who share, whose account is enabled and not a demo;
//   - only a session recorded on this server (not imported, not backfilled) with
//     listening in it, the newest per person, within HouseWindow;
//   - only a book the CALLER may see: a person's newest book outside the caller's
//     scopes drops that person, never falls back to an older book;
//   - never a device, an address, an app or a token: HouseListeningRow has no
//     field that could carry one (unlike the admin's LiveSessions).

// HouseWindow is how far back a housemate's newest session may be to show.
const HouseWindow = 24 * time.Hour

// House listening states, from the age of the housemate's newest save.
const (
	HousePlaying = SessionPlaying // a save within livePlaying
	HousePaused  = SessionPaused  // within SessionGap
	HouseRecent  = "recent"       // within HouseWindow
)

// HouseholdSettings is a person's own household choice (GET/PUT /me/household).
type HouseholdSettings struct {
	ShareListening bool `json:"share_listening"`
}

// HouseListening is GET /household/listening: whether the caller shares, and
// (only then) what the other people who share are listening to, newest first.
type HouseListening struct {
	Sharing   bool                `json:"sharing"`
	Listening []HouseListeningRow `json:"listening"`
}

// HouseListeningRow is one housemate's newest book. Author, Duration, Chapter and
// ChapterIndex are null when unknown (Chapter is also null for a chapter whose
// title names nothing, where ChapterIndex still says which; a book with one
// chapter has neither). UpdatedAt is the newest save, fixed-width millisecond UTC.
type HouseListeningRow struct {
	UserID       int64    `json:"user_id"`
	Username     string   `json:"username"`
	LibraryID    int64    `json:"library_id"`
	Path         string   `json:"path"`
	Title        string   `json:"title"`
	Author       *string  `json:"author"`
	State        string   `json:"state"`
	Position     float64  `json:"position"`
	Duration     *float64 `json:"duration"`
	Chapter      *string  `json:"chapter"`
	ChapterIndex *int     `json:"chapter_index"`
	UpdatedAt    string   `json:"updated_at"`
}

// HouseholdSharing reports whether userID shares their listening.
func (c *Catalog) HouseholdSharing(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, errNoUser
	}
	var on bool
	err := c.db.QueryRowContext(ctx,
		`SELECT share_listening FROM household_sharing WHERE user_id = ?`, userID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return on, err
}

// SetHouseholdSharing records whether userID shares their listening. The caller
// refuses demo accounts (a public demo must not show up in anyone's house).
func (c *Catalog) SetHouseholdSharing(ctx context.Context, userID int64, share bool) error {
	if userID <= 0 {
		return errNoUser
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO household_sharing(user_id, share_listening, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET share_listening = excluded.share_listening, updated_at = excluded.updated_at`,
		userID, share, c.stamp())
	return err
}

// HouseListening answers GET /household/listening for callerID, whose current
// scopes (UserScopes) are callerScopes. See the rules above.
func (c *Catalog) HouseListening(ctx context.Context, callerID int64, callerScopes []Scope) (*HouseListening, error) {
	sharing, err := c.HouseholdSharing(ctx, callerID)
	if err != nil {
		return nil, err
	}
	out := &HouseListening{Sharing: sharing, Listening: []HouseListeningRow{}}
	if !sharing {
		return out, nil
	}
	type row struct {
		HouseListeningRow
		author   string
		duration float64
		bookID   int64
	}
	// Each housemate's newest session with listening in it (ROW_NUMBER over the
	// window's sessions, read by idx_sessions_last), then the scope check in Go:
	// a newest book the caller can't see drops the person rather than letting an
	// older one stand in.
	rows, err := queryRows(ctx, c.db, func(rs *sql.Rows, r *row) error {
		return rs.Scan(&r.UserID, &r.Username, &r.LibraryID, &r.Path, &r.Title, &r.author,
			&r.Position, &r.duration, &r.UpdatedAt, &r.bookID)
	}, `SELECT user_id, username, library_id, rel_path, title, author, end_pos, duration, last_at, book_id FROM (
	      SELECT s.user_id, u.username, s.library_id, s.rel_path, COALESCE(b.title, '') AS title,
	             COALESCE(b.author, '') AS author, s.end_pos,
	             COALESCE(NULLIF(b.duration, 0), s.duration) AS duration, s.last_at,
	             COALESCE(b.id, 0) AS book_id,
	             ROW_NUMBER() OVER (PARTITION BY s.user_id ORDER BY s.last_at DESC, s.id DESC) AS n
	        FROM listening_sessions s
	        JOIN users u ON u.id = s.user_id AND u.disabled = 0 AND u.is_demo = 0
	        JOIN household_sharing h ON h.user_id = s.user_id AND h.share_listening = 1
	        LEFT JOIN books b ON b.library_id = s.library_id AND b.rel_path = s.rel_path
	       WHERE s.last_at >= ? AND s.user_id <> ? AND s.import_id = 0 AND s.backfilled = 0 AND `+listenedSQL+`
	    ) WHERE n = 1 ORDER BY last_at DESC, user_id`,
		formatSessionTime(c.now().Add(-HouseWindow)), callerID)
	if err != nil {
		return nil, err
	}
	chapters := c.newChapterCache()
	for _, r := range rows {
		if !ScopesAllow(callerScopes, Ref{LibraryID: r.LibraryID, Path: r.Path}) {
			continue
		}
		if r.Title == "" {
			r.Title = path.Base(r.Path) // not indexed (any more): the path names it
		}
		if r.author != "" {
			r.Author = &r.author
		}
		if r.duration > 0 {
			r.Duration = &r.duration
		}
		title, idx, err := chapters.at(ctx, r.bookID, r.Position)
		if err != nil {
			return nil, err
		}
		if title != "" {
			r.Chapter = &title
		}
		r.ChapterIndex = idx
		r.State = houseState(c.now().Sub(parseSessionTime(r.UpdatedAt)))
		out.Listening = append(out.Listening, r.HouseListeningRow)
	}
	return out, nil
}

// houseState is a housemate's state from the age of their newest save.
func houseState(age time.Duration) string {
	if s := sessionState(age); s != SessionEnded {
		return s
	}
	return HouseRecent
}

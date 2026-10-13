package catalog

import (
	"cmp"
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
//   - reciprocal: a person who doesn't share sees nobody (HouseListening is empty);
//   - only other people who share, whose account is enabled and not a demo;
//   - never a kid (household member), as a row or as a viewer: a kid has no
//     household_sharing row (auth clears it when someone becomes a kid, and the
//     route refuses a kid's PUT), so the opt-in rule already leaves them out;
//   - only a session recorded on this server (not imported, not backfilled) with
//     listening in it, the newest per person, within houseWindow;
//   - only a book the CALLER may see: a person's newest book outside the caller's
//     scopes drops that person, never falls back to an older book;
//   - never a device, an address, an app or a token: HouseListeningRow has no
//     field that could carry one (unlike the admin's LiveSessions).

// houseWindow is how far back a housemate's newest session may be to show.
const houseWindow = 24 * time.Hour

// HouseRecent is a housemate's state when their newest save is older than
// SessionGap (within houseWindow); newer ones are SessionPlaying or
// SessionPaused, as live sessions say.
const HouseRecent = "recent"

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
	// A kid never shares (the rules above rely on no kid having a row): refused
	// in the write itself, so a request racing the owner's switch to kid can't
	// leave one behind.
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO household_sharing(user_id, share_listening, updated_at)
		 SELECT ?1, ?2, ?3 WHERE NOT EXISTS (SELECT 1 FROM users WHERE id = ?1 AND `+isKidSQL+`)
		 ON CONFLICT(user_id) DO UPDATE SET share_listening = excluded.share_listening, updated_at = excluded.updated_at`,
		userID, share, c.stamp())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return cmp.Or(err, ErrKidAccount)
	}
	return nil
}

// ErrKidAccount is a write a kid can't make, refused inside it
// (SetHouseholdSharing, SetCollectionShares): the transport's notKid answers
// first, but a request that passed it just before the owner's switch to kid
// committed lands here.
var ErrKidAccount = errors.New("not available for a kid's account")

// isKidSQL keeps the users rows that are kids: all three member columns, as
// auth's User.IsKid reads them.
const isKidSQL = `(member_kind = 'kid' AND managed_by IS NOT NULL AND member_access IS NOT NULL)`

// HouseListening is what the other people who share are listening to, as
// callerID sees it (callerScopes, as UserScopes gives them), newest first: empty
// unless the caller shares. See the rules above. The api asks HouseholdSharing
// first, so a caller who doesn't share costs no scope load; the query checks it
// again itself, so the rule holds for any caller.
func (c *Catalog) HouseListening(ctx context.Context, callerID int64, callerScopes []Scope) ([]HouseListeningRow, error) {
	type row struct {
		HouseListeningRow
		bookID int64
	}
	// Driven from household_sharing: the caller's own row (reciprocal), then each
	// other sharer, their account, and their newest session with listening in it
	// (the scalar subquery walks idx_sessions_user_last newest first and stops at
	// the first match); the books join runs only for those winners. CROSS JOIN
	// keeps that order: without it, and with no ANALYZE statistics, the planner
	// starts from idx_users_demo and visits every account. Verified plan:
	//
	//	SEARCH me USING INTEGER PRIMARY KEY (rowid=?)
	//	SCAN h
	//	SEARCH u USING INTEGER PRIMARY KEY (rowid=?)
	//	SEARCH w USING INTEGER PRIMARY KEY (rowid=?)
	//	CORRELATED SCALAR SUBQUERY 1
	//	  SEARCH s USING INDEX idx_sessions_user_last (user_id=? AND last_at>?)
	//	SEARCH b USING INDEX sqlite_autoindex_books_1 (library_id=? AND rel_path=?) LEFT-JOIN
	//	USE TEMP B-TREE FOR ORDER BY
	//
	// The scope check is in Go, on each person's newest book only: one the caller
	// can't see drops the person rather than letting an older one stand in.
	rows, err := queryRows(ctx, c.db, func(rs *sql.Rows, r *row) error {
		return rs.Scan(&r.UserID, &r.Username, &r.LibraryID, &r.Path, &r.Title, &r.Author,
			&r.Position, &r.Duration, &r.UpdatedAt, &r.bookID)
	}, `SELECT h.user_id, u.username, w.library_id, w.rel_path, COALESCE(b.title, ''), NULLIF(b.author, ''),
	           w.end_pos, NULLIF(COALESCE(NULLIF(b.duration, 0), w.duration), 0), w.last_at, COALESCE(b.id, 0)
	      FROM household_sharing me
	     CROSS JOIN household_sharing h ON h.share_listening = 1 AND h.user_id <> me.user_id
	     CROSS JOIN users u ON u.id = h.user_id AND u.disabled = 0 AND u.is_demo = 0
	      JOIN listening_sessions w ON w.id = (
	           SELECT s.id FROM listening_sessions s
	            WHERE s.user_id = h.user_id AND s.last_at >= ?1 AND s.import_id = 0 AND s.backfilled = 0 AND `+listenedSQL+`
	            ORDER BY s.last_at DESC, s.id DESC LIMIT 1)
	      LEFT JOIN books b ON b.library_id = w.library_id AND b.rel_path = w.rel_path
	     WHERE me.user_id = ?2 AND me.share_listening = 1
	     ORDER BY w.last_at DESC, h.user_id`,
		formatSessionTime(c.now().Add(-houseWindow)), callerID)
	if err != nil {
		return nil, err
	}
	out := []HouseListeningRow{}
	chapters := c.newChapterCache()
	for _, r := range rows {
		if !ScopesAllow(callerScopes, Ref{LibraryID: r.LibraryID, Path: r.Path}) {
			continue
		}
		if r.Title == "" {
			r.Title = path.Base(r.Path) // not indexed (any more): the path names it
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
		out = append(out, r.HouseListeningRow)
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

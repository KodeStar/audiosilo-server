package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// The admin console's support card (GET/POST /admin/support): a quiet note on the
// Overview saying AudioSilo is free and pointing at GitHub Sponsors. The server
// decides when it shows. An admin's answer ("I've donated", "Not now") is stored
// server-wide, on trust: nothing is checked, nothing is sent anywhere, and hiding
// the card unlocks nothing, so a donation stays a gift.

const (
	// SupportAfterDays is how long a server has been in use before the card shows.
	SupportAfterDays = 30
	// SupportAfterFinished lets the card show sooner on a busy server: this many
	// books finished here, by anyone, since the server's first account.
	SupportAfterFinished = 10
	// SupportMinDays is the floor under SupportAfterFinished: never in a server's
	// first week, however many finishes it holds (an Audiobookshelf import can
	// bring a household's finished books on day one).
	SupportMinDays = 7
	// SupportSnoozeMonths is how long "Not now" hides the card.
	SupportSnoozeMonths = 6
)

// SupportChoice is an admin's answer to the support card.
type SupportChoice string

const (
	// SupportDonated hides the card for good on this server.
	SupportDonated SupportChoice = "donated"
	// SupportSnoozed hides it until SupportSnoozeMonths from the answer.
	SupportSnoozed SupportChoice = "snoozed"
)

// ErrInvalidSupportChoice is returned by SetSupportChoice for any other choice.
var ErrInvalidSupportChoice = errors.New("invalid support choice")

// supportKey is the card's row in server_state.
const supportKey = "support_card"

// supportState is server_state's value for supportKey.
type supportState struct {
	Choice SupportChoice `json:"choice"`
	// Until is when a snooze ends (RFC 3339, UTC); empty for a donation.
	Until string `json:"until,omitempty"`
}

// SupportCardDue reports whether the support card shows now: never after "I've
// donated", not while a "Not now" lasts, and otherwise once the server has been
// in use SupportAfterDays, or SupportMinDays with SupportAfterFinished books
// finished. "In use" starts at the earliest account that is not a demo account,
// so a server restored from a backup keeps its age and an empty one (setup not
// finished) has none.
func (c *Catalog) SupportCardDue(ctx context.Context) (bool, error) {
	now := c.now()
	st, err := c.supportState(ctx)
	if err != nil {
		return false, err
	}
	switch st.Choice {
	case SupportDonated:
		return false, nil
	case SupportSnoozed:
		// A snooze whose end can't be read has ended: the card is one click from gone.
		if until, err := time.Parse(time.RFC3339Nano, st.Until); err == nil && now.Before(until) {
			return false, nil
		}
	}

	// unixepoch skips a created_at it can't read (NULL), so one odd row can't
	// make the server look new or old.
	var first sql.NullInt64
	if err := c.db.QueryRowContext(ctx,
		`SELECT MIN(unixepoch(created_at)) FROM users WHERE is_demo = 0`).Scan(&first); err != nil {
		return false, err
	}
	if !first.Valid {
		return false, nil
	}
	since := time.Unix(first.Int64, 0)
	age := now.Sub(since)
	const day = 24 * time.Hour
	switch {
	case age < SupportMinDays*day:
		return false, nil
	case age >= SupportAfterDays*day:
		return true, nil
	}
	// Books finished here: an import's finishes from before the server existed
	// don't count.
	var finished int
	if err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM progress p JOIN users u ON u.id = p.user_id
		 WHERE p.finished = 1 AND u.is_demo = 0 AND unixepoch(p.finished_at) >= ?`,
		since.Unix()).Scan(&finished); err != nil {
		return false, err
	}
	return finished >= SupportAfterFinished, nil
}

// SupportChoiceResult is what SetSupportChoice stored.
type SupportChoiceResult struct {
	// Changed is false when the server already held a donation: a later "Not now"
	// never turns "for good" into six months.
	Changed bool
	// Until is when a snooze ends (zero for a donation).
	Until time.Time
}

// SetSupportChoice records an admin's answer to the support card for the whole
// server.
func (c *Catalog) SetSupportChoice(ctx context.Context, choice SupportChoice) (SupportChoiceResult, error) {
	st := supportState{Choice: choice}
	var res SupportChoiceResult
	switch choice {
	case SupportDonated:
	case SupportSnoozed:
		res.Until = c.now().UTC().AddDate(0, SupportSnoozeMonths, 0)
		st.Until = res.Until.Format(time.RFC3339)
	default:
		return res, ErrInvalidSupportChoice
	}
	value, err := json.Marshal(st)
	if err != nil {
		return res, err
	}
	r, err := c.db.ExecContext(ctx,
		`INSERT INTO server_state(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		 WHERE json_extract(server_state.value, '$.choice') IS NOT ?`,
		supportKey, string(value), c.stamp(), string(SupportDonated))
	if err != nil {
		return res, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return res, err
	}
	res.Changed = n > 0
	if !res.Changed {
		res.Until = time.Time{}
	}
	return res, nil
}

// supportState reads the card's stored answer; none (or one that isn't JSON) is
// the zero state.
func (c *Catalog) supportState(ctx context.Context) (supportState, error) {
	var raw string
	err := c.db.QueryRowContext(ctx, `SELECT value FROM server_state WHERE key = ?`, supportKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return supportState{}, nil
	}
	if err != nil {
		return supportState{}, err
	}
	var st supportState
	if json.Unmarshal([]byte(raw), &st) != nil {
		return supportState{}, nil
	}
	return st, nil
}

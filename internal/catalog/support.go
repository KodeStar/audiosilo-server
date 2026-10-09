package catalog

import (
	"context"
	"database/sql"
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
	// Until is when a snooze ends (formatSessionTime); empty for a donation.
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
	var st supportState
	if _, err := getServerState(ctx, c.db, supportKey, &st); err != nil {
		return false, err
	}
	switch st.Choice {
	case SupportDonated:
		return false, nil
	case SupportSnoozed:
		// A snooze whose end can't be read (zero) has ended: the card is one click from gone.
		if now.Before(parseSessionTime(st.Until)) {
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
	// don't count. Counting stops at SupportAfterFinished.
	var finished int
	if err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (
		   SELECT 1 FROM progress p JOIN users u ON u.id = p.user_id
		    WHERE p.finished = 1 AND u.is_demo = 0 AND p.finished_at >= ? LIMIT ?)`,
		since.UTC().Format(time.RFC3339), SupportAfterFinished).Scan(&finished); err != nil {
		return false, err
	}
	return finished >= SupportAfterFinished, nil
}

// SupportChoiceResult is what SetSupportChoice stored.
type SupportChoiceResult struct {
	// Changed is false when the server already held a donation: a later "Not now"
	// never turns "for good" into a snooze.
	Changed bool
	// Until is when a stored snooze ends (zero for a donation, or when nothing changed).
	Until time.Time
}

// SetSupportChoice records an admin's answer to the support card for the whole
// server.
func (c *Catalog) SetSupportChoice(ctx context.Context, choice SupportChoice) (SupportChoiceResult, error) {
	st := supportState{Choice: choice}
	var until time.Time
	switch choice {
	case SupportDonated:
	case SupportSnoozed:
		until = c.now().UTC().AddDate(0, SupportSnoozeMonths, 0)
		st.Until = formatSessionTime(until)
	default:
		return SupportChoiceResult{}, ErrInvalidSupportChoice
	}
	changed := false
	err := c.db.WithTx(ctx, "SetSupportChoice", func(tx *sql.Tx) error {
		var cur supportState
		if _, err := getServerState(ctx, tx, supportKey, &cur); err != nil {
			return err
		}
		if cur.Choice == SupportDonated {
			return nil
		}
		changed = true
		return putServerState(ctx, tx, supportKey, st, c.stamp())
	})
	if err != nil || !changed {
		return SupportChoiceResult{}, err
	}
	return SupportChoiceResult{Changed: true, Until: until}, nil
}

package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Bridge Phase 4a: Audible. The Bridge holds the Audible sign-ins (its vault)
// and polls each account's library; the server stores the accounts' mapping
// and titles (catalog/audible.go) and queues the polls.
const (
	// KindAudiblePoll lists an Audible account's library on the Bridge,
	// matches each title against the account's library on the server and
	// reports the titles (PUT /bridge/audible/accounts/{account}/titles). It
	// writes nothing into the library (an ASIN it backfills goes through the
	// server: PUT /bridge/libraries/{id}/enrichment). Params {account}.
	KindAudiblePoll = "audible.poll"
	// ModuleAudible is the managed module Audible needs (its id:
	// internal/modules/builtin): its admin routes answer 404 module_off and
	// its queued jobs are held while it is off.
	ModuleAudible = "audible"
	// FeatureAudible is the feature of a Bridge that runs Audible jobs (the
	// family of KindAudiblePoll), the module's Bridge requirement.
	FeatureAudible = "audible"
)

// AudibleAccountPattern is an Audible account's id: the Bridge makes it,
// opaque to the server.
var AudibleAccountPattern = regexp.MustCompile(`^[a-z0-9]{8,32}$`)

// AudiblePollParams are audible.poll's params: the account to poll.
type AudiblePollParams struct {
	Account string `json:"account"`
}

// audiblePollParams checks audible.poll's params strictly.
func audiblePollParams(raw json.RawMessage) (json.RawMessage, error) {
	var p AudiblePollParams
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid("params: %v", err)
	}
	if !AudibleAccountPattern.MatchString(p.Account) {
		return nil, invalid("account must match %s", AudibleAccountPattern)
	}
	return json.Marshal(p)
}

// audiblePollSuffix extends an audible.poll's dedupe key with its account, so
// each account has at most one open poll.
func audiblePollSuffix(j NewJob) string {
	var p AudiblePollParams
	_ = json.Unmarshal(j.Params, &p)
	return "|" + p.Account
}

// AudiblePoll is the NewJob that polls account.
func AudiblePoll(account string, priority int) NewJob {
	raw, _ := json.Marshal(AudiblePollParams{Account: account})
	return NewJob{Kind: KindAudiblePoll, Params: raw, Priority: priority}
}

// pruneAudiblePolls keeps only the newest ended poll of the account a new poll
// is queued for (its dedupe key), inside Enqueue's transaction: a poll every
// few hours would otherwise pile up in the job list.
func pruneAudiblePolls(ctx context.Context, tx *sql.Tx, key string) error {
	// The kind lets idx_bridge_jobs_kind_finished serve both scans (the
	// dedupe_key index holds only open jobs).
	_, err := tx.ExecContext(ctx, `DELETE FROM bridge_jobs WHERE kind = ?2 AND dedupe_key = ?1 AND state IN `+EndedStatesSQL+`
		AND id <> (SELECT MAX(id) FROM bridge_jobs WHERE kind = ?2 AND dedupe_key = ?1 AND state IN `+EndedStatesSQL+`)`,
		key, KindAudiblePoll)
	return err
}

// The server's reasons for cancelling an account's open poll (IsCancelReason;
// the console keeps a copy: admin-ui generate-model.ts).
const (
	// CancelAudibleRemoved: the Bridge removed the account.
	CancelAudibleRemoved = "the Audible account was removed"
	// CancelAudiblePaused: the account's library was deleted (it is paused).
	CancelAudiblePaused = "the Audible account's library was deleted"
)

// CancelAudiblePolls cancels account's open poll inside tx, reason one of
// CancelAudibleRemoved or CancelAudiblePaused: at once when queued or parked,
// else at the Bridge's next report.
func CancelAudiblePolls(ctx context.Context, tx *sql.Tx, account, reason string, now time.Time) error {
	return cancelOpenTx(ctx, tx, jobKey(AudiblePoll(account, 0)), at(now), reason)
}

// CleanText is v as printable bounded text on one line (cleanText): what the
// Bridge reports from the outside world (an Audible title, its authors) is
// cleaned rather than refused, so one odd title never fails a whole poll.
func CleanText(v string, max int) string { return strings.TrimSpace(cleanText(v, max, false)) }

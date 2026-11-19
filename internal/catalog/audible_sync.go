package catalog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

// Bridge Phase 4c: keeping an Audible account's places in step with this
// server, both ways. The Bridge reads the account's places on Audible (the
// audible.sync job, params built here: AudibleSyncTitles) and posts them for
// the account's person as a furthest-only batch (SyncBatch.FurthestOnly), and
// pushes the person's places here back to Audible from GET /bridge/progress
// (AudibleProgress). Only the titles the console lets sync take part: matched
// to a book here, not "Not this book" (rejected), not "Don't sync this book"
// (no_sync), not ignored (the manager registry's Ignored), not gone from the
// account, not still a preorder. See CONVENTIONS.md, Audible sync.

var (
	// ErrAudibleSyncOff: the account's sync is off (403 sync_off).
	ErrAudibleSyncOff = errors.New("sync with Audible is off for the account")
	// ErrAudibleSyncUser: the account's person may no longer be synced for: a
	// kid, demo or disabled account since it was mapped (403 user_unavailable).
	// A deleted person takes the account with them (ErrUnknownAccount).
	ErrAudibleSyncUser = errors.New("the account's person can't be synced for")
)

// AudibleDevicePrefix starts the device id of an account's places
// ("audible:<account>").
const AudibleDevicePrefix = "audible:"

// IsAudibleDevice reports whether a device id names an Audible account.
func IsAudibleDevice(id string) bool { return strings.HasPrefix(id, AudibleDevicePrefix) }

// syncTitleSQL is a title (aliased t) whose book takes part in its account's
// sync: matched, not rejected, ignored or "Don't sync this book", neither gone
// from the account nor still a preorder (bridge#60: the poll may match a
// preorder to another edition on disk, and Audible holds no place for an
// unreleased ASIN, so a push to it is refused; released, it syncs, and the
// re-match trigger, 0071, offers its book's place again).
const syncTitleSQL = `t.match_library_id IS NOT NULL AND t.match_path IS NOT NULL AND t.rejected = 0 AND t.ignored = 0 AND t.no_sync = 0
	AND t.state NOT IN ('` + AudibleGone + `', '` + AudiblePreorder + `')`

// audibleBookLengthSQL is the length a title's runtime is checked against
// (audibleRuntimeAgrees) for its book (b, the index) and the account's person's
// row on it (p): the indexed duration, else the row's. AudibleSyncBooks and
// AudibleProgress share it, so the listening route and the feed agree (server#303).
const audibleBookLengthSQL = `CASE WHEN COALESCE(b.duration, 0) > 0 THEN b.duration ELSE COALESCE(p.duration, 0) END`

// AudibleSyncAccount is an account a sync call acts for: its person (whose
// scopes and progress rows the call reads and writes) and its names.
type AudibleSyncAccount struct {
	Account     string
	Label       string
	Marketplace string
	UserID      int64
	Admin       bool
}

// DeviceID is the device id the account's places are written under.
func (a AudibleSyncAccount) DeviceID() string { return AudibleDevicePrefix + a.Account }

// DeviceName is the name the account's places carry ("Audible (Chris US)"),
// the store's when the account has no label.
func (a AudibleSyncAccount) DeviceName() string {
	name := a.Label
	if name == "" {
		name = strings.ToUpper(a.Marketplace)
	}
	return "Audible (" + name + ")"
}

// AudibleSyncFor is account as a sync call acting for it needs it:
// ErrUnknownAccount for none (its person deleted, too), ErrAudibleSyncOff while
// its sync is off or the account is paused (its library deleted: no sync is
// queued for it either, DueAudibleSyncs), ErrAudibleSyncUser for a person an
// account may no longer be mapped to (bridgeUserSQL: disabled, a kid or a demo
// account).
func (c *Catalog) AudibleSyncFor(ctx context.Context, account string) (*AudibleSyncAccount, error) {
	a := AudibleSyncAccount{Account: account}
	var on, allowed bool
	var role string
	err := c.db.QueryRowContext(ctx,
		`SELECT a.label, a.marketplace, a.sync = 1 AND a.library_id IS NOT NULL, a.user_id, u.role, `+bridgeUserSQL+`
		   FROM audible_accounts a JOIN users u ON u.id = a.user_id WHERE a.account = ?`, account).
		Scan(&a.Label, &a.Marketplace, &on, &a.UserID, &role, &allowed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrUnknownAccount
	case err != nil:
		return nil, err
	case !on:
		return nil, ErrAudibleSyncOff
	case !allowed:
		return nil, ErrAudibleSyncUser
	}
	a.Admin = role == "admin"
	return &a, nil
}

// AudibleSyncTitles are account's titles that take part in its sync
// (syncTitleSQL), by ASIN: the titles a claim of its audible.sync answers
// (bridge.Service.ClaimAudibleSync).
func (c *Catalog) AudibleSyncTitles(ctx context.Context, account string) ([]bridge.AudibleSyncTitle, error) {
	return queryRows(ctx, c.db, func(rows *sql.Rows, t *bridge.AudibleSyncTitle) error {
		return rows.Scan(&t.ASIN, &t.LibraryID, &t.Path, &t.RuntimeS)
	}, `SELECT t.asin, t.match_library_id, t.match_path, t.runtime_s FROM audible_titles t
		 WHERE t.account = ? AND `+syncTitleSQL+` ORDER BY t.asin`, account)
}

// HasAudibleSyncTitles reports whether any of account's titles takes part in
// its sync (syncTitleSQL): the scheduler queues no audible.sync without one.
func (c *Catalog) HasAudibleSyncTitles(ctx context.Context, account string) (bool, error) {
	var has bool
	err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audible_titles t WHERE t.account = ? AND `+syncTitleSQL+`)`,
		account).Scan(&has)
	return has, err
}

// AudibleSyncBooks are the books account's sync may write (its titles'
// matches, syncTitleSQL), as progress keys, each with "" or the reason the
// sync's events on it are rejected: duration_mismatch when no title on the
// book has a runtime that agrees with the book's length (audibleRuntimeAgrees:
// the title is matched to another edition or volume, so neither its places
// nor its finish are this book's). The length is the indexed duration, else
// the account's person's own row's, the one rule the feed checks a push by
// (AudibleProgress, server#303).
func (c *Catalog) AudibleSyncBooks(ctx context.Context, account string) (map[Ref]string, error) {
	type row struct {
		ref      Ref
		runtimeS int64
		duration float64
	}
	titles, err := queryRows(ctx, c.db, func(rows *sql.Rows, x *row) error {
		return rows.Scan(&x.ref.LibraryID, &x.ref.Path, &x.runtimeS, &x.duration)
	}, `SELECT t.match_library_id, t.match_path, t.runtime_s,
	           `+audibleBookLengthSQL+`
	      FROM audible_titles t
	      JOIN audible_accounts a ON a.account = t.account
	      LEFT JOIN books b ON b.library_id = t.match_library_id AND b.rel_path = t.match_path
	      LEFT JOIN progress p ON p.user_id = a.user_id AND p.library_id = t.match_library_id AND p.rel_path = t.match_path
	     WHERE t.account = ? AND `+syncTitleSQL, account)
	if err != nil {
		return nil, err
	}
	out := make(map[Ref]string, len(titles))
	for _, t := range titles {
		if audibleRuntimeAgrees(t.runtimeS, t.duration) {
			out[t.ref] = ""
		} else if _, ok := out[t.ref]; !ok {
			out[t.ref] = reasonDurationMismatch
		}
	}
	return out, nil
}

// audibleUndatedID is the id the Bridge gives the event of a place Audible
// sent no last_updated for (CROSS-REPO.md §23: "aud-" and the first 32 hex of
// sha256(account|asin|position_ms|last_updated|kind|library_id|path), its
// last_updated empty), kind "" for the listen and EventFinish for the finish.
func audibleUndatedID(account, asin string, positionMs int64, kind string, ref Ref) string {
	sum := sha256.Sum256([]byte(account + "|" + asin + "|" + strconv.FormatInt(positionMs, 10) + "||" + kind + "|" +
		strconv.FormatInt(ref.LibraryID, 10) + "|" + ref.Path))
	return "aud-" + hex.EncodeToString(sum[:])[:32]
}

// AudibleUndatedEvents are the ids of the events of account's batch that the
// Bridge dated on its own clock: a place Audible gave no last_updated for is
// stamped with the Bridge's clock when it read it, every other with Audible's
// own time. The wire doesn't say which, but the event's id does: it is a hash
// of the place with its last_updated (audibleUndatedID), so an id that is the
// hash of the event's own place under one of the book's synced titles with no
// last_updated is one of these, and so is the finish at such a place in the
// same batch (a finish carries no place; the Bridge sends it right after its
// listen, and one split from it into the next batch is dated as sent). The
// listening route corrects these alone by the Bridge's clock (SyncBatch.
// SenderDated): uncorrected, a Bridge running behind dated one before a
// restart or Stay made since, and it was answered "behind" at every sync, its
// id never changing (server#415). An id the Bridge makes another way matches
// nothing here and is dated as sent.
func (c *Catalog) AudibleUndatedEvents(ctx context.Context, account string, events []SyncEvent) (map[string]bool, error) {
	titles, err := c.AudibleSyncTitles(ctx, account)
	if err != nil {
		return nil, err
	}
	asins := make(map[Ref][]string, len(titles))
	for _, t := range titles {
		ref := Ref{LibraryID: t.LibraryID, Path: t.Path}
		asins[ref] = append(asins[ref], t.ASIN)
	}
	type heard struct {
		asin string
		ms   int64
	}
	out := map[string]bool{}
	places := map[Ref][]heard{} // the undated places, by book as sent
	for _, ev := range events {
		if ev.Kind != EventListen || ev.undecodable {
			continue
		}
		ref := Ref{LibraryID: ev.LibraryID, Path: ev.Path}
		ms := int64(math.Round(ev.ToPos * 1000))
		for _, asin := range asins[ref] {
			if audibleUndatedID(account, asin, ms, "", ref) == ev.ID {
				out[ev.ID] = true
				places[ref] = append(places[ref], heard{asin, ms})
			}
		}
	}
	for _, ev := range events {
		if ev.Kind != EventFinish {
			continue
		}
		ref := Ref{LibraryID: ev.LibraryID, Path: ev.Path}
		for _, h := range places[ref] {
			if audibleUndatedID(account, h.asin, h.ms, EventFinish, ref) == ev.ID {
				out[ev.ID] = true
			}
		}
	}
	return out, nil
}

// audibleRuntimeAgrees reports whether an Audible title's runtime (seconds)
// and the book's length (audibleBookLengthSQL) are one recording's: within 5% of the
// longer plus 2 minutes (furthestPastEndRatio, the manager's wrong-match
// guard), or either unknown.
func audibleRuntimeAgrees(runtimeS int64, duration float64) bool {
	if runtimeS <= 0 || duration <= 0 {
		return true
	}
	rt := float64(runtimeS)
	return math.Abs(rt-duration) <= max(rt, duration)*(furthestPastEndRatio-1)+furthestPastEndSlack
}

// AudibleProgressRow is one of the person's places the Bridge may push to the
// account on Audible (GET /bridge/progress): the book, the title's ASIN and
// the row as stored (its duration, else the indexed book's).
type AudibleProgressRow struct {
	LibraryID int64   `json:"library_id"`
	Path      string  `json:"path"`
	ASIN      string  `json:"asin"`
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	Finished  bool    `json:"finished"`
	UpdatedAt string  `json:"updated_at"`
	DeviceID  string  `json:"device_id"`
	Version   int64   `json:"version"`
}

// MaxAudibleProgress is the most rows one GET /bridge/progress answers, and
// defaultAudibleProgress its page when the call names no limit.
const (
	MaxAudibleProgress     = 500
	defaultAudibleProgress = 200
)

// progressCursor is a GET /bridge/progress position: the last row's
// (change_seq, asin) and the feed that made it (progressFeedID; none in a
// cursor from a server before 2.7.0), opaque on the wire (base64url JSON).
type progressCursor struct {
	Seq  int64  `json:"q"`
	ASIN string `json:"s"`
	Feed string `json:"f,omitempty"`
}

func (k progressCursor) encode() string {
	raw, _ := json.Marshal(k)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeProgressCursor reads AudibleProgress's since ("" from the start).
// ErrInvalidCursor for one it can't read, or one naming no row (a JSON null
// or {} is no position, never the start), which the route answers 400
// invalid_cursor so the Bridge reads the feed again from its start
// (bridge#50). A cursor names a row once read, and every row's change_seq is
// at least 1.
func decodeProgressCursor(s string) (progressCursor, error) {
	if s == "" {
		return progressCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return progressCursor{}, ErrInvalidCursor
	}
	var k progressCursor
	if json.Unmarshal(raw, &k) != nil || k.Seq < 1 || k.ASIN == "" {
		return progressCursor{}, ErrInvalidCursor
	}
	return k, nil
}

// progressFeedKey is the server_state row holding this server's progress feed
// id: a random id every GET /bridge/progress cursor carries, so a cursor from
// another feed (another server's, a database started afresh, or this one's
// before a backup was restored here: RenewProgressFeed) is refused, never read
// as a place in this feed (bridge#50).
const progressFeedKey = "progress_feed"

// progressFeedID is the progress feed's id, made on first use.
func (c *Catalog) progressFeedID(ctx context.Context) (string, error) {
	id, err := getServerState[string](ctx, c.db, progressFeedKey)
	if err != nil || id != "" {
		return id, err
	}
	err = c.db.WithTx(ctx, "progressFeedID", func(tx *sql.Tx) error {
		if id, err = getServerState[string](ctx, tx, progressFeedKey); err != nil || id != "" {
			return err
		}
		id = rand.Text()
		return putServerState(ctx, tx, progressFeedKey, id, c.stamp())
	})
	return id, err
}

// progressFeedRestoreKey is the server_state row naming the restore the
// progress feed was last renewed for (RenewProgressFeed).
const progressFeedRestoreKey = "progress_feed_restore"

// RenewProgressFeed gives the progress feed a new id, so every cursor handed
// out before is refused (400 invalid_cursor) and the Bridge reads the feed
// again from its start: the launcher's step after restoring a backup, whose
// feed is behind the cursors the Bridge holds (a backup carries its id). Once
// for a restore (restore names it: when it was applied), which the database
// records with the new id, so the launcher asks at every start and a start
// that ended before this committed leaves it to the next; renewed is whether
// this call made the new id.
func (c *Catalog) RenewProgressFeed(ctx context.Context, restore string) (renewed bool, err error) {
	if done, err := getServerState[string](ctx, c.db, progressFeedRestoreKey); err != nil || done == restore {
		return false, err
	}
	err = c.renewProgressFeed(ctx, "RenewProgressFeed", func(tx *sql.Tx, at string) error {
		return putServerState(ctx, tx, progressFeedRestoreKey, restore, at)
	})
	return err == nil, err
}

// RenewProgressFeedBehind renews the progress feed when its end (end, the last
// number the change counter gave) is below mark, the end the launcher last
// wrote down outside the database: the database went back with no restore
// recorded, a backup put in place by hand or commits a power cut lost, and it
// kept its feed id. A cursor past end is refused already (progressFrom), but
// only until the counter passes it again, after which it would page on past
// the places written in between; with a new id it is refused for good.
func (c *Catalog) RenewProgressFeedBehind(ctx context.Context, mark int64) (end int64, renewed bool, err error) {
	if end, err = c.ProgressFeedEnd(ctx); err != nil || end >= mark {
		return end, false, err
	}
	err = c.renewProgressFeed(ctx, "RenewProgressFeedBehind", nil)
	return end, err == nil, err
}

// renewProgressFeed stores a new feed id, and what also (when set) writes with
// it, in one transaction. Durable: the launcher goes on to record outside the
// database that the renewal is made (RenewProgressFeedBehind's mark), which a
// power cut must not leave standing over a renewal it undid.
func (c *Catalog) renewProgressFeed(ctx context.Context, name string, also func(tx *sql.Tx, at string) error) error {
	return c.db.WithDurableTx(ctx, name, func(tx *sql.Tx) error {
		at := c.stamp()
		if err := putServerState(ctx, tx, progressFeedKey, rand.Text(), at); err != nil || also == nil {
			return err
		}
		return also(tx, at)
	})
}

// ProgressFeedEnd is the progress feed's end: the last number the change
// counter gave (progress_change_counter, migration 0066).
func (c *Catalog) ProgressFeedEnd(ctx context.Context) (int64, error) {
	var end int64
	err := c.db.QueryRowContext(ctx, `SELECT seq FROM progress_change_counter WHERE id = 1`).Scan(&end)
	return end, err
}

// progressFrom reads since as a place in this server's feed (feed, its id):
// the start for "", else ErrInvalidCursor for a cursor that isn't one
// (decodeProgressCursor). With refuseStale (the Bridge asked for it:
// AudibleProgress) it is ErrInvalidCursor too for one from another feed, or
// one past the feed's end (ProgressFeedEnd). That last is a database that went
// back and kept its id (RenewProgressFeedBehind renews it at the next start
// the launcher sees it from): until the counter passes the cursor again, the
// feed would answer nothing and the rows written since would never reach
// Audible. A cursor from a server before 2.7.0 carries no id and has only that
// check. Without refuseStale (v0.6.0 and older Bridges, which retry a refused
// cursor for ever and so would push no place again) both are read as before
// 2.7.0, a cursor from another feed as a place in this one, except that one
// past the end is read as the feed's end (progressEndASIN), so the page is
// empty and its next reads the rows written from now on. In a feed no row was
// ever written to (the counter at 0) there is no such place and since stays
// as it is. The place comes back with feed as its id.
func (c *Catalog) progressFrom(ctx context.Context, since, feed string, refuseStale bool) (progressCursor, error) {
	from, err := decodeProgressCursor(since)
	if err != nil || since == "" {
		return from, err
	}
	if refuseStale && from.Feed != "" && from.Feed != feed {
		return progressCursor{}, ErrInvalidCursor
	}
	end, err := c.ProgressFeedEnd(ctx)
	if err != nil {
		return progressCursor{}, err
	}
	if from.Seq > end {
		if refuseStale {
			return progressCursor{}, ErrInvalidCursor
		}
		if end > 0 {
			from = progressCursor{Seq: end, ASIN: progressEndASIN}
		}
	}
	from.Feed = feed
	return from, nil
}

// progressEndASIN is a progress cursor's ASIN after every title's (an ASIN is
// ten upper-case letters and digits, bridge's asinPattern): a cursor at a row
// number with it is past all of that number's rows, so the feed's end.
const progressEndASIN = "~"

// AudibleProgress is a page of the places of a's person that the Bridge may
// push to Audible: the progress rows on the books of the account's synced
// titles (syncTitleSQL) that the person can still reach (scopes, theirs), one
// per title (two titles on one book are two rows, each with its ASIN), not
// written by the account itself (the place Audible gave is never echoed back),
// written after the cursor since ("" from the start), in the order this server
// wrote them (progress.change_seq, migration 0066: a row written late with an
// earlier updated_at, such as a device's offline backlog or a diverged apply
// keeping the later time, still comes after the cursor), at most limit
// (defaultAudibleProgress for none, never past MaxAudibleProgress). next is
// where the next page starts (the last row read, or since when none was: a
// feed's cursor, kept for the next poll) and more whether a full page was read
// (ask again with next now; a page may carry fewer rows than it read, a row
// whose title's runtime disagrees with the book's length left out).
// A cursor carries the feed's id (progressFeedID): one from before 2.7.0 that
// is still good comes back as next with it. With refuseStale (the Bridge
// handles the refusal by reading the feed again from its start: bridge#50),
// ErrInvalidCursor for a cursor not made by this feed, or past its end;
// without it (a Bridge v0.6.0 or older), the answer progressFrom gives.
func (c *Catalog) AudibleProgress(ctx context.Context, a *AudibleSyncAccount, scopes []Scope, since string, limit int,
	refuseStale bool) (rows []AudibleProgressRow, next string, more bool, err error) {
	feed, err := c.progressFeedID(ctx)
	if err != nil {
		return nil, "", false, err
	}
	from, err := c.progressFrom(ctx, since, feed, refuseStale)
	if err != nil {
		return nil, "", false, err
	}
	if limit <= 0 {
		limit = defaultAudibleProgress
	}
	limit = min(limit, MaxAudibleProgress)
	type read struct {
		AudibleProgressRow
		seq      int64
		runtimeS int64
		length   float64
	}
	scoped, scopeArgs := scopesFilterSQL("p.library_id", "p.rel_path", scopes)
	args := append([]any{a.Account, a.UserID, a.DeviceID(), from.Seq, from.Seq, from.ASIN}, scopeArgs...)
	got, err := queryRows(ctx, c.db, func(r *sql.Rows, x *read) error {
		return r.Scan(&x.LibraryID, &x.Path, &x.ASIN, &x.Position, &x.Duration, &x.Finished, &x.UpdatedAt, &x.DeviceID,
			&x.Version, &x.seq, &x.runtimeS, &x.length)
	}, `SELECT p.library_id, p.rel_path, t.asin, p.position,
	           CASE WHEN p.duration > 0 THEN p.duration ELSE COALESCE(b.duration, 0) END,
	           p.finished, p.updated_at, p.device_id, p.version, p.change_seq, t.runtime_s,
	           `+audibleBookLengthSQL+`
	      FROM progress p
	      JOIN audible_titles t ON t.account = ? AND t.match_library_id = p.library_id AND t.match_path = p.rel_path
	      LEFT JOIN books b ON b.library_id = p.library_id AND b.rel_path = p.rel_path
	     WHERE p.user_id = ? AND `+syncTitleSQL+` AND p.device_id <> ?
	       AND p.change_seq >= ? AND (p.change_seq, t.asin) > (?, ?) AND `+scoped+`
	     ORDER BY p.change_seq, t.asin
	     LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, "", false, err
	}
	if len(got) == 0 {
		if since != "" {
			since = from.encode()
		}
		return []AudibleProgressRow{}, since, false, nil
	}
	// A row whose title's runtime disagrees with the book's length (the
	// indexed duration, else the row's own: audibleRuntimeAgrees, the listening
	// route's duration_mismatch, AudibleSyncBooks) is another edition or volume's, so it is never
	// pushed to that ASIN; the Bridge needs no check of its own.
	rows = make([]AudibleProgressRow, 0, len(got))
	for _, x := range got {
		if audibleRuntimeAgrees(x.runtimeS, x.length) {
			rows = append(rows, x.AudibleProgressRow)
		}
		next = progressCursor{Seq: x.seq, ASIN: x.ASIN, Feed: feed}.encode()
	}
	return rows, next, len(got) == limit, nil
}

// syncQueueableSQL is an account (aliased a, its person u) whose sync may be
// queued: sync on, not paused, not refused, mapped to a person an account may
// still be mapped to (bridgeUserSQL).
const syncQueueableSQL = `a.sync = 1 AND a.library_id IS NOT NULL AND a.refused_at IS NULL AND ` + bridgeUserSQL

// AudibleSyncQueueable reports whether account's sync may be queued now
// (syncQueueableSQL), whenever it was last queued: audible.SyncNow's check.
func (c *Catalog) AudibleSyncQueueable(ctx context.Context, account string) (bool, error) {
	var ok bool
	err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audible_accounts a JOIN users u ON u.id = a.user_id
		  WHERE a.account = ? AND `+syncQueueableSQL+`)`, account).Scan(&ok)
	return ok, err
}

// DueAudibleSyncs are the accounts whose audible.sync is due at now, at most
// limit, the longest waiting first: sync on, not paused, not refused, mapped to
// a person an account may still be mapped to (bridgeUserSQL), and not queued
// within bridge.AudibleSyncEvery.
func (c *Catalog) DueAudibleSyncs(ctx context.Context, now time.Time, limit int) ([]string, error) {
	type row struct {
		account string
		queued  sql.NullString
	}
	all, err := queryRows(ctx, c.db, func(r *sql.Rows, x *row) error { return r.Scan(&x.account, &x.queued) },
		`SELECT a.account, a.sync_queued_at FROM audible_accounts a JOIN users u ON u.id = a.user_id
		  WHERE `+syncQueueableSQL+`
		  ORDER BY a.sync_queued_at IS NOT NULL, a.sync_queued_at, a.account`)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, x := range all {
		if len(out) == limit {
			break
		}
		if x.queued.Valid {
			if at, err := time.Parse(time.RFC3339Nano, x.queued.String); err == nil && now.Sub(at) < bridge.AudibleSyncEvery {
				continue
			}
		}
		out = append(out, x.account)
	}
	return out, nil
}

// MarkAudibleSyncQueued records that account's sync was queued now (the
// scheduler counts bridge.AudibleSyncEvery from it). A user-state write.
func (c *Catalog) MarkAudibleSyncQueued(ctx context.Context, account string) error {
	_, err := c.db.ExecUserState(ctx, `UPDATE audible_accounts SET sync_queued_at = ? WHERE account = ?`, c.ts(), account)
	return err
}

// FoldAudibleSync records an ended audible.sync or audible.poll on its
// account: a sync that read Audible's places (bridge.AudibleSynced: done,
// neither waiting on Audible nor finding the sync off) is the account's last
// sync (last_sync_at); either failing because Audible refused the account
// (bridge.SyncAudibleRefused) marks it refused, so no sync is queued and the
// console's accounts card asks for a sign-in again (bridge#40), until the
// Bridge maps the account again or polls it whole (PutAudibleTitles: Audible
// answered the account), unless the refusal ended in the job's claim's second
// or after it (refusalEndedSince: a fresh sign-in while it ran, server#303, or
// one the two's whole seconds can't order, server#359). Any other ending
// changes nothing (the next one is due as usual; audible_sign_in, Audible
// refusing the sign-in for now, among them), and its params aren't read: a
// sync the server ended because they can't be (bridge.fillAudibleSync) folds
// to nothing rather than an error. A user-state write.
func (c *Catalog) FoldAudibleSync(ctx context.Context, job bridge.Job) error {
	synced := job.Kind == bridge.KindAudibleSync && bridge.AudibleSynced(job)
	if !synced && bridge.AudibleRefusalCode(job) != bridge.SyncAudibleRefused {
		return nil
	}
	account, err := bridge.AudibleAccountOf(job)
	if err != nil {
		return err
	}
	if synced {
		_, err = c.db.ExecUserState(ctx, `UPDATE audible_accounts SET last_sync_at = ? WHERE account = ?`, c.ts(), account)
		return err
	}
	var ended sql.NullString
	err = c.db.QueryRowContext(ctx, `SELECT refusal_ended_at FROM audible_accounts WHERE account = ?`, account).Scan(&ended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || refusalEndedSince(ended, job.ClaimedAt) {
		return err
	}
	// Only while the refusal_ended_at read stands: a sign-in or complete poll
	// landing between the read and this write ended the refusal after the
	// claim, so it marks nothing.
	_, err = c.db.ExecUserState(ctx, `UPDATE audible_accounts SET refused_at = ? WHERE account = ? AND refusal_ended_at IS ?`,
		c.ts(), account, ended)
	return err
}

// refusalEndedSince reports whether an account's refusal ended (ended, its
// refusal_ended_at) in the second of a job's claim (claimed, whole seconds) or
// a later one: that job's refusal is from before it, or can't be told from one
// that is. In the claim's own second the two can't be ordered (a sync or poll
// claimed just before the sign-in, or just after it while the Bridge had yet
// to save the new sign-in): the refusal counts as unknown rather than marking
// an account just signed in, and one that stands is marked by the next job
// (server#359). Unknown either way, it did not.
func refusalEndedSince(ended sql.NullString, claimed *string) bool {
	if !ended.Valid || claimed == nil {
		return false
	}
	end, ok1 := ParseStamp(ended.String)
	at, ok2 := ParseStamp(*claimed)
	return ok1 && ok2 && !end.Truncate(time.Second).Before(at)
}

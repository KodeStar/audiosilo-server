package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

const (
	KindPodcastSubscribe = "podcast.subscribe"
	KindPodcastPoll      = "podcast.poll"
	KindPodcastFetch     = "podcast.fetch"
	KindPodcastRetain    = "podcast.retain"
	ModulePodcasts       = "podcasts"
	FeaturePodcast       = "podcast"

	// The libraries.kind values a kindSpec.libraryKind names (catalog.KindBooks,
	// catalog.KindPodcasts; bridge never imports catalog).
	libraryBooks    = "books"
	libraryPodcasts = "podcasts"
)

var ErrTooManyRequests = errors.New("too many open subscribe requests")

// ErrPrivacyConflict is a subscribe for a feed URL the same person already has
// an open request for with the other privacy: the open request is never
// adopted across privacy (a private URL is never attached to a public job, a
// public request never rides a private one).
var ErrPrivacyConflict = errors.New("an open follow request for this feed has the other privacy")

// ErrNoNewRequest is an adopt-only subscribe (SubscribeRequest.AdoptOnly: an
// OPML import past its new-request budget) with no open request to adopt.
var ErrNoNewRequest = errors.New("no new subscribe request may be queued")
var ErrNotPodcastLibrary = errors.New("not a podcast library")
var ErrNotBookLibrary = errors.New("not a book library")

// EndedStatesSQL is the states a job ends in, as an SQL list (OpenStatesSQL's
// complement among the states a job stays in).
const EndedStatesSQL = `('done', 'failed', 'cancelled')`

func IsPodcast(kind string) bool {
	return kind == KindPodcastSubscribe || kind == KindPodcastPoll || kind == KindPodcastFetch || kind == KindPodcastRetain
}
func Internal(kind string) bool { return kinds[kind].internal }

// podcastWork reports whether kind is one of a followed show's bound jobs
// (poll, fetch, retain): params naming the show's identity, which an open job
// for a replaced identity must not absorb, and terminal rows pruned per target.
func podcastWork(kind string) bool {
	return kind == KindPodcastPoll || kind == KindPodcastFetch || kind == KindPodcastRetain
}

// Ended reports whether j is in a state it ends in.
func (j *Job) Ended() bool {
	return j.State == StateDone || j.State == StateFailed || j.State == StateCancelled
}

type EpisodeParams struct {
	ShowID    string `json:"show_id"`
	EpisodeID string `json:"episode_id"`
}
type RetainParams struct {
	ShowID string `json:"show_id"`
}

func fetchParams(raw json.RawMessage) (json.RawMessage, error) {
	var p EpisodeParams
	if decodeStrict(raw, &p) != nil || !feed.ValidHex(p.ShowID, 16) || !feed.ValidHex(p.EpisodeID, 16) {
		return nil, invalid("invalid fetch params")
	}
	return json.Marshal(p)
}
func retainParams(raw json.RawMessage) (json.RawMessage, error) {
	var p RetainParams
	if decodeStrict(raw, &p) != nil || !feed.ValidHex(p.ShowID, 16) {
		return nil, invalid("invalid retention params")
	}
	return json.Marshal(p)
}

type SubscribeParams struct {
	ShowID        string `json:"show_id,omitempty"`
	FeedURL       string `json:"feed_url"`
	FeedURLSHA256 string `json:"feed_url_sha256"`
	FeedHost      string `json:"feed_host"`
	Private       bool   `json:"private"`
	ShowPath      string `json:"show_path"`
}
type PollParams struct {
	ShowID       string `json:"show_id,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	// Scheduled marks the scheduler's poll (server#403): only its failures
	// count toward the admin's failing-feed notice, never a follower's Check
	// now. Additive: a Bridge reads the params leniently and ignores it.
	Scheduled bool `json:"scheduled,omitempty"`
}

// PodcastOutcome is a podcast job's result (plan 2.9), as the fold and a
// listener's request both read it.
type PodcastOutcome struct {
	ShowPath          string `json:"show_path"`
	ShowID            string `json:"show_id"`
	Status            int    `json:"status"`
	ETag              string `json:"etag"`
	LastModified      string `json:"last_modified"`
	NextPollSeconds   int    `json:"next_poll_seconds"`
	Code              string `json:"code"`
	Message           string `json:"message"`
	RetryAfterSeconds *int   `json:"retry_after_seconds"`
}

// PodcastOutcomeOf decodes j's result. A failed job's code defaults to
// bridge_lost (a lease that ran out with no result) and its message to the
// job's error.
func PodcastOutcomeOf(j Job) (PodcastOutcome, error) {
	var o PodcastOutcome
	err := json.Unmarshal(j.Result, &o)
	if j.State == StateFailed {
		if o.Code == "" {
			o.Code = "bridge_lost"
		}
		if o.Message == "" {
			o.Message = j.Error
		}
	}
	return o, err
}

func subscribeParams(raw json.RawMessage) (json.RawMessage, error) {
	var p SubscribeParams
	if decodeStrict(raw, &p) != nil || !feed.ValidHex(p.FeedURLSHA256, 64) || p.FeedHost == "" || len(p.FeedHost) > feed.MaxHostLen || p.ShowPath != "" && !feed.ShowPath(p.ShowPath) {
		return nil, invalid("invalid subscribe params")
	}
	if p.Private {
		if p.FeedURL != "" {
			return nil, invalid("private URL must not be stored")
		}
	} else {
		norm, err := feed.NormalizeFeedURL(p.FeedURL)
		if err != nil || norm != p.FeedURL || feed.IsPrivateFeedURL(norm) || feed.FeedURLHash(norm) != p.FeedURLSHA256 {
			return nil, invalid("invalid public URL")
		}
	}
	return json.Marshal(p)
}

// MaxPollValidator caps each of a podcast.poll's conditional-GET validators.
const MaxPollValidator = 512

func pollParams(raw json.RawMessage) (json.RawMessage, error) {
	var p PollParams
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if decodeStrict(raw, &p) != nil || p.ShowID != "" && !feed.ValidHex(p.ShowID, 16) || len(p.ETag) > MaxPollValidator || len(p.LastModified) > MaxPollValidator {
		return nil, invalid("invalid poll params")
	}
	return json.Marshal(p)
}

// subscribePrivate is a subscribe's params' privacy flag.
func subscribePrivate(raw json.RawMessage) bool {
	var p SubscribeParams
	_ = json.Unmarshal(raw, &p)
	return p.Private
}

func subscribeSuffix(j NewJob) string {
	var p SubscribeParams
	_ = json.Unmarshal(j.Params, &p)
	return subscribeIdentity(p.FeedURLSHA256, j.RequestedBy)
}

// subscribeIdentity is a subscribe's dedupe key suffix: its feed hash and requester.
func subscribeIdentity(feedHash string, user int64) string {
	return "|" + feedHash + "|" + fmt.Sprint(user)
}

// SubscribeRequest is one podcast.subscribe to queue or adopt.
type SubscribeRequest struct {
	LibraryID int64
	User      int64
	FeedURL   string // normalized (feed.NormalizeFeedURL)
	Private   bool
	ShowPath  string // a known show this repairs, "" for a new one
	// AdoptOnly answers the open request for this feed and user (created
	// false), or ErrNoNewRequest: nothing new is queued.
	AdoptOnly bool
}

// EnqueueSubscribe is Subscribe for a request that may queue a new job.
func (s *Service) EnqueueSubscribe(ctx context.Context, lib, user int64, norm string, private bool, show string) (*Job, bool, error) {
	return s.Subscribe(ctx, SubscribeRequest{LibraryID: lib, User: user, FeedURL: norm, Private: private, ShowPath: show})
}

// Subscribe transfers a private URL only into volatile per-job memory. The
// same lock covers its claim, so a freshly queued job cannot expire between the
// commit and the memory write. It remains available for lease retries. The
// open request for the same feed and user is adopted only with the same
// privacy (ErrPrivacyConflict otherwise), checked in the dedupe transaction.
func (s *Service) Subscribe(ctx context.Context, r SubscribeRequest) (*Job, bool, error) {
	lib, user, norm, private, show := r.LibraryID, r.User, r.FeedURL, r.Private, r.ShowPath
	u, err := url.Parse(norm)
	if err != nil {
		return nil, false, feed.ErrInvalidFeedURL
	}
	p := SubscribeParams{FeedURL: norm, FeedURLSHA256: feed.FeedURLHash(norm), FeedHost: u.Hostname(), Private: private, ShowPath: show}
	if show != "" {
		// A show pruned since the caller found it has no identity left to bind:
		// the repair proceeds like a new subscription instead of failing.
		err := s.db.QueryRowContext(ctx, `SELECT show_id FROM podcast_shows WHERE library_id=? AND show_path=?`, lib, show).Scan(&p.ShowID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
	}
	if private {
		p.FeedURL = ""
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, false, err
	}
	nj := NewJob{Kind: KindPodcastSubscribe, LibraryID: &lib, Params: raw, RequestedBy: user, Priority: PriorityListener, feedRedacted: feed.RedactURL(norm), adoptOnly: r.AdoptOnly}
	// Enqueue's own checks, made once here (enqueueChecked below), before an
	// adopt-only request looks for its open one: a row that could never be
	// queued says why (ErrInvalid, ErrNotPodcastLibrary) rather than
	// ErrNoNewRequest (server#219).
	if err := s.checkNewJob(ctx, &nj); err != nil {
		return nil, false, err
	}
	if r.AdoptOnly {
		// An OPML row past the upload's new requests only adopts an open one:
		// with none open it is refused at once, without the lock or Enqueue's
		// writer transaction (server#219). One opened since this read is
		// adopted by the next upload.
		open, err := openJob(ctx, s.db, jobKey(nj))
		if err != nil {
			return nil, false, err
		}
		if open == nil {
			return nil, false, ErrNoNewRequest
		}
	}
	s.secretFlowMu.Lock()
	defer s.secretFlowMu.Unlock()
	j, created, err := s.enqueueChecked(ctx, nj)
	if err == nil && private {
		if s.beforeSecretHeld != nil {
			s.beforeSecretHeld(j)
		}
		s.secretMu.Lock()
		s.privateURLs[j.ID] = norm
		s.secretMu.Unlock()
		s.dropIfEnded(ctx, j.ID)
	}
	return j, created, err
}

// dropIfEnded lets go of the private URL just stored for job id when the job has
// already ended. reportEnded drops a job's URL once its end has committed, but
// an end that committed after Enqueue and before the URL was stored found
// nothing to drop (server#200): this read, after the store, sees that end, and
// any end it doesn't see is still to be reported, after the store. Without the
// read the URL waits for PruneSecrets.
func (s *Service) dropIfEnded(ctx context.Context, id int64) {
	var open bool
	err := s.db.QueryRowContext(context.WithoutCancel(ctx),
		`SELECT EXISTS(SELECT 1 FROM bridge_jobs WHERE id=? AND state IN `+OpenStatesSQL+`)`, id).Scan(&open)
	if err == nil && !open {
		s.secretMu.Lock()
		delete(s.privateURLs, id)
		s.secretMu.Unlock()
	}
}

// notFeedFailures are the podcast job failure codes (a subscribe's or a
// poll's) that say nothing about the feed: the Bridge or the library failed
// (bridge_lost: the lease ran out on every attempt) or a private request lost
// its URL. Every other code (the
// feed's answer, its address, the network) is the feed's failure.
var notFeedFailures = []string{"request_expired", "bridge_lost", "library_not_writable", "not_podcast_library", "write_failed", "feed_secret_missing", "identity_changed"}

// FeedFailure reports whether a failed podcast job's code is the feed's own
// failure (its answer, its address, the network), not one of notFeedFailures
// (the Bridge, the library or a lost private URL): the admin's failing-feed
// notification counts only these (catalog.FoldPodcastJob).
func FeedFailure(code string) bool {
	return code != "" && !slices.Contains(notFeedFailures, code)
}

// networkFailures are the feed failure codes the Bridge raises when it could not
// reach the feed at all (its netguard: the host unresolved, no connection, no
// answer in time, a secure connection it could not verify), what every show
// fails with at once while the Bridge's own internet is down.
var networkFailures = []string{"dns", "network", "timeout", "tls"}

// NetworkFailure reports whether a feed failure's code is network-class (one of
// networkFailures), which the admin's failing-feed notification needs twice as
// many of in a row (catalog.feedRun), so an outage at the Bridge doesn't
// announce every followed show.
func NetworkFailure(code string) bool {
	return slices.Contains(networkFailures, code)
}

// unsentFailures are the feed failure codes the Bridge raises before it
// connects to anything: the address refused as written, or as resolved.
var unsentFailures = []string{"invalid_feed_url", "blocked_address"}

// FeedAnswered reports whether a failed podcast job's code says the Bridge
// reached the feed: the feed's own failure (FeedFailure), not network-class
// and not refused before any connection (unsentFailures). An HTTP error, a
// redirect loop or a body that is no feed all came from the feed's host, so
// the Bridge's internet was up (catalog.FoldPodcastJob's outage check).
func FeedAnswered(code string) bool {
	return FeedFailure(code) && !NetworkFailure(code) && !slices.Contains(unsentFailures, code)
}

// RecentSubscribeFailure is user's last podcast.subscribe for norm in lib when
// it failed within the last `within` (the Service's clock), else nil: an OPML
// import passes such a feed over instead of queueing it again. A failure that
// isn't the feed's (notFeedFailures) never counts.
func (s *Service) RecentSubscribeFailure(ctx context.Context, lib, user int64, norm string, within time.Duration) (*Job, error) {
	if err := s.expireDue(ctx); err != nil {
		return nil, err
	}
	key := DedupeKey(KindPodcastSubscribe, &lib, nil) + subscribeIdentity(feed.FeedURLHash(norm), user)
	// The newest request for the feed that ended inside the window (the window
	// bounds the read, the person's whole history doesn't)...
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+jobFrom+` WHERE j.kind='`+KindPodcastSubscribe+`' AND j.finished_at>=? AND j.dedupe_key=? ORDER BY j.id DESC LIMIT 1`, at(s.now().Add(-within)), key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if j.State != StateFailed {
		return nil, nil
	}
	if o, _ := PodcastOutcomeOf(*j); !FeedFailure(o.Code) {
		return nil, nil
	}
	// ...counts only while it is still the last request for it.
	var newer bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM bridge_jobs WHERE kind='`+KindPodcastSubscribe+`' AND requested_by=? AND id>? AND dedupe_key=?)`, user, j.ID, key).Scan(&newer); err != nil || newer {
		return nil, err
	}
	r := RedactedJob(*j)
	return &r, nil
}

// PruneSecrets keeps a private feed URL only while its podcast.subscribe is
// open, so a credential never outlives its request in memory: a deleted
// library's jobs go with it (cascaded), and a request that ended while
// Subscribe was adopting it lets go too. Once called it runs to the end (the
// delete it follows has committed). The same lock as Subscribe's, so a request
// being queued is never mistaken for a gone one.
func (s *Service) PruneSecrets(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	s.secretFlowMu.Lock()
	defer s.secretFlowMu.Unlock()
	s.secretMu.Lock()
	held := len(s.privateURLs)
	s.secretMu.Unlock()
	if held == 0 {
		return nil
	}
	open, err := queryIDs(ctx, s.db, `SELECT id FROM bridge_jobs WHERE kind=? AND state IN `+OpenStatesSQL, KindPodcastSubscribe)
	if err != nil {
		return err
	}
	s.secretMu.Lock()
	maps.DeleteFunc(s.privateURLs, func(id int64, _ string) bool { return !slices.Contains(open, id) })
	s.secretMu.Unlock()
	return nil
}

// ExpirePrivateRequests also runs before claims. It never hands out a private
// subscription whose credential was lost during a server restart.
func (s *Service) ExpirePrivateRequests(ctx context.Context) error {
	s.secretFlowMu.Lock()
	defer s.secretFlowMu.Unlock()
	var ended []Job
	err := s.db.WithTx(ctx, "bridge.expirePrivate", func(tx *sql.Tx) error {
		var err error
		ended, err = s.expirePrivateTx(ctx, tx, s.ts())
		return err
	})
	if err == nil {
		for _, j := range ended {
			s.report(ctx, &j, StateQueued, "")
		}
	}
	return err
}

// expirePrivateTx follows lease expiry in the claim transaction too: a lease
// requeued after a restart must fail before nextQueued can hand it out.
func (s *Service) expirePrivateTx(ctx context.Context, tx *sql.Tx, now string) ([]Job, error) {
	queued, err := s.lostPrivateRequests(ctx, tx)
	if err != nil {
		return nil, err
	}
	return expireRequestsTx(ctx, tx, queued, now, "The follow request expired. Follow the feed again.")
}

// expireRequestsTx fails the follow requests ids (code request_expired, which
// the player reads as "follow again") with message, and returns them as ended.
func expireRequestsTx(ctx context.Context, tx *sql.Tx, ids []int64, now, message string) ([]Job, error) {
	result, err := json.Marshal(FetchError{Code: "request_expired", Message: message})
	if err != nil {
		return nil, err
	}
	ended := []Job{}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE bridge_jobs SET state='failed',error=?1,result=?2,finished_at=?3,updated_at=?3 WHERE id=?4`, message, string(result), now, id); err != nil {
			return nil, err
		}
		got, err := getJob(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		ended = append(ended, *got)
	}
	return ended, nil
}

// offlineSubscribesSQL is the queued follow requests, with literals so the
// partial idx_bridge_jobs_queue (state = 'queued') serves it.
const offlineSubscribesSQL = `FROM bridge_jobs WHERE state='queued' AND kind='` + KindPodcastSubscribe + `'`

// offlineRequestMessage is a follow request expireOfflineSubscribes failed.
const offlineRequestMessage = "The Bridge went offline before it took up the follow request. Follow the feed again."

// expireOfflineSubscribes fails the queued follow requests while no online
// Bridge runs podcast.subscribe (its heartbeat lapsed, it was unpaired, or it
// stopped offering the kind), so a request never waits on a Bridge that left:
// an OPML import's next wave then answers its rows unavailable instead of
// waiting on them (server#179). The silence must have lasted OnlineWindow since
// a sweep first saw it (offlineLasted), so a server restart, a host waking from
// sleep or a wall clock that jumped fails none before the Bridge's next beat.
// Run by the player's poll of its requests (SubscribeRequests) and the
// scheduler's sweep (EndedPodcasts).
func (s *Service) expireOfflineSubscribes(ctx context.Context) error {
	offline := func(h *Heartbeat) bool { return !h.Online(s.now()) || !h.Offers(KindPodcastSubscribe) }
	h, err := s.LastHeartbeat(ctx)
	if err != nil || !s.offlineLasted(offline(h)) {
		return err
	}
	// The reader pool first (idx_bridge_jobs_queue): the writer only when
	// there is a request to fail.
	var queued bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 `+offlineSubscribesSQL+`)`).Scan(&queued); err != nil || !queued {
		return err
	}
	var ended []Job
	s.secretFlowMu.Lock()
	err = s.db.WithTx(ctx, "bridge.expireOffline", func(tx *sql.Tx) error {
		// The heartbeat again, in the transaction: one saved since the cached
		// read keeps every request.
		h, err := readHeartbeat(ctx, tx)
		if err != nil || !offline(h) {
			return err
		}
		ids, err := queryIDs(ctx, tx, `SELECT id `+offlineSubscribesSQL)
		if err != nil {
			return err
		}
		ended, err = expireRequestsTx(ctx, tx, ids, s.ts(), offlineRequestMessage)
		return err
	})
	s.secretFlowMu.Unlock()
	if err != nil {
		return err
	}
	// Reported once the lock is let go: each end's fold has a budget of its own
	// (foldContext), which a claim or a follow must not wait behind. The
	// requests have ended, so none can be adopted or claimed meanwhile, and
	// reportEnded drops a private URL whenever it runs (Subscribe's dropIfEnded
	// covers one stored in between).
	for _, j := range ended {
		s.report(ctx, &j, StateQueued, "")
	}
	return nil
}

// offlineLasted records one sighting of the Bridge (offline or not) and reports
// whether it has been offline at every sighting for OnlineWindow, counted on the
// Service's clock from the first.
func (s *Service) offlineLasted(offline bool) bool {
	s.offlineMu.Lock()
	defer s.offlineMu.Unlock()
	if !offline {
		s.offlineSince = time.Time{}
		return false
	}
	now := s.now()
	if s.offlineSince.IsZero() {
		s.offlineSince = now
	}
	return now.Sub(s.offlineSince) >= OnlineWindow
}

// lostPrivateRequests is the queued private subscriptions whose feed URL this
// server doesn't hold (it restarted since they were asked for), or whose
// requester's account was deleted since (requested_by is set NULL: a private
// feed is that person's credential, never followed for nobody), read through q.
// One whose params can't be read is passed by (ParamSQL): its claim ends it (claimPriority), where a
// malformed row here would fail every claim.
func (s *Service) lostPrivateRequests(ctx context.Context, q rowsQuerier) ([]int64, error) {
	type request struct {
		id       int64
		orphaned bool
	}
	rows, err := q.QueryContext(ctx, `SELECT id, requested_by IS NULL FROM bridge_jobs WHERE kind=? AND state=? AND `+ParamSQL("params", "$.private")+`=1`, KindPodcastSubscribe, StateQueued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var queued []request
	for rows.Next() {
		var r request
		if err := rows.Scan(&r.id, &r.orphaned); err != nil {
			return nil, err
		}
		queued = append(queued, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	var lost []int64
	for _, r := range queued {
		if r.orphaned || s.privateURLs[r.id] == "" {
			lost = append(lost, r.id)
		}
	}
	return lost, nil
}

func queryIDs(ctx context.Context, db rowsQuerier, q string, args ...any) ([]int64, error) {
	return queryColumn[int64](ctx, db, q, args...)
}

// queryColumn reads a query's one column.
func queryColumn[T any](ctx context.Context, db rowsQuerier, q string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Service) reportEnded(ctx context.Context, j *Job, from string) {
	if from == j.State || !j.Ended() {
		return
	}
	s.secretMu.Lock()
	delete(s.privateURLs, j.ID)
	s.secretMu.Unlock()
	if IsPodcast(j.Kind) {
		// Only podcast outcomes are folded; other kinds needn't wake the scheduler.
		s.ended.fire()
	}
	if s.OnEnded != nil {
		fold, cancel := foldContext(ctx)
		defer cancel()
		s.OnEnded(fold, *j)
	}
}

// Ended is a wakeup for background folding, independent of notification policy.
func (s *Service) Ended() <-chan struct{} { return s.ended.wait() }

// podcastURLs runs a URL to the next space, whatever it holds: a quote or angle
// bracket inside a private URL mustn't end the match and leave the rest behind.
var podcastURLs = regexp.MustCompile(`(?i)https?://\S+`)

func redactPodcastText(s string) string { return podcastURLs.ReplaceAllStringFunc(s, feed.RedactURL) }
func redactPodcastJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return json.RawMessage(`{}`)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return redactPodcastText(x)
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		case map[string]any:
			clean := make(map[string]any, len(x))
			for k, v := range x {
				clean[redactPodcastText(k)] = walk(v)
			}
			return clean
		}
		return v
	}
	b, err := json.Marshal(walk(v))
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// RedactedJob is suitable for console and listener responses; claim injection is
// the sole response path that carries a private credential. Stored JSON that
// can't be read (a damaged row) answers null (readableJob), flagged for params.
func RedactedJob(j Job) Job {
	j = readableJob(j)
	if !IsPodcast(j.Kind) {
		return j
	}
	if j.Kind == KindPodcastSubscribe {
		var p SubscribeParams
		if json.Unmarshal(j.Params, &p) == nil {
			if p.FeedURL != "" {
				p.FeedURL = feed.RedactURL(p.FeedURL)
			}
			j.Params, _ = json.Marshal(p)
		}
	}
	j.Error = redactPodcastText(j.Error)
	j.Progress = redactPodcastJSON(j.Progress)
	j.Result = redactPodcastJSON(j.Result)
	return j
}

// readableJob is j with its stored params, progress and result that aren't
// JSON (a damaged row) set to null, and its params flagged so
// (ParamsUnreadable): encoding a json.RawMessage that isn't JSON fails, and a
// handler that has written its 200 by then answers an empty body.
func readableJob(j Job) Job {
	if j.Params != nil && !json.Valid(j.Params) {
		j.Params, j.ParamsUnreadable = nil, true
	}
	if j.Progress != nil && !json.Valid(j.Progress) {
		j.Progress = nil
	}
	if j.Result != nil && !json.Valid(j.Result) {
		j.Result = nil
	}
	return j
}

// PollFoldWindow is how long an ended poll (or follow) waits for its fold:
// EndedPodcasts sweeps those that ended within it. The scheduler counts an
// unfolded poll as the show's poll in hand over a shorter window of its own
// (catalog.DuePodcastPolls).
const PollFoldWindow = 24 * time.Hour

// UnfoldedSQL holds for a bridge_jobs row aliased j whose outcome is not folded
// yet: the sweep's rule, shared with the scheduler's poll in hand.
const UnfoldedSQL = `NOT EXISTS(SELECT 1 FROM podcast_folded f WHERE f.job_id=j.id)`

// EndedPodcasts recovers a crash between the job commit and folding
// (idx_bridge_jobs_kind_finished serves it), after failing the follow requests
// left queued on a Bridge that went offline (expireOfflineSubscribes).
func (s *Service) EndedPodcasts(ctx context.Context) ([]Job, error) {
	if err := s.expireDue(ctx); err != nil {
		return nil, err
	}
	if err := s.expireOfflineSubscribes(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+jobFrom+` WHERE j.kind IN ('`+KindPodcastSubscribe+`','`+KindPodcastPoll+`','`+KindPodcastFetch+`','`+KindPodcastRetain+`') AND j.state IN `+EndedStatesSQL+` AND (j.kind IN ('`+KindPodcastFetch+`','`+KindPodcastRetain+`') OR j.finished_at>=?) AND `+UnfoldedSQL+` ORDER BY j.id`, at(s.now().Add(-PollFoldWindow)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// SubscribeRequests returns only this person's recent requests, never URLs held
// in memory, and includes all open requests even if they are older than a week.
// A request still queued while no online Bridge runs podcast.subscribe is
// failed first (expireOfflineSubscribes).
func (s *Service) SubscribeRequests(ctx context.Context, user int64) ([]Job, error) {
	if err := s.expireDue(ctx); err != nil {
		return nil, err
	}
	if err := s.expireOfflineSubscribes(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+jobFrom+` WHERE j.kind='`+KindPodcastSubscribe+`' AND j.requested_by=? AND (j.state IN `+OpenStatesSQL+` OR j.finished_at>=?) ORDER BY j.id DESC`, user, at(s.now().Add(-7*24*time.Hour)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, RedactedJob(*j))
	}
	return out, rows.Err()
}

// checkLibraryKind refuses a job of a kind for one kind of library
// (kindSpec.libraryKind) on a library of the other.
func (s *Service) checkLibraryKind(ctx context.Context, j NewJob) error {
	want := kinds[j.Kind].libraryKind
	if want == "" {
		return nil
	}
	var kind string
	if err := s.db.QueryRowContext(ctx, `SELECT kind FROM libraries WHERE id=?`, j.LibraryID).Scan(&kind); err != nil {
		return err
	}
	switch {
	case kind == want:
		return nil
	case want == libraryPodcasts:
		return ErrNotPodcastLibrary
	default:
		return ErrNotBookLibrary
	}
}

// prunePolls keeps only the newest ended poll of a show (the dedupe key's
// kind, library and path, which idx_bridge_jobs_book serves).
func (s *Service) prunePolls(ctx context.Context, tx *sql.Tx, j NewJob) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM bridge_jobs WHERE library_id=?1 AND rel_path=?2 AND kind=?3 AND state IN `+EndedStatesSQL+`
		AND (kind='podcast.poll' OR EXISTS(SELECT 1 FROM podcast_folded f WHERE f.job_id=bridge_jobs.id)) AND id<>(SELECT MAX(id) FROM bridge_jobs WHERE library_id=?1 AND rel_path=?2 AND kind=?3 AND state IN `+EndedStatesSQL+`)`, j.LibraryID, j.Path, j.Kind)
	return err
}
func sanitizePodcastUpdate(u *Update) {
	if u.Error != nil {
		v := redactPodcastText(*u.Error)
		u.Error = &v
	}
	u.Progress = redactPodcastJSON(u.Progress)
	u.Result = redactPodcastJSON(u.Result)
}

func validShowPath(p *string) bool {
	return p != nil && feed.ShowPath(*p)
}

func (s *Service) claim(ctx context.Context, token int64, kinds []string) (*Job, error) {
	return s.claimPriority(ctx, token, kinds, nil)
}

// claimPriority claims the next job (claimSecret), then gives a claimed
// audible.sync its titles (fillAudibleSync) outside the podcast secrets' lock.
// A job of any kind whose stored params can't be read (not the JSON its
// queueing stored, or for a podcast.subscribe not its params) can never be
// handed out, so its first claim ends it (paramsUnreadReason): answered as it
// is, its encoding would fail after the 200 and the empty claim would put it
// back at the front of the queue. A job ended so (failed, never claimed again)
// hands out the job behind it.
func (s *Service) claimPriority(ctx context.Context, token int64, kinds []string, minPriority *int32) (*Job, error) {
	for {
		j, err := s.claimSecret(ctx, token, kinds, minPriority)
		if err != nil || j == nil {
			return j, err
		}
		if j.ParamsUnreadable || !json.Valid(j.Params) {
			if err := s.endUnfillableClaim(ctx, j, paramsUnreadReason(j.Kind),
				fmt.Errorf("%s %d: stored params unreadable", j.Kind, j.ID),
				"bridge: a claimed job ended, its stored params unreadable", "kind", j.Kind); err != nil {
				return nil, err
			}
			continue
		}
		if j.Kind != KindAudibleSync || s.ClaimAudibleSync == nil {
			return j, nil
		}
		if j, err = s.fillAudibleSync(ctx, j); j != nil || err != nil {
			return j, err
		}
	}
}

// paramsUnread is the error of a job ended at its claim because the params
// stored when it was queued can't be read (claimPriority); a podcast.subscribe
// and an audible.sync name what they lost.
const (
	paramsUnread          = "The server could not read this job's options."
	subscribeParamsUnread = "The server could not read which feed this subscription is for."
)

// paramsUnreadReason is the error a job of kind ends with when its claim finds
// its stored params unreadable.
func paramsUnreadReason(kind string) string {
	switch kind {
	case KindPodcastSubscribe:
		return subscribeParamsUnread
	case KindAudibleSync:
		return syncParamsUnread
	}
	return paramsUnread
}

// claimSecret claims the next job, a private podcast.subscribe with its feed
// URL put back from memory, under secretFlowMu. A podcast.subscribe whose
// params can't be read comes back as stored, flagged (ParamsUnreadable), for
// claimPriority to end outside the lock.
func (s *Service) claimSecret(ctx context.Context, token int64, kinds []string, minPriority *int32) (*Job, error) {
	s.secretFlowMu.Lock()
	defer s.secretFlowMu.Unlock()
	j, err := s.claimStored(ctx, token, kinds, minPriority)
	if err != nil || j == nil {
		return j, err
	}
	if j.Kind == KindPodcastSubscribe {
		var p SubscribeParams
		if err := json.Unmarshal(j.Params, &p); err != nil {
			j.ParamsUnreadable = true
			return j, nil
		}
		if p.Private {
			s.secretMu.Lock()
			p.FeedURL = s.privateURLs[j.ID]
			s.secretMu.Unlock()
			j.Params, err = json.Marshal(p)
		}
	}
	return j, err
}

var ErrIdentityChanged = errors.New("podcast identity changed")

func validEpisodePath(p string) bool { return feed.EpisodePath(strings.SplitN(p, "/", 2)[0], p) }
func samePodcastIdentity(a, b json.RawMessage) bool {
	var x, y EpisodeParams
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	return x.ShowID == y.ShowID && x.EpisodeID == y.EpisodeID
}

// checkPodcastTarget refuses podcast work naming an identity its show or
// episode no longer has (ErrIdentityChanged), so a stale request never replaces
// the current one's open job. A poll's show may have no row yet: its identity is
// then none ("").
func checkPodcastTarget(ctx context.Context, tx *sql.Tx, j NewJob) error {
	switch j.Kind {
	case KindPodcastFetch, KindPodcastRetain, KindPodcastPoll:
	default:
		return nil
	}
	var p EpisodeParams // a poll's and a retain's show_id read the same
	_ = json.Unmarshal(j.Params, &p)
	var showID, episodeID string
	var err error
	if j.Kind == KindPodcastFetch {
		err = tx.QueryRowContext(ctx, `SELECT s.show_id,e.episode_id FROM podcast_episodes e JOIN books b ON b.id=e.book_id JOIN podcast_shows s ON s.library_id=e.library_id AND s.show_path=e.show_path WHERE b.library_id=? AND b.rel_path=?`, j.LibraryID, j.Path).Scan(&showID, &episodeID)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT show_id FROM podcast_shows WHERE library_id=? AND show_path=?`, j.LibraryID, j.Path).Scan(&showID)
	}
	if errors.Is(err, sql.ErrNoRows) && j.Kind != KindPodcastPoll {
		return ErrNotFound
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if showID != p.ShowID || j.Kind == KindPodcastFetch && episodeID != p.EpisodeID {
		return ErrIdentityChanged
	}
	return nil
}

// WakePodcasts asks the scheduler to reconsider policies and follows immediately.
func (s *Service) WakePodcasts() { s.ended.fire() }

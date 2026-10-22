package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	if decodeStrict(raw, &p) != nil || !feed.ValidHex(p.FeedURLSHA256, 64) || p.FeedHost == "" || len(p.FeedHost) > 253 || p.ShowPath != "" && !feed.ShowPath(p.ShowPath) {
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
	return "|" + p.FeedURLSHA256 + "|" + fmt.Sprint(j.RequestedBy)
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
	s.secretFlowMu.Lock()
	defer s.secretFlowMu.Unlock()
	j, created, err := s.Enqueue(ctx, NewJob{Kind: KindPodcastSubscribe, LibraryID: &lib, Params: raw, RequestedBy: user, Priority: 10, feedRedacted: feed.RedactURL(norm), adoptOnly: r.AdoptOnly})
	if err == nil && private {
		s.secretMu.Lock()
		s.privateURLs[j.ID] = norm
		s.secretMu.Unlock()
	}
	return j, created, err
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
	queued, err := queryIDs(ctx, tx, `SELECT id FROM bridge_jobs WHERE kind=? AND state=? AND json_extract(params,'$.private')=1`, KindPodcastSubscribe, StateQueued)
	if err != nil {
		return nil, err
	}
	s.secretMu.Lock()
	queued = slices.DeleteFunc(queued, func(id int64) bool { return s.privateURLs[id] != "" })
	s.secretMu.Unlock()
	ended := []Job{}
	for _, id := range queued {
		if _, err := tx.ExecContext(ctx, `UPDATE bridge_jobs SET state='failed',error='The follow request expired. Follow the feed again.',result='{"code":"request_expired","message":"The follow request expired. Follow the feed again."}',finished_at=?1,updated_at=?1 WHERE id=?2`, now, id); err != nil {
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
func queryIDs(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
		committed, cancel := committedContext(ctx)
		defer cancel()
		s.OnEnded(committed, *j)
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
// the sole response path that carries a private credential.
func RedactedJob(j Job) Job {
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

// EndedPodcasts recovers a crash between the job commit and folding
// (idx_bridge_jobs_kind_finished serves it).
func (s *Service) EndedPodcasts(ctx context.Context) ([]Job, error) {
	if err := s.expireDue(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+jobFrom+` WHERE j.kind IN ('`+KindPodcastSubscribe+`','`+KindPodcastPoll+`','`+KindPodcastFetch+`','`+KindPodcastRetain+`') AND j.state IN `+EndedStatesSQL+` AND (j.kind IN ('`+KindPodcastFetch+`','`+KindPodcastRetain+`') OR j.finished_at>=?) AND NOT EXISTS(SELECT 1 FROM podcast_folded f WHERE f.job_id=j.id) ORDER BY j.id`, at(s.now().Add(-24*time.Hour)))
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
func (s *Service) SubscribeRequests(ctx context.Context, user int64) ([]Job, error) {
	if err := s.expireDue(ctx); err != nil {
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
func (s *Service) claimPriority(ctx context.Context, token int64, kinds []string, minPriority *int32) (*Job, error) {
	s.secretFlowMu.Lock()
	defer s.secretFlowMu.Unlock()
	j, err := s.claimStored(ctx, token, kinds, minPriority)
	if err != nil || j == nil {
		return j, err
	}
	if j.Kind == KindPodcastSubscribe {
		var p SubscribeParams
		if err := json.Unmarshal(j.Params, &p); err != nil {
			return nil, err
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
func checkPodcastTarget(ctx context.Context, tx *sql.Tx, j NewJob) error {
	if j.Kind != KindPodcastFetch && j.Kind != KindPodcastRetain {
		return nil
	}
	var p EpisodeParams
	_ = json.Unmarshal(j.Params, &p)
	var showID, episodeID string
	var err error
	if j.Kind == KindPodcastFetch {
		err = tx.QueryRowContext(ctx, `SELECT s.show_id,e.episode_id FROM podcast_episodes e JOIN books b ON b.id=e.book_id JOIN podcast_shows s ON s.library_id=e.library_id AND s.show_path=e.show_path WHERE b.library_id=? AND b.rel_path=?`, j.LibraryID, j.Path).Scan(&showID, &episodeID)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT show_id FROM podcast_shows WHERE library_id=? AND show_path=?`, j.LibraryID, j.Path).Scan(&showID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if showID != p.ShowID || j.Kind == KindPodcastFetch && episodeID != p.EpisodeID {
		return ErrIdentityChanged
	}
	return nil
}

// WakePodcasts asks the scheduler to reconsider policies and follows immediately.
func (s *Service) WakePodcasts() { s.ended.fire() }

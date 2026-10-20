package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

const (
	KindPodcastSubscribe = "podcast.subscribe"
	KindPodcastPoll      = "podcast.poll"
	ModulePodcasts       = "podcasts"
	FeaturePodcast       = "podcast"
)

var ErrTooManyRequests = errors.New("too many open subscribe requests")
var ErrNotPodcastLibrary = errors.New("not a podcast library")
var ErrNotBookLibrary = errors.New("not a book library")

func IsPodcast(kind string) bool { return kind == KindPodcastSubscribe || kind == KindPodcastPoll }
func Internal(kind string) bool  { return kinds[kind].internal }

type SubscribeParams struct {
	FeedURL       string `json:"feed_url"`
	FeedURLSHA256 string `json:"feed_url_sha256"`
	FeedHost      string `json:"feed_host"`
	Private       bool   `json:"private"`
	ShowPath      string `json:"show_path"`
}
type PollParams struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

func subscribeParams(raw json.RawMessage) (json.RawMessage, error) {
	var p SubscribeParams
	if decodeStrict(raw, &p) != nil || len(p.FeedURLSHA256) != 64 || p.FeedHost == "" || len(p.FeedHost) > 253 || p.ShowPath != "" && !feed.ShowPath(p.ShowPath) {
		return nil, invalid("invalid subscribe params")
	}
	for _, c := range p.FeedURLSHA256 {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, invalid("invalid feed hash")
		}
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
func pollParams(raw json.RawMessage) (json.RawMessage, error) {
	var p PollParams
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if decodeStrict(raw, &p) != nil || len(p.ETag) > 512 || len(p.LastModified) > 512 {
		return nil, invalid("invalid poll params")
	}
	return json.Marshal(p)
}
func subscribeSuffix(j NewJob) string {
	var p SubscribeParams
	_ = json.Unmarshal(j.Params, &p)
	return "|" + p.FeedURLSHA256 + "|" + fmt.Sprint(j.RequestedBy)
}

// EnqueueSubscribe transfers a private URL only into volatile per-job memory.
// The same lock covers its claim, so a freshly queued job cannot expire between
// the commit and the memory write. It remains available for lease retries.
func (s *Service) EnqueueSubscribe(ctx context.Context, lib, user int64, norm string, private bool, show string) (*Job, bool, error) {
	u, err := url.Parse(norm)
	if err != nil {
		return nil, false, feed.ErrInvalidFeedURL
	}
	p := SubscribeParams{FeedURL: norm, FeedURLSHA256: feed.FeedURLHash(norm), FeedHost: u.Hostname(), Private: private, ShowPath: show}
	if private {
		p.FeedURL = ""
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, false, err
	}
	s.secretFlowMu.Lock()
	defer s.secretFlowMu.Unlock()
	j, created, err := s.Enqueue(ctx, NewJob{Kind: KindPodcastSubscribe, LibraryID: &lib, Params: raw, RequestedBy: user, Priority: 10, feedRedacted: feed.RedactURL(norm)})
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
	return s.expirePrivate(ctx)
}
func (s *Service) expirePrivate(ctx context.Context) error {
	var ended []Job
	err := s.db.WithTx(ctx, "bridge.expirePrivate", func(tx *sql.Tx) error { var err error; ended, err = s.expirePrivateTx(ctx, tx, s.ts()); return err })
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
	rows, err := tx.QueryContext(ctx, `SELECT `+jobColumns+jobFrom+` WHERE j.kind='podcast.subscribe' AND j.state='queued'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		var p SubscribeParams
		if err := json.Unmarshal(j.Params, &p); err != nil {
			return nil, err
		}
		s.secretMu.Lock()
		missing := s.privateURLs[j.ID] == ""
		s.secretMu.Unlock()
		if p.Private && missing {
			candidates = append(candidates, j)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	ended := []Job{}
	for _, j := range candidates {
		if _, err := tx.ExecContext(ctx, `UPDATE bridge_jobs SET state='failed',error='The follow request expired. Follow the feed again.',result='{"code":"request_expired","message":"The follow request expired. Follow the feed again."}',finished_at=?1,updated_at=?1 WHERE id=?2`, now, j.ID); err != nil {
			return nil, err
		}
		got, err := getJob(ctx, tx, j.ID)
		if err != nil {
			return nil, err
		}
		ended = append(ended, *got)
	}
	return ended, nil
}
func (s *Service) reportEnded(ctx context.Context, j *Job, from string) {
	if from == j.State || j.State != StateDone && j.State != StateFailed && j.State != StateCancelled {
		return
	}
	s.secretMu.Lock()
	delete(s.privateURLs, j.ID)
	s.secretMu.Unlock()
	s.ended.fire()
	if s.OnEnded != nil {
		s.OnEnded(context.WithoutCancel(ctx), *j)
	}
}

// Ended is a wakeup for background folding, independent of notification policy.
func (s *Service) Ended() <-chan struct{} { return s.ended.wait() }

var podcastURLs = regexp.MustCompile(`(?i)https?://[^\s"<>]+`)

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

// EndedPodcasts recovers a crash between the job commit and folding.
func (s *Service) EndedPodcasts(ctx context.Context) ([]Job, error) {
	if err := s.expireDue(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+jobFrom+` WHERE j.kind IN ('podcast.subscribe','podcast.poll') AND j.state IN ('done','failed','cancelled') AND j.finished_at>=? AND NOT EXISTS(SELECT 1 FROM podcast_folded f WHERE f.job_id=j.id) ORDER BY j.id`, at(s.now().Add(-24*time.Hour)))
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+jobFrom+` WHERE j.kind='podcast.subscribe' AND j.requested_by=? AND (j.state IN `+OpenStatesSQL+` OR j.finished_at>=?) ORDER BY j.id DESC`, user, at(s.now().Add(-7*24*time.Hour)))
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
func (s *Service) checkPodcastLibrary(ctx context.Context, j NewJob) error {
	spec := kinds[j.Kind]
	if !IsPodcast(j.Kind) && !spec.booksOnly {
		return nil
	}
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM libraries WHERE id=?`, j.LibraryID).Scan(&kind)
	if err != nil {
		return err
	}
	if IsPodcast(j.Kind) && kind != "podcasts" {
		return ErrNotPodcastLibrary
	}
	if spec.booksOnly && kind != "books" {
		return ErrNotBookLibrary
	}
	return nil
}
func (s *Service) prunePolls(ctx context.Context, tx *sql.Tx, key string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM bridge_jobs WHERE dedupe_key=?1 AND state IN ('done','failed','cancelled') AND id<>(SELECT MAX(id) FROM bridge_jobs WHERE dedupe_key=?1 AND state IN ('done','failed','cancelled'))`, key)
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
	return p != nil && feed.ShowPath(*p) && !strings.ContainsAny(*p, "\r\n")
}

func (s *Service) claim(ctx context.Context, token int64, kinds []string) (*Job, error) {
	s.secretFlowMu.Lock()
	defer s.secretFlowMu.Unlock()
	j, err := s.claimStored(ctx, token, kinds)
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

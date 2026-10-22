package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

var (
	ErrFetchCooldown  = errors.New("podcast fetch is cooling down")
	ErrFetchModuleOff = errors.New("podcasts module is off")
	ErrFetchDemo      = errors.New("demo cannot start a podcast fetch")
)

// FetchOptions belongs to the server's trusted trigger, never a request body.
type FetchOptions struct {
	Retry bool
	NoNew bool
}

// EnsurePodcastFetch is the single transactional entry for every fetch trigger.
// An open intent wins even when new disk work cannot be authorized. Promotion
// never changes its attempt, lease, progress or requester.
func (s *Service) EnsurePodcastFetch(ctx context.Context, j NewJob, opt FetchOptions) (*Job, bool, error) {
	j.Kind = KindPodcastFetch
	j.fetch = &opt
	return s.Enqueue(ctx, j)
}

func (s *Service) fetchRefusal(h *Heartbeat, lib int64, noNew bool) error {
	switch {
	case noNew:
		return ErrFetchDemo
	case s.ModuleOn != nil && !s.ModuleOn(ModulePodcasts):
		return ErrFetchModuleOff
	case h == nil || !h.Online(s.now()) || !h.Offers(KindPodcastFetch):
		return ErrKindUnavailable
	case !h.Writable(lib):
		return ErrLibraryNotWritable
	default:
		return nil
	}
}

func fetchCooling(ctx context.Context, tx *sql.Tx, j NewJob, now time.Time) (bool, error) {
	var p EpisodeParams
	if err := json.Unmarshal(j.Params, &p); err != nil {
		return false, err
	}
	var next sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT next_attempt_at FROM podcast_fetch_state WHERE library_id=? AND rel_path=? AND show_id=?`, j.LibraryID, j.Path, p.ShowID).Scan(&next)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if next.Valid && next.String > at(now) {
		return true, nil
	}
	// A terminal update can commit just before the outcome fold. It must not
	// open a retry window between those transactions.
	var state string
	var finished sql.NullString
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id,state,finished_at FROM bridge_jobs WHERE kind='podcast.fetch' AND library_id=? AND rel_path=? AND json_extract(params,'$.show_id')=? AND json_extract(params,'$.episode_id')=? ORDER BY id DESC LIMIT 1`, j.LibraryID, j.Path, p.ShowID, p.EpisodeID).Scan(&id, &state, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state != StateFailed {
		return false, nil
	}
	var folded bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM podcast_fetch_state WHERE library_id=? AND rel_path=? AND show_id=? AND last_job_id>=?)`, j.LibraryID, j.Path, p.ShowID, id).Scan(&folded); err != nil {
		return false, err
	}
	t, parseErr := time.Parse(time.RFC3339, finished.String)
	return !folded && (parseErr != nil || now.Before(t.Add(30*time.Minute))), nil
}

// FetchStatus is the public summary, deliberately excluding job internals.
// Every key is present, including null values.
type FetchStatus struct {
	State             string      `json:"state"`
	RequestID         *int64      `json:"request_id"`
	BytesReceived     int64       `json:"bytes_received"`
	BytesTotal        *int64      `json:"bytes_total"`
	RetryAfterSeconds int         `json:"retry_after_seconds"`
	Error             *FetchError `json:"error"`
}
type FetchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// fetchMessages is our own copy for every safe fetch error code.
var fetchMessages = map[string]string{
	"dns":                  "The episode host could not be resolved.",
	"network":              "The episode could not be reached.",
	"timeout":              "The episode did not respond in time.",
	"tls":                  "The episode's secure connection could not be verified.",
	"invalid_feed_url":     "The episode address is invalid.",
	"not_a_feed":           "The episode's feed could not be read.",
	"not_a_podcast":        "The feed has no supported podcast episodes.",
	"feed_secret_missing":  "The private feed secret is missing. Follow the feed again.",
	"write_failed":         "The episode could not be saved.",
	"not_podcast_library":  "This job needs a podcast library.",
	"identity_changed":     "The episode identity changed.",
	"gone":                 "The episode is no longer available.",
	"too_large":            "The episode exceeds the size limit.",
	"episode_not_found":    "Episode not found.",
	"enclosure_missing":    "The episode has no available enclosure.",
	"invalid_audio":        "The enclosure is not supported audio.",
	"probe_unavailable":    "The audio probe is unavailable.",
	"unsupported_encoding": "The enclosure uses an unsupported encoding.",
	"index_failed":         "The episode could not be indexed.",
	"module_off":           "The podcasts module is off.",
	"bridge_unavailable":   "No online Bridge can fetch this episode.",
	"library_not_writable": "The Bridge cannot write to this library.",
	"demo_account":         "Demo accounts cannot fetch episodes.",
	"blocked_address":      "The episode address is blocked.",
	"fetch_failed":         "The episode could not be fetched.",
	"bridge_lost":          "The Bridge stopped reporting.",
}

// FetchErrorFor accepts only known safe codes and supplies our own copy.
func FetchErrorFor(code string) *FetchError {
	message, ok := fetchMessages[code]
	if !ok && len(code) == 8 && code[:5] == "http_" {
		if status, err := strconv.Atoi(code[5:]); err == nil && code[5] >= '1' && code[5] <= '5' {
			message = "The episode returned HTTP " + strconv.Itoa(status) + "."
			ok = true
		}
	}
	if !ok {
		code = "fetch_failed"
		message = fetchMessages[code]
	}
	return &FetchError{Code: code, Message: message}
}

// ReadyFetchStatus is the summary of a present committed generation.
func ReadyFetchStatus(f *feed.Fetch) FetchStatus {
	return FetchStatus{State: "ready", BytesReceived: f.Bytes, BytesTotal: &f.Bytes}
}

// UnavailableFetchStatus overlays a known scheduling refusal on a summary.
func UnavailableFetchStatus(st FetchStatus, refusal error) FetchStatus {
	if refusal == nil || st.State == "ready" {
		return st
	}
	code := "bridge_unavailable"
	switch {
	case errors.Is(refusal, ErrFetchModuleOff):
		code = "module_off"
	case errors.Is(refusal, ErrLibraryNotWritable):
		code = "library_not_writable"
	case errors.Is(refusal, ErrFetchDemo):
		code = "demo_account"
	}
	st.State = "unavailable"
	st.RetryAfterSeconds = 2
	st.Error = FetchErrorFor(code)
	return st
}

// PodcastFetchStatuses reads a whole page in one index query. Ready indexed
// generations win over stale work, and summaries survive terminal-row pruning.
func (s *Service) PodcastFetchStatuses(ctx context.Context, lib int64, paths []string) (map[string]FetchStatus, error) {
	out := map[string]FetchStatus{}
	if len(paths) == 0 {
		return out, nil
	}
	h, err := s.LastHeartbeat(ctx)
	if err != nil {
		return nil, err
	}
	refusal := s.fetchRefusal(h, lib, false)
	raw, _ := json.Marshal(paths)
	rows, err := s.db.QueryContext(ctx, `SELECT b.rel_path,e.present,e.fetch_json,j.id,j.state,j.progress,j.result,f.error_code,f.failures
 FROM podcast_episodes e JOIN books b ON b.id=e.book_id
 JOIN podcast_shows s ON s.library_id=e.library_id AND s.show_path=e.show_path
 LEFT JOIN podcast_fetch_state f ON f.library_id=b.library_id AND f.rel_path=b.rel_path AND f.show_id=s.show_id
 LEFT JOIN bridge_jobs j ON j.id=(SELECT x.id FROM bridge_jobs x WHERE x.kind='podcast.fetch' AND x.library_id=b.library_id AND x.rel_path=b.rel_path AND json_extract(x.params,'$.show_id')=s.show_id AND json_extract(x.params,'$.episode_id')=e.episode_id ORDER BY x.id DESC LIMIT 1)
 WHERE b.library_id=? AND b.rel_path IN (SELECT value FROM json_each(?))`, lib, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var present bool
		var pin, state, progress, result, code sql.NullString
		var id, failures sql.NullInt64
		if err = rows.Scan(&path, &present, &pin, &id, &state, &progress, &result, &code, &failures); err != nil {
			return nil, err
		}
		st := FetchStatus{State: "absent"}
		var f *feed.Fetch
		_ = json.Unmarshal([]byte(pin.String), &f)
		switch {
		case present && feed.ValidFetch(f) && f.DeletedAt == "":
			st = ReadyFetchStatus(f)
		case state.String == StateQueued:
			st.State = "queued"
			st.RequestID = &id.Int64
			st.RetryAfterSeconds = 2
		case state.String == StateClaimed || state.String == StateRunning || state.String == StateNeedsAttention:
			st.State = "fetching"
			st.RequestID = &id.Int64
			st.RetryAfterSeconds = 2
			var p struct {
				Received int64  `json:"bytes_received"`
				Total    *int64 `json:"bytes_total"`
			}
			if json.Unmarshal([]byte(progress.String), &p) == nil {
				st.BytesReceived = max(p.Received, 0)
				if p.Total != nil && *p.Total >= 0 {
					st.BytesTotal = p.Total
					st.BytesReceived = min(st.BytesReceived, *p.Total)
				}
			}
		case state.String == StateFailed || failures.Int64 > 0:
			st.State = "failed"
			if state.String == StateFailed {
				st.RequestID = &id.Int64
			}
			c := code.String
			if state.String == StateFailed {
				var r struct {
					Code string `json:"code"`
				}
				_ = json.Unmarshal([]byte(result.String), &r)
				if r.Code != "" {
					c = r.Code
				}
			}
			st.Error = FetchErrorFor(c)
		}
		out[path] = UnavailableFetchStatus(st, refusal)
	}
	return out, rows.Err()
}

type fetchKey struct {
	lib  int64
	path string
}
type fetchWatch struct {
	ch   chan struct{}
	refs int
}

// WatchPodcast registers before the caller rechecks presence, closing the
// check/sleep race. Both distinct paths and registrations have fixed bounds.
func (s *Service) WatchPodcast(lib int64, path string) (<-chan struct{}, func()) {
	s.fetchWaitMu.Lock()
	defer s.fetchWaitMu.Unlock()
	key := fetchKey{lib, path}
	if s.fetchWait == nil {
		s.fetchWait = map[fetchKey]*fetchWatch{}
	}
	w := s.fetchWait[key]
	if w == nil {
		if len(s.fetchWait) >= 4096 {
			return nil, func() {}
		}
		w = &fetchWatch{ch: make(chan struct{})}
		s.fetchWait[key] = w
	}
	if w.refs >= 128 {
		return nil, func() {}
	}
	w.refs++
	return w.ch, func() {
		s.fetchWaitMu.Lock()
		defer s.fetchWaitMu.Unlock()
		w.refs--
		if w.refs == 0 && s.fetchWait[key] == w {
			delete(s.fetchWait, key)
		}
	}
}
func (s *Service) NotifyPodcast(lib int64, path string) {
	s.fetchWaitMu.Lock()
	defer s.fetchWaitMu.Unlock()
	key := fetchKey{lib, path}
	if w := s.fetchWait[key]; w != nil {
		close(w.ch)
		delete(s.fetchWait, key)
	}
}

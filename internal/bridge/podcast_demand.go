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
	ErrFetchLimit     = errors.New("too many open podcast fetches")
)

// IsFetchRefusal reports whether err is a known scheduling refusal, which a
// trigger answers with a fetch status rather than a fault.
func IsFetchRefusal(err error) bool {
	for _, refusal := range []error{ErrFetchCooldown, ErrKindUnavailable, ErrLibraryNotWritable, ErrFetchModuleOff, ErrFetchDemo, ErrFetchLimit} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}

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
	case s.moduleOff(ModulePodcasts):
		return ErrFetchModuleOff
	default:
		return s.heldFetchRefusal(h, lib)
	}
}

// heldFetchRefusal is what refuses a fetch the Bridge already holds: the
// Bridge's own state alone. It carries on with the module off, which holds
// only queued work (server#179).
func (s *Service) heldFetchRefusal(h *Heartbeat, lib int64) error {
	switch {
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
	var state, result string
	var finished sql.NullString
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id,state,finished_at,result FROM bridge_jobs WHERE kind='podcast.fetch' AND library_id=? AND rel_path=? AND `+ParamSQL("params", "$.show_id")+`=? AND `+ParamSQL("params", "$.episode_id")+`=? ORDER BY id DESC LIMIT 1`, j.LibraryID, j.Path, p.ShowID, p.EpisodeID).Scan(&id, &state, &finished, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A failure stored cancelled (reported after a cancel) cools as one.
	if !PodcastFailed(Job{State: state, Result: json.RawMessage(result)}) {
		return false, nil
	}
	var folded bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM podcast_fetch_state WHERE library_id=? AND rel_path=? AND show_id=? AND last_job_id>=?)`, j.LibraryID, j.Path, p.ShowID, id).Scan(&folded); err != nil {
		return false, err
	}
	t, parseErr := time.Parse(time.RFC3339, finished.String)
	return !folded && (parseErr != nil || now.Before(t.Add(30*time.Minute))), nil
}

// The literal kind and working states match migration 0094's partial index.
const openFetchCountSQL = `SELECT COUNT(*) FROM bridge_jobs WHERE kind='` + KindPodcastFetch + `'
    AND requested_by=? AND state IN ` + workingStatesSQL

// checkFetchLimit shares the insertion's writer transaction, after open-intent
// dedupe. The creator owns the quota even when another listener promotes it. A
// parked fetch (needs_attention: the Bridge waits on something no listener can
// change) holds none of it (server#179).
func checkFetchLimit(ctx context.Context, tx *sql.Tx, by int64) error {
	if by == 0 { // Scheduled work has no requester.
		return nil
	}
	var admin bool
	if err := tx.QueryRowContext(ctx, `SELECT role='admin' FROM users WHERE id=?`, by).Scan(&admin); err != nil {
		return err
	}
	if admin {
		return nil
	}
	return checkOpenRequests(ctx, tx, openFetchCountSQL, by, ErrFetchLimit)
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

// fetchMessages is our own copy for every safe fetch error code. The console's
// job list words a failed fetch with the same sentences (bridge.podcastCode.<code>
// in admin-ui's en.json, PODCAST_FETCH_CODES): change both together
// (TestFetchMessagesMatchConsole).
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
	"fetch_limit":          "Too many episodes are being fetched. Try again shortly.",
	"blocked_address":      "The episode address is blocked.",
	"fetch_failed":         "The episode could not be fetched.",
	"bridge_lost":          "The Bridge stopped reporting.",
	"redirect_loop":        "The episode address redirects too many times.", // the Bridge's redirect loop (server#403)
	"http_310":             "The episode address redirects too many times.", // an older Bridge's redirect loop, not a status
}

// HTTPStatus is the status of a podcast job's `http_NNN` failure code (1xx to
// 5xx), and whether code is one.
func HTTPStatus(code string) (int, bool) {
	if len(code) != 8 || code[:5] != "http_" || code[5] < '1' || code[5] > '5' {
		return 0, false
	}
	status, err := strconv.Atoi(code[5:])
	return status, err == nil
}

// FetchErrorFor accepts only known safe codes and supplies our own copy.
func FetchErrorFor(code string) *FetchError {
	message, ok := fetchMessages[code]
	if status, isHTTP := HTTPStatus(code); !ok && isHTTP {
		message = "The episode returned HTTP " + strconv.Itoa(status) + "."
		ok = true
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
	case errors.Is(refusal, ErrFetchLimit):
		code = "fetch_limit"
	}
	st.State = "unavailable"
	st.RetryAfterSeconds = 2
	st.Error = FetchErrorFor(code)
	return st
}

// PodcastFetchStatuses reads a whole page in one index query. Ready indexed
// generations win over stale work, and summaries survive terminal-row pruning.
func (s *Service) PodcastFetchStatuses(ctx context.Context, lib int64, paths []string) (map[string]FetchStatus, error) {
	return s.podcastFetchStatuses(ctx, lib, paths, true)
}

// UnreadyFetchStatus is one episode's summary once its committed file failed
// to open (library.OpenPodcast): the indexed pin is not ready, so its open
// work, failure or scheduling refusal shows instead; absent when not indexed.
func (s *Service) UnreadyFetchStatus(ctx context.Context, lib int64, path string) (FetchStatus, error) {
	states, err := s.podcastFetchStatuses(ctx, lib, []string{path}, false)
	if err != nil {
		return FetchStatus{}, err
	}
	st, ok := states[path]
	if !ok {
		st = FetchStatus{State: "absent"}
	}
	return st, nil
}

func (s *Service) podcastFetchStatuses(ctx context.Context, lib int64, paths []string, pinReady bool) (map[string]FetchStatus, error) {
	out := map[string]FetchStatus{}
	if len(paths) == 0 {
		return out, nil
	}
	h, err := s.LastHeartbeat(ctx)
	if err != nil {
		return nil, err
	}
	refusal := s.fetchRefusal(h, lib, false)
	// A fetch the Bridge holds carries on with the module off (only a queued
	// one is held): it stays fetching unless the Bridge itself left or can't
	// write (server#179).
	held := refusal
	if errors.Is(refusal, ErrFetchModuleOff) {
		held = s.heldFetchRefusal(h, lib)
	}
	raw, _ := json.Marshal(paths)
	rows, err := s.db.QueryContext(ctx, `SELECT b.rel_path,e.present,e.fetch_json,j.id,j.state,j.progress,j.result,f.error_code,f.failures
 FROM podcast_episodes e JOIN books b ON b.id=e.book_id
 JOIN podcast_shows s ON s.library_id=e.library_id AND s.show_path=e.show_path
 LEFT JOIN podcast_fetch_state f ON f.library_id=b.library_id AND f.rel_path=b.rel_path AND f.show_id=s.show_id
 LEFT JOIN bridge_jobs j ON j.id=(SELECT x.id FROM bridge_jobs x WHERE x.kind='podcast.fetch' AND x.library_id=b.library_id AND x.rel_path=b.rel_path AND `+ParamSQL("x.params", "$.show_id")+`=s.show_id AND `+ParamSQL("x.params", "$.episode_id")+`=e.episode_id ORDER BY x.id DESC LIMIT 1)
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
		case pinReady && present && feed.ValidFetch(f) && f.DeletedAt == "":
			st = ReadyFetchStatus(f)
		case IsOpenState(state.String):
			st = OpenFetchStatus(Job{ID: id.Int64, State: state.String, Progress: json.RawMessage(progress.String)})
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
		if st.State == "fetching" {
			out[path] = UnavailableFetchStatus(st, held)
		} else {
			out[path] = UnavailableFetchStatus(st, refusal)
		}
	}
	return out, rows.Err()
}

// OpenFetchStatus is the summary of j, an open podcast.fetch: queued until a
// claim hands it out, then fetching (claimed, running or parked) with the
// bytes its progress reports.
func OpenFetchStatus(j Job) FetchStatus {
	st := FetchStatus{State: "queued", RequestID: &j.ID, RetryAfterSeconds: 2}
	if j.State == StateQueued {
		return st
	}
	st.State = "fetching"
	var p struct {
		Received int64  `json:"bytes_received"`
		Total    *int64 `json:"bytes_total"`
	}
	if json.Unmarshal(j.Progress, &p) == nil {
		st.BytesReceived = max(p.Received, 0)
		if p.Total != nil && *p.Total >= 0 {
			st.BytesTotal = p.Total
			st.BytesReceived = min(st.BytesReceived, *p.Total)
		}
	}
	return st
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

package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

// OnlineWindow is how recent the last heartbeat must be for the Bridge to count
// as online: the bridge capability, and the console's status. The Bridge beats
// every 30 s.
const OnlineWindow = 2 * time.Minute

// Status is what the Bridge last reported (PUT /bridge/status), as stored.
type Status struct {
	// Version is the Bridge's version.
	Version string `json:"version"`
	// Kinds are the job kinds it implements (KindVerify, ...). The server queues
	// only kinds that are both here and known to it (Kinds).
	Kinds []string `json:"kinds"`
	// Libraries is the Bridge's mapping of each server library to its own mount,
	// checked on every heartbeat.
	Libraries []LibraryStatus `json:"libraries"`
	// Workers are the ASR workers connected to it (Phase 1b).
	Workers []Worker `json:"workers"`
	// UIURL is the Bridge's own LAN page, linked from the console.
	UIURL string `json:"ui_url"`
	// Estimates are the Bridge's figures for costing a job before it is queued
	// (GET /admin/bridge/estimate); nil when it sent none.
	Estimates *Estimates `json:"estimates,omitempty"`
	// Setup is the Bridge's onboarding checklist (Phase 2b, setup.go); nil when
	// it sent none (an older Bridge, or one this server's heartbeat reply hasn't
	// told it may: HeartbeatAccepts).
	Setup *Setup `json:"setup,omitempty"`
}

// Estimates are the Bridge's costing figures, one block per job family. A key the
// server doesn't know is ignored (see UnmarshalJSON), so a newer Bridge can send
// more without its heartbeat being refused.
type Estimates struct {
	Sidecars *SidecarsEstimates `json:"sidecars,omitempty"`
}

// SidecarsEstimates are the Bridge's figures for sidecars.generate.
type SidecarsEstimates struct {
	// ASRSecondsPerAudioHour is the ASR wall time per hour of audio (its running
	// average, or a seed before the first book).
	ASRSecondsPerAudioHour float64 `json:"asr_seconds_per_audio_hour"`
	// AgentUSDPerAudioHour and AgentUSDPerEbook are the agent's spend per hour of
	// audio and per ebook book, from history; nil before any.
	AgentUSDPerAudioHour *float64 `json:"agent_usd_per_audio_hour"`
	AgentUSDPerEbook     *float64 `json:"agent_usd_per_ebook"`
	// Samples is how many finished books the figures come from.
	Samples int `json:"samples"`
	// AgentBackend is the agent CLI in use ("" when none); AgentReady that the
	// agent container answers and holds a credential; Subscription that the
	// credential is a subscription login (so the cost is notional).
	AgentBackend string `json:"agent_backend"`
	AgentReady   bool   `json:"agent_ready"`
	Subscription bool   `json:"subscription"`
	// CanContribute: the Bridge can offer a run's results to the community (it
	// holds what contributing needs), so a job may ask for contribute "issue".
	CanContribute bool `json:"can_contribute"`
	// MonthlyBudgetUSD is the monthly agent cap (0 = none); MonthSpentUSD what
	// this month has spent.
	MonthlyBudgetUSD float64 `json:"monthly_budget_usd"`
	MonthSpentUSD    float64 `json:"month_spent_usd"`
}

// UnmarshalJSON reads estimates leniently: the heartbeat's decoder refuses
// unknown fields, but the figures are the Bridge's to extend, so a key the server
// doesn't know is ignored here instead, and a block that doesn't decode (a figure
// of the wrong type) is dropped rather than refusing the heartbeat: estimates only
// cost a dialog, while a refused heartbeat takes the Bridge offline. Validate
// still bounds what it keeps.
func (e *Estimates) UnmarshalJSON(b []byte) error {
	type lenient Estimates
	var v lenient
	if err := json.Unmarshal(b, &v); err != nil {
		*e = Estimates{}
		return nil
	}
	*e = Estimates(v)
	return nil
}

// Bounds on the estimates: far above anything real, so a figure past one is a
// Bridge bug, and its block is dropped.
const (
	maxEstimateSeconds = 30 * 24 * 3600 // per audio hour
	maxEstimateUSD     = 1_000_000
	maxEstimateSamples = 1_000_000_000
)

// validate bounds the estimates: a block with a number below 0 or over its bound
// is dropped (the dialog then says there is no estimate), never refused, and the
// backend's name is cleaned to printable bounded text.
func (e *Estimates) validate() {
	if e == nil || e.Sidecars == nil {
		return
	}
	sc := e.Sidecars
	ok := inRange(sc.ASRSecondsPerAudioHour, maxEstimateSeconds) && sc.Samples >= 0 && sc.Samples <= maxEstimateSamples
	for _, v := range []*float64{sc.AgentUSDPerAudioHour, sc.AgentUSDPerEbook, &sc.MonthlyBudgetUSD, &sc.MonthSpentUSD} {
		ok = ok && (v == nil || inRange(*v, maxEstimateUSD))
	}
	if !ok {
		e.Sidecars = nil
		return
	}
	sc.AgentBackend = cleanText(sc.AgentBackend, maxShortText, false)
}

// inRange reports whether v is a number from 0 to max (JSON carries no NaN or
// infinity, but a figure is checked as one anyway).
func inRange(v, max float64) bool { return v >= 0 && v <= max && !math.IsNaN(v) }

// LibraryStatus is one library's mapping as the Bridge checked it.
type LibraryStatus struct {
	LibraryID int64 `json:"library_id"`
	// MountPath is where the Bridge sees the library's root.
	MountPath string `json:"mount_path"`
	// Mapped reports that a known indexed book was found at MountPath with the
	// size the server recorded; Writable that a probe file could be written and
	// removed there. Writing jobs are refused for a library that isn't both.
	Mapped   bool `json:"mapped"`
	Writable bool `json:"writable"`
	// CheckedPath is the book the check used; Error why it failed ("" when it
	// didn't).
	CheckedPath string `json:"checked_path"`
	Error       string `json:"error"`
}

// Worker is one ASR worker the Bridge reports.
type Worker struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	LastSeen string `json:"last_seen"`
}

// Heartbeat is the stored status with when it arrived.
type Heartbeat struct {
	Status
	At time.Time
}

// Online reports whether the heartbeat is recent enough (OnlineWindow) at now.
func (h *Heartbeat) Online(now time.Time) bool {
	return h != nil && !h.At.IsZero() && now.Sub(h.At) < OnlineWindow && h.At.Sub(now) < OnlineWindow
}

// FeatureContribute is the feature of a Bridge that can file contributions to
// the community metadata database now (it holds a GitHub sign-in:
// CanContribute). The contributions module needs it.
const FeatureContribute = "contribute"

// Features are what the Bridge offers that the server knows: the job families
// (the part of each known kind before its dot, "bridge.verify" is "bridge"),
// plus FeatureContribute while CanContribute; sorted, once each, and never nil
// (the wire's features is a list, empty for none). The bridge capability
// carries them, and a module's RequiresBridge names one.
func (st Status) Features() []string {
	out := []string{}
	for _, k := range st.Kinds {
		if _, ok := kinds[k]; !ok {
			continue
		}
		family, _, _ := strings.Cut(k, ".")
		if !slices.Contains(out, family) {
			out = append(out, family)
		}
	}
	if st.CanContribute() && !slices.Contains(out, FeatureContribute) {
		out = append(out, FeatureContribute)
	}
	slices.Sort(out)
	return out
}

// CanContribute reports whether the Bridge last said it can file contributions
// to the community metadata database (it holds a GitHub sign-in): its setup
// block's can_contribute when it sent one (Phase 2b), else its
// estimates.sidecars.can_contribute (Phase 2a; a Bridge sends both, so an
// older server still reads the second); false when it sent neither.
func (st Status) CanContribute() bool {
	if st.Setup != nil {
		return st.Setup.CanContribute
	}
	return st.Estimates != nil && st.Estimates.Sidecars != nil && st.Estimates.Sidecars.CanContribute
}

// Offers reports whether the Bridge implements kind.
func (st Status) Offers(kind string) bool { return slices.Contains(st.Kinds, kind) }

// Library returns the Bridge's mapping of a library, or nil when it sent none.
func (st Status) Library(id int64) *LibraryStatus {
	for i := range st.Libraries {
		if st.Libraries[i].LibraryID == id {
			return &st.Libraries[i]
		}
	}
	return nil
}

// Limits on a heartbeat: its body is the Bridge's, so the server bounds what it
// stores and later shows in the console.
const (
	MaxStatusBytes = 64 << 10
	maxKinds       = 64
	maxLibraries   = 512
	maxWorkers     = 64
	maxShortText   = 128
	maxLongText    = 1024
)

// kindPattern is a job kind: family.name, lower case.
var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}\.[a-z][a-z0-9_]{0,31}$`)

// ErrInvalid wraps every rejection of what the Bridge or an admin sent (a 400).
var ErrInvalid = errors.New("invalid")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate checks a heartbeat before it is stored: kinds are well formed and
// listed once, libraries listed once by a positive id, and ui_url, which the
// console renders as a link, is an absolute http(s) URL or empty (anything else
// is refused). An estimate out of its bounds is dropped, and so is a setup
// block that doesn't decode (a setup item it can't keep is left out). Its free text (the version,
// each mapping's paths and error, the workers, the estimates' backend) is
// cleaned to printable bounded text instead (cleanText).
func (st *Status) Validate() error {
	st.Version = cleanText(st.Version, maxShortText, false)
	if len(st.Kinds) > maxKinds {
		return invalid("too many kinds")
	}
	for i, k := range st.Kinds {
		if !kindPattern.MatchString(k) || slices.Contains(st.Kinds[:i], k) {
			return invalid("kind %q", k)
		}
	}
	if len(st.Libraries) > maxLibraries {
		return invalid("too many libraries")
	}
	for i := range st.Libraries {
		l := &st.Libraries[i]
		if l.LibraryID <= 0 || slices.ContainsFunc(st.Libraries[:i], func(o LibraryStatus) bool { return o.LibraryID == l.LibraryID }) {
			return invalid("library %d", l.LibraryID)
		}
		l.MountPath = cleanText(l.MountPath, maxLongText, false)
		l.CheckedPath = cleanText(l.CheckedPath, maxLongText, false)
		l.Error = cleanText(l.Error, maxLongText, false)
	}
	if len(st.Workers) > maxWorkers {
		return invalid("too many workers")
	}
	for i := range st.Workers {
		w := &st.Workers[i]
		w.Name, w.Version = cleanText(w.Name, maxShortText, false), cleanText(w.Version, maxShortText, false)
		w.Platform, w.LastSeen = cleanText(w.Platform, maxShortText, false), cleanText(w.LastSeen, maxShortText, false)
	}
	if st.UIURL != "" {
		if err := checkText("ui_url", st.UIURL, maxLongText); err != nil {
			return err
		}
		u, err := url.Parse(st.UIURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return invalid("ui_url must be an http(s) URL")
		}
	}
	st.Estimates.validate()
	st.Setup = st.Setup.validate()
	if st.Kinds == nil {
		st.Kinds = []string{}
	}
	if st.Libraries == nil {
		st.Libraries = []LibraryStatus{}
	}
	if st.Workers == nil {
		st.Workers = []Worker{}
	}
	return nil
}

// checkText bounds one text field: valid UTF-8, at most max bytes, no control
// characters.
func checkText(name, v string, max int) error {
	if len(v) > max || !utf8.ValidString(v) || strings.ContainsFunc(v, isControl) {
		return invalid("%s", name)
	}
	return nil
}

// isControl is a control character (C0 or DEL), which free text never keeps.
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// cleanText is v as printable bounded text: valid UTF-8 (a broken sequence
// becomes U+FFFD), each control character a space (but a line break when lines
// keeps them), at most max bytes, cut at a character's boundary. What the Bridge
// reports is partly the outside world's (a tool's output, a file's name, an OS
// error), so its free text is cleaned rather than the whole report refused: a
// refused job report is retried for ever while its job is re-run, and a refused
// heartbeat leaves the Bridge offline.
func cleanText(v string, max int, lines bool) string {
	v = strings.Map(func(r rune) rune {
		if isControl(r) && !(lines && r == '\n') {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(v, "\uFFFD"))
	if len(v) <= max {
		return v
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return v[:cut]
}

// ErrNotPaired is a call from a Bridge that is no longer the paired one: its
// token was revoked (an unpair, or another Bridge paired) after the request was
// authenticated (a 401).
var ErrNotPaired = errors.New("no bridge is paired")

// SaveHeartbeat records st (already validated) as the Bridge's status now. token
// is the request's bridge token, which must still be the paired Bridge's: a
// heartbeat that was authenticated before a pairing or an unpair never lands on
// the pairing that replaced it.
func (s *Service) SaveHeartbeat(ctx context.Context, token int64, st Status) error {
	body, err := json.Marshal(st)
	if err != nil {
		return err
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE bridge_status SET heartbeat_at = ?, status = ?
		  WHERE id = 1 AND paired_at IS NOT NULL AND `+auth.BridgeTokenLiveSQL, at(now), string(body), token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotPaired
	}
	stored, _ := time.Parse(time.RFC3339, at(now)) // the stored precision
	s.remember(&Heartbeat{Status: st, At: stored})
	return nil
}

// heartbeatTTL is how long a read heartbeat is reused: well inside the 2-minute
// online window, and how late an unpair or a demoted admin shows on GET /server
// at most (Forget clears it at once for a pairing or an unpair made here).
const heartbeatTTL = 5 * time.Second

// remember caches h as the heartbeat now (a heartbeat just saved).
func (s *Service) remember(h *Heartbeat) {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	s.hbGen++
	s.hb, s.hbRead = h, s.now()
}

// keep caches h, read from the database while the cache was at generation gen,
// unless a heartbeat was saved or the cache dropped since: their answer is newer
// than this read's.
func (s *Service) keep(gen uint64, h *Heartbeat) {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	if s.hbGen == gen {
		s.hb, s.hbRead = h, s.now()
	}
}

// Forget drops the cached heartbeat (after a pairing or an unpair).
func (s *Service) Forget() {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	s.hbGen++
	s.hb, s.hbRead = nil, time.Time{}
}

// LastHeartbeat returns the paired Bridge's last heartbeat, or nil when no
// Bridge is paired (auth.BridgeLiveSQL: a live bridge token of an enabled
// admin) or none has beaten since it paired. The result is shared: never
// change it.
func (s *Service) LastHeartbeat(ctx context.Context) (*Heartbeat, error) {
	s.hbMu.Lock()
	if !s.hbRead.IsZero() && s.now().Sub(s.hbRead) < heartbeatTTL {
		h := s.hb
		s.hbMu.Unlock()
		return h, nil
	}
	gen := s.hbGen
	s.hbMu.Unlock()
	var (
		stamp sql.NullString
		body  string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT heartbeat_at, status FROM bridge_status
		  WHERE id = 1 AND paired_at IS NOT NULL AND `+auth.BridgeLiveSQL).Scan(&stamp, &body)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var h *Heartbeat
	if err == nil && stamp.Valid {
		// An unreadable stamp or body is no heartbeat.
		if t, perr := time.Parse(time.RFC3339, stamp.String); perr == nil {
			h = &Heartbeat{At: t}
			if json.Unmarshal([]byte(body), &h.Status) != nil {
				h = nil
			}
		}
	}
	s.keep(gen, h)
	return h, nil
}

// Online returns the last heartbeat when it is recent enough to count the Bridge
// as online, else nil.
func (s *Service) Online(ctx context.Context) (*Heartbeat, error) {
	h, err := s.LastHeartbeat(ctx)
	if err != nil || !h.Online(s.now()) {
		return nil, err
	}
	return h, nil
}

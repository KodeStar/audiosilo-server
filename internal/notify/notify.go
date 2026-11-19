// Package notify records what happened on the server that an admin may want to
// know (the console's bell) and sends it to the destinations they set up: a
// webhook, an ntfy topic or a Discord channel.
//
// What is sent is what the console shows: names, counts, a version, a library's
// name. Never a secret (no token, code, password or destination address), never an
// IP address. Deliveries happen in the background, a few at a time, each retried
// twice on a timeout, a refused connection or a 5xx/429, and never follow a
// redirect. A failure is recorded on the destination as a short reason; neither the
// destination's address nor what the far end answered is ever logged or shown.
package notify

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/oidc"
)

// Event kinds an admin can subscribe a destination to.
const (
	KindBookAdded          = "book_added"
	KindScanFailed         = "scan_failed"
	KindLibraryUnavailable = "library_unavailable"
	KindNewDevice          = "new_device"
	KindInviteRedeemed     = "invite_redeemed"
	KindUpdateAvailable    = "update_available"
	KindBackupFailed       = "backup_failed"
	KindBridgeJobFinished  = "bridge_job_finished"
	KindBridgeJobFailed    = "bridge_job_failed"
	KindBookReleased       = "book_released"
	// A podcast show's news (catalog.PodcastNotice, PodcastNotice).
	KindPodcastEpisodesNew   = "podcast_episodes_new"
	KindPodcastFeedFailing   = "podcast_feed_failing"
	KindPodcastFeedRecovered = "podcast_feed_recovered"
	// A single sign-on no account took, waiting for an admin (OIDCPending).
	KindOIDCPending = "oidc_pending"
)

// Kinds lists the event kinds, in the order the console shows them. A kind
// added later starts unticked on every existing destination.
var Kinds = []string{
	KindBookAdded, KindScanFailed, KindLibraryUnavailable, KindNewDevice,
	KindInviteRedeemed, KindUpdateAvailable, KindBackupFailed,
	KindBridgeJobFinished, KindBridgeJobFailed, KindBookReleased,
	KindPodcastEpisodesNew, KindPodcastFeedFailing, KindPodcastFeedRecovered,
	KindOIDCPending,
}

// kindTest is the "Send test" message's kind (sent, never recorded).
const kindTest = "test"

// Event is something that happened. Data are its facts (see each Emit caller).
type Event struct {
	Kind string
	Data map[string]any
	// Dedup, when set, announces the event once while it is in the feed (90
	// days): a second one with the same kind and key is dropped, even one
	// emitted at the same moment (Service.dedupMu). Once the first has left
	// the feed, the same key is announced again.
	Dedup string
}

// Server is what a message says about the server: its name, and the public
// address links point at ("" when none is set: messages then carry no links).
type Server struct {
	Name      string
	PublicURL string
}

// Delivery tuning.
const (
	sendTimeout = 10 * time.Second
	queueSize   = 256
	workers     = 4
	maxBody     = 64 << 10 // read at most this much of an answer (then drop it)
)

// retryDelays are the waits before the second and third attempt.
var retryDelays = []time.Duration{10 * time.Second, time.Minute}

// Service records events and delivers them. A nil *Service does nothing, so
// callers needn't check.
type Service struct {
	cat     *catalog.Catalog
	client  *http.Client
	log     *slog.Logger
	version string
	server  func() Server
	queue   chan delivery
	delays  []time.Duration
	now     func() time.Time // the clock (tests)

	mu sync.Mutex
	// oidcPending: the oidc_pending notifications of the last hour, when each
	// was raised and by a sign-in from which address (oidcPendingPerHour,
	// oidcPendingCeiling); oidcPendingCapLogged: when reaching a cap was last
	// logged.
	oidcPending          []oidcPendingTaken
	oidcPendingCapLogged time.Time

	// dedupMu: an emit with a dedup key holds it from the check to the record,
	// so two at once (two callbacks for one waiting sign-in) announce it once
	// (server#408). Dedup emits are rare; one lock for all keys is enough.
	dedupMu sync.Mutex
}

// oidcPendingTaken is one oidc_pending notification raised: when, and the
// address its sign-in came from.
type oidcPendingTaken struct {
	at   time.Time
	from string
}

type delivery struct {
	target  catalog.NotifyTarget
	event   catalog.ServerEvent
	attempt int // 0 for the first
}

// New returns a service; Run delivers. server reports the server's name and
// public address at the time of sending.
func New(cat *catalog.Catalog, version string, server func() Server, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cat: cat,
		client: &http.Client{
			Timeout: sendTimeout,
			// A destination is an address an admin typed; a redirect would send the
			// message (and a webhook's signature) somewhere they didn't.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log: log, version: version, server: server,
		queue:  make(chan delivery, queueSize),
		delays: retryDelays,
		now:    time.Now,
	}
}

// Run delivers queued messages until ctx ends.
func (s *Service) Run(ctx context.Context) {
	if s == nil {
		return
	}
	for range workers {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case d := <-s.queue:
					s.deliver(ctx, d)
				}
			}
		}()
	}
}

// Emit records an event in the feed and queues it for every enabled destination
// subscribed to its kind. It never blocks on sending; a full queue drops the
// delivery (logged).
func (s *Service) Emit(ctx context.Context, e Event) {
	s.emit(ctx, e, nil)
}

// emit is Emit with a gate: allow (when set) runs after the dedup check and
// before the event is recorded, and false drops it.
func (s *Service) emit(ctx context.Context, e Event, allow func() bool) {
	if s == nil {
		return
	}
	ctx = context.WithoutCancel(ctx) // the request that caused it may be ending
	ev, ok := s.recordEvent(ctx, e, allow)
	if !ok {
		return
	}
	s.Deliver(ctx, ev)
}

// Deliver queues an event already in the feed for every enabled destination
// subscribed to its kind (Emit's second half, for an event recorded in the
// transaction of the write that raised it: catalog.SetPodcastNotice). It never
// blocks on sending.
func (s *Service) Deliver(ctx context.Context, ev catalog.ServerEvent) {
	if s == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	targets, err := s.cat.ListNotifyTargets(ctx)
	if err != nil {
		s.log.Warn("loading notification destinations failed", "err", err)
		return
	}
	for _, t := range targets {
		if !t.Enabled || !slices.Contains(t.Events, ev.Kind) {
			continue
		}
		select {
		case s.queue <- delivery{target: t, event: ev}:
		default:
			s.log.Warn("notification queue full; dropped a delivery", "target", t.ID, "kind", ev.Kind)
		}
	}
}

// recordEvent adds e to the feed unless its dedup key was seen or allow refuses it.
// For a dedup key the check, the gate and the record are one step (dedupMu).
func (s *Service) recordEvent(ctx context.Context, e Event, allow func() bool) (catalog.ServerEvent, bool) {
	if e.Dedup != "" {
		s.dedupMu.Lock()
		defer s.dedupMu.Unlock()
		seen, err := s.cat.ServerEventSeen(ctx, e.Kind, e.Dedup)
		if err != nil || seen {
			return catalog.ServerEvent{}, false
		}
	}
	if allow != nil && !allow() {
		return catalog.ServerEvent{}, false
	}
	ev, err := s.cat.RecordServerEvent(ctx, e.Kind, e.Data, e.Dedup)
	if err != nil {
		s.log.Warn("recording an event failed", "kind", e.Kind, "err", err)
		return catalog.ServerEvent{}, false
	}
	return ev, true
}

// Test sends a test message to t now, once, and records the outcome on it. It
// returns the failure reason ("" when it arrived).
func (s *Service) Test(ctx context.Context, t catalog.NotifyTarget) string {
	if s == nil {
		return "failed"
	}
	ev := catalog.ServerEvent{Kind: kindTest, At: time.Now().UTC().Format(time.RFC3339), Data: map[string]any{}}
	reason := s.send(ctx, t, ev)
	s.record(ctx, t.ID, reason)
	return reason
}

// deliver sends one message and records the outcome, or queues a retry.
//
// A failure that may pass is queued again after its delay rather than waited out
// here, so a destination that doesn't answer holds a worker for one attempt at a
// time, never the others' deliveries.
func (s *Service) deliver(ctx context.Context, d delivery) {
	if d.attempt > 0 {
		// A retry goes where the destination is set to send now: not at all once it
		// is deleted, switched off or unsubscribed, and to its new address and secret
		// after a change (a leaked one is rotated, not used again).
		t, err := s.cat.GetNotifyTarget(context.WithoutCancel(ctx), d.target.ID)
		if err != nil || !t.Enabled || !slices.Contains(t.Events, d.event.Kind) {
			return
		}
		d.target = *t
	}
	reason := s.send(ctx, d.target, d.event)
	if reason != "" && retryable(reason) && d.attempt < len(s.delays) {
		next := d
		next.attempt++
		time.AfterFunc(s.delays[d.attempt], func() {
			if ctx.Err() != nil {
				return
			}
			select {
			case s.queue <- next:
			default:
				s.log.Warn("notification queue full; dropped a retry", "target", d.target.ID, "kind", d.event.Kind)
			}
		})
		return
	}
	if reason != "" {
		s.log.Info("notification not delivered", "target", d.target.ID, "kind", d.event.Kind, "reason", reason)
	}
	s.record(ctx, d.target.ID, reason)
}

func (s *Service) record(ctx context.Context, id int64, reason string) {
	if err := s.cat.RecordDelivery(context.WithoutCancel(ctx), id, reason == "", reason); err != nil {
		s.log.Warn("recording a delivery failed", "target", id, "err", err)
	}
}

// retryable reports whether a failure may pass: a timeout, no connection, the far
// end overloaded or failing.
func retryable(reason string) bool {
	switch {
	case reason == "timeout", reason == "unreachable", reason == "http_429":
		return true
	case strings.HasPrefix(reason, "http_5"):
		return true
	}
	return false
}

// send makes one attempt and returns why it failed ("" when it arrived): "timeout",
// "unreachable", "http_<status>" or "failed". Never the error's own text, which
// would carry the destination's address.
func (s *Service) send(ctx context.Context, t catalog.NotifyTarget, ev catalog.ServerEvent) string {
	req, err := s.request(ctx, t, ev)
	if err != nil {
		return "failed"
	}
	resp, err := s.client.Do(req)
	if err != nil {
		var ne net.Error
		switch {
		case errors.As(err, &ne) && ne.Timeout():
			return "timeout"
		case ctx.Err() != nil:
			return "failed"
		}
		return "unreachable"
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "http_" + strconv.Itoa(resp.StatusCode)
	}
	return ""
}

// request builds the HTTP request a destination of t's kind expects.
func (s *Service) request(ctx context.Context, t catalog.NotifyTarget, ev catalog.ServerEvent) (*http.Request, error) {
	srv := s.server()
	m := compose(ev, srv)
	var (
		target = t.URL
		body   []byte
		err    error
	)
	switch t.Kind {
	case TargetDiscord:
		body, err = json.Marshal(discordBody(m, srv))
	case TargetNtfy:
		target, body, err = ntfyBody(t.URL, m)
	default:
		body, err = json.Marshal(webhookBody(ev, m, srv))
	}
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "AudioSilo/"+s.version)
	switch t.Kind {
	case TargetWebhook:
		req.Header.Set("X-AudioSilo-Event", ev.Kind)
		if t.Secret != "" {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			req.Header.Set("X-AudioSilo-Timestamp", ts)
			req.Header.Set("X-AudioSilo-Signature", "sha256="+Sign(t.Secret, ts, body))
		}
	case TargetNtfy:
		if t.Secret != "" {
			req.Header.Set("Authorization", "Bearer "+t.Secret)
		}
	}
	return req, nil
}

// Sign is a webhook's signature: hex HMAC-SHA256, keyed by the secret, of the
// timestamp header, a dot and the body. A receiver recomputes it and rejects an
// old timestamp, so a captured message can't be replayed.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// message is an event put into words (English: the destinations are outside the
// console's language setting).
type message struct {
	Title, Text, Link string
	Tags              []string // ntfy tags (emoji shortcodes)
	Priority          int      // ntfy priority, 1-5
	Color             int      // Discord embed colour
}

// Colours for Discord embeds: info, warning, problem.
const (
	colorInfo = 0x3b5bdb
	colorWarn = 0xd97706
	colorBad  = 0xc42b3c
)

// compose words an event. Links point into the console at the public address.
func compose(ev catalog.ServerEvent, srv Server) message {
	str := func(k string) string { s, _ := ev.Data[k].(string); return s }
	num := func(k string) int { n, _ := ev.Data[k].(float64); return int(n) }
	// A private podcast show's data carry no names (PodcastNotice).
	privateShow := ev.Data["private"] == true
	feedTitle := func(state string) string {
		if privateShow {
			return "A private podcast feed is " + state
		}
		return "A podcast feed is " + state + ": " + str("show")
	}
	link := func(path string) string {
		if srv.PublicURL == "" {
			return ""
		}
		return strings.TrimRight(srv.PublicURL, "/") + "/admin" + path
	}
	m := message{Priority: 3, Color: colorInfo}
	switch ev.Kind {
	case KindBookAdded:
		n := num("count")
		m.Title = plural(n, "1 new book in %s", "%d new books in %s", str("library"))
		m.Text, m.Link, m.Tags = titleLines(ev, n), link("/library"), []string{"books"}
	case KindBookReleased:
		n := num("count")
		m.Title = plural(n, "1 new release in %s", "%d new releases in %s", "your series")
		m.Text, m.Link, m.Tags = titleLines(ev, n), link("/library/series"), []string{"new"}
	case KindScanFailed:
		m.Title = "Scanning " + str("library") + " failed"
		m.Text = "See the scan's log in Health > Jobs."
		m.Link, m.Tags, m.Priority, m.Color = link("/health/jobs"), []string{"warning"}, 4, colorBad
	case KindLibraryUnavailable:
		m.Title = str("library") + " is offline"
		m.Text = "Its folder can't be read, so the scan stopped. Nothing was removed: books and progress are kept until it is back."
		m.Link, m.Tags, m.Priority, m.Color = link("/library/libraries"), []string{"electric_plug"}, 4, colorWarn
	case KindNewDevice:
		m.Title = "New sign-in: " + str("user")
		m.Text = strings.Join(nonEmpty(str("device"), str("app")), " · ")
		m.Link, m.Tags = link("/people/devices"), []string{"iphone"}
	case KindInviteRedeemed:
		m.Title = str("user") + "'s invite was used"
		m.Text = "A device was set up from it."
		if d := str("device"); d != "" {
			m.Text = d + " was set up from it."
		}
		m.Link, m.Tags = link("/people/invites"), []string{"envelope"}
	case KindUpdateAvailable:
		m.Title = "AudioSilo " + str("version") + " is available"
		m.Text = cmp.Or(str("name"), "A new version of the server is out.")
		m.Link, m.Tags = str("url"), []string{"package"}
	case KindBackupFailed:
		m.Title = "A backup failed"
		m.Text = backupReason(str("error"), str("detail"))
		m.Link, m.Tags, m.Priority, m.Color = link("/server?topic=backups"), []string{"warning"}, 4, colorBad
	case KindBridgeJobFinished:
		m.Title = bridgeJobLabel(str("job_kind")) + " finished: " + bridgeJobBook(ev)
		m.Text = "The Bridge finished the job."
		if w := bridgeKindWords[str("job_kind")]; w.ready != "" {
			m.Title = w.ready + ": " + bridgeJobBook(ev)
			m.Text = w.readyText
		}
		// Found chapters stand in only when the book has no other source (one
		// an admin chose, or the community's, keeps them out of the player), so
		// they are promised to listeners only when they are the book's now.
		// Under an admin's edit (saved while the job ran) they are a fallback
		// only: the chapter source can't put them over it.
		if str("job_kind") == bridge.KindDetectChapters && str("chapters_source") != catalog.ChaptersFromGenerated {
			m.Text = "The chapters were saved, but the book uses other chapters. Choose its chapter source on the book page."
			if str("chapters_source") == catalog.ChaptersFromEdited {
				m.Text = "The chapters were saved, but the book uses the chapters edited on this server. Revert the edit on the book page to use them."
			}
		}
		// A read-along says how much of the ebook lined up with the narration.
		if c, ok := ev.Data["coverage"].(float64); ok && str("job_kind") == bridge.KindReadalong {
			m.Title += fmt.Sprintf(" (%d%% lined up)", int(math.Floor(c*100+1e-9)))
		}
		// A LibriVox book claims its recording only when it is matched to it.
		if str("job_kind") == bridge.KindLibrivoxFetch {
			if text := librivoxMatchText[str("librivox_match")]; text != "" {
				m.Text = text
			}
		}
		// A contribute job whose every dimension the community already had
		// filed nothing, so nothing went for review.
		if none, _ := ev.Data["shared_nothing"].(bool); none && str("job_kind") == bridge.KindContribute {
			m.Title = "Already in the community: " + bridgeJobBook(ev)
			m.Text = "The community already had what this server generated for the book, so nothing was sent."
		}
		m.Link, m.Tags = link("/server?topic=bridge"), []string{"white_check_mark"}
		// A LibriVox book left unmatched is a warning, as the bell shows it.
		if str("job_kind") == bridge.KindLibrivoxFetch && str("librivox_match") == catalog.LibrivoxUnmatched {
			m.Tags, m.Color = []string{"warning"}, colorWarn
		}
	case KindBridgeJobFailed:
		if str("state") == bridge.StateNeedsAttention {
			m.Title = "A Bridge job needs attention: " + bridgeJobBook(ev)
			m.Color = colorWarn
		} else {
			m.Title = bridgeJobLabel(str("job_kind")) + " failed: " + bridgeJobBook(ev)
			m.Color = colorBad
		}
		m.Text = bridgeParkReason(str("code"))
		m.Link, m.Tags, m.Priority = link("/server?topic=bridge"), []string{"warning"}, 4
	case KindPodcastEpisodesNew:
		n := num("count")
		if privateShow {
			m.Title = plural(n, "A private podcast has 1 new episode", "A private podcast has %d new episodes")
		} else {
			m.Title = plural(n, "1 new episode of %s", "%d new episodes of %s", str("show"))
		}
		m.Text, m.Link, m.Tags = titleLines(ev, n), link("/library/libraries"), []string{"studio_microphone"}
	case KindPodcastFeedFailing:
		m.Title = feedTitle("failing")
		m.Text = feedFailingText(str("code"), num("failures"))
		m.Link, m.Tags, m.Priority, m.Color = link("/library/libraries"), []string{"warning"}, 4, colorWarn
	case KindPodcastFeedRecovered:
		m.Title = feedTitle("back")
		m.Text = "The feed answered again, so new episodes arrive as before."
		m.Link, m.Tags = link("/library/libraries"), []string{"white_check_mark"}
	case KindOIDCPending:
		who := cmp.Or(str("person"), "Someone")
		m.Title = who + " is waiting to sign in with " + cmp.Or(str("provider"), "single sign-on")
		m.Text = cmp.Or(oidcPendingText[str("reason")], "No account took the sign-in.") +
			" Link it to an account, make one, or dismiss it in People."
		m.Link, m.Tags = link("/people"), []string{"hourglass"}
	case kindTest:
		m.Title = "Test from " + srv.Name
		m.Text = "Notifications from this AudioSilo server will arrive here."
		m.Tags = []string{"white_check_mark"}
	default:
		m.Title = ev.Kind
	}
	return m
}

// bridgeKindWords is how a message words each Bridge job kind it knows: its
// label in a title and, for a kind whose finished job leaves something for
// listeners or the community, what is ready (the title's start) and where it
// went.
var bridgeKindWords = map[string]struct{ label, ready, readyText string }{
	bridge.KindSidecars:       {"Characters and recaps", "Characters and recaps ready", "Listeners see them in the player now."},
	bridge.KindDescribe:       {"Description", "Description ready", "Listeners see it in the player's About when the book has no other description."},
	bridge.KindDetectChapters: {"Chapters", "Chapters found", "Listeners see them in the player now."},
	bridge.KindContribute: {"Sharing with the community", "Shared with the community",
		"What this server generated for the book went to the community for review. The book page links it."},
	bridge.KindCorrect: {"Corrections", "Corrections sent",
		"The book's corrections went to the community for review. The book page links them."},
	bridge.KindReadalong: {"Read-along", "Read-along made",
		"It is saved with the book. Open the book in a compatible AudioSilo player to follow the text with the narration."},
	bridge.KindLibrivoxFetch: {"Adding from LibriVox", "Added from LibriVox", "It is in the library now."},
	bridge.KindAudibleImport: {"Audible backup", "Backed up from Audible", "It is in the library now."},
	bridge.KindVerify:        {label: "Bridge check"},
}

// librivoxMatchText is a finished librivox.fetch's message by how its book is
// matched (the event's librivox_match); none known keeps the kind's own.
var librivoxMatchText = map[string]string{
	catalog.LibrivoxMatched: "It is in the library now, matched to its LibriVox recording.",
	catalog.LibrivoxKept:    "It is in the library now. It was there already and keeps the match it had.",
	catalog.LibrivoxUnmatched: "It is in the library now, but it couldn't be matched to its LibriVox recording. " +
		"Add it again from Library > LibriVox to match it.",
}

// bridgeJobLabel names a Bridge job's kind in a message title.
func bridgeJobLabel(kind string) string {
	return cmp.Or(bridgeKindWords[kind].label, "Bridge job")
}

// bridgeJobBook is the book a Bridge job event is about: its title, else its
// path, else its library's name.
func bridgeJobBook(ev catalog.ServerEvent) string {
	for _, k := range []string{"title", "path", "library"} {
		if s, _ := ev.Data[k].(string); s != "" {
			return s
		}
	}
	return "a book"
}

// bridgeParkReason words a parked job's park code (the Bridge's
// progress.park.code). A transient park (bridge's transientParks) is never
// announced (bridge.Job.Notable); share_unconfirmed is worded all the same, so
// a bell that ever shows it never reads as the GitHub fault. A sentence says
// the job carries on by itself only where the Bridge resumes it with no Retry
// whatever the fix (a replaced agent credential, a raised or reset budget, a
// GitHub sign-in, or GitHub answering a signed-in Bridge again). The agent,
// GitHub and share sentences are the console's (bridge.park.<code> in en.json,
// TestBridgeParkReasonsMatchConsole), as the bell shows them. The Bridge's own
// page says more.
func bridgeParkReason(code string) string {
	switch code {
	case "book_not_writable":
		return "The Bridge can't write into the book's folder."
	case "budget_exceeded":
		return "The job reached a spending limit set on the Bridge. Raise it there (one set in its config.yaml or environment needs a restart), or wait for the monthly limit to reset; the job then carries on by itself."
	case "agent_credential_refused":
		return "The agent's credential was refused. Replace it on the Bridge's Accounts tab, or in the agent container if it has one of its own, which it uses first; the job then carries on by itself."
	case "contrib_unavailable":
		return "The Bridge can't share with the community: it has no GitHub sign-in it can use. Sign in to GitHub on the Bridge's Accounts tab and the job carries on; if the Bridge is signed in already, it carries on by itself once GitHub can be reached."
	case "share_unconfirmed":
		return "The Bridge couldn't ask the server whether this book is still to be shared, so nothing has been filed. The job carries on by itself once it can."
	case "core_needed":
		return "The book needs a community record before it can be contributed."
	case "chapters_not_detected":
		return "The Bridge couldn't find the chapters in the narration. Add chapter markers to the audio file, or match the book to a recording with a chapter list."
	}
	return "See the job in Settings > Bridge."
}

// feedCodeText words a failed podcast poll's code (bridge.PodcastOutcome's,
// the Bridge's own): what went wrong with the feed, never its address.
var feedCodeText = map[string]string{ //nolint:gosec // G101: sentences keyed by failure codes, not credentials
	"gone":                "The feed's address says it is gone, so the Bridge stopped checking it. Follow the show again by its new address.",
	"feed_secret_missing": "The Bridge no longer has this private feed's address. Follow the show again by its address to restore it.",
	"dns":                 "The feed's host couldn't be found.",
	"network":             "The feed couldn't be reached.",
	"timeout":             "The feed didn't answer in time.",
	"tls":                 "The feed's secure connection couldn't be verified.",
	"not_a_feed":          "The feed's address no longer serves a podcast feed.",
	"not_a_podcast":       "The feed has no podcast episodes the Bridge can use.",
	"too_large":           "The feed is larger than the Bridge accepts.",
	"blocked_address":     "The feed's address is one the Bridge won't contact.",
	"invalid_feed_url":    "The feed's address isn't valid.",
	"redirect_loop":       "The feed's address redirects too many times.", // the Bridge's redirect loop (server#403)
	"http_310":            "The feed's address redirects too many times.", // an older Bridge's redirect loop, not a status
}

// feedFailingText is a failing feed's message: its code worded (an HTTP status
// as such) and, for the feed's own failures, how many checks in a row failed.
func feedFailingText(code string, failures int) string {
	text, ok := feedCodeText[code]
	if !ok {
		text = "The Bridge couldn't check the feed."
		if status, ok := bridge.HTTPStatus(code); ok {
			text = fmt.Sprintf("The feed answered HTTP %d.", status)
		}
	}
	if failures > 1 {
		text += fmt.Sprintf(" %d checks in a row failed.", failures)
	}
	return text
}

// backupReason is a failed backup's message: detail (the folder, the server's
// user, its owner and the fix) when the folder refused the server.
func backupReason(code, detail string) string {
	switch code {
	case "disk_full":
		return "The disk is full. Free some space; the next backup runs on schedule."
	case "permission_denied":
		if detail != "" {
			return upperFirst(detail)
		}
		return "The server can't write to its backups folder. Check the folder's owner and permissions: Server > Logs names the folder and the user the server runs as."
	}
	return "See Server > Logs for the cause."
}

// upperFirst is s with its first letter a capital, as a sentence starts.
func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}

// titleLines is an event's "titles" one a line, then "and N more" when count
// is more than it names.
func titleLines(ev catalog.ServerEvent, count int) string {
	titles, _ := ev.Data["titles"].([]any)
	var lines []string
	for _, t := range titles {
		if s, ok := t.(string); ok {
			lines = append(lines, s)
		}
	}
	if more := count - len(lines); more > 0 && len(lines) > 0 {
		lines = append(lines, fmt.Sprintf("and %d more", more))
	}
	return strings.Join(lines, "\n")
}

// plural formats one (with args) when n is 1, else many (with n, then args).
func plural(n int, one, many string, args ...any) string {
	if n == 1 {
		return fmt.Sprintf(one, args...)
	}
	return fmt.Sprintf(many, append([]any{n}, args...)...)
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// webhookBody is a webhook's JSON: the event as the feed has it, worded, with the
// server it came from.
func webhookBody(ev catalog.ServerEvent, m message, srv Server) map[string]any {
	return map[string]any{
		"event":   ev.Kind,
		"id":      ev.ID,
		"at":      ev.At,
		"server":  map[string]string{"name": srv.Name, "url": srv.PublicURL},
		"title":   m.Title,
		"message": m.Text,
		"link":    m.Link,
		"data":    outbound(ev.Data),
	}
}

// internalData are event facts the console's bell shows but no destination is
// sent as data: a failed scan's detail is the tool's or the OS's own message,
// which can name a folder on the server. (A failed backup's detail is the
// message's own text when its folder refused the server: backupReason.)
var internalData = []string{"detail"}

// outbound is an event's data without its internal facts.
func outbound(data map[string]any) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		if !slices.Contains(internalData, k) {
			out[k] = v
		}
	}
	return out
}

// ntfyBody publishes as JSON to the server's root, which takes any title (headers
// would need non-ASCII titles encoded).
func ntfyBody(raw string, m message) (string, []byte, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", nil, err
	}
	server, topic := ntfySplit(u)
	if topic == "" {
		return "", nil, errors.New("not an ntfy topic")
	}
	msg := map[string]any{"topic": topic, "title": m.Title, "message": cmp.Or(m.Text, m.Title),
		"priority": m.Priority, "tags": m.Tags}
	if m.Link != "" {
		msg["click"] = m.Link
	}
	b, err := json.Marshal(msg)
	return server, b, err
}

// discordBody is one embed. Mentions are switched off: a book title or a device
// name could say @everyone.
func discordBody(m message, srv Server) map[string]any {
	embed := map[string]any{
		"title":       truncate(m.Title, 256),
		"description": truncate(m.Text, 4000),
		"color":       m.Color,
		"footer":      map[string]string{"text": truncate(srv.Name, 2000)},
	}
	if m.Link != "" {
		embed["url"] = m.Link
	}
	return map[string]any{
		"username":         "AudioSilo",
		"embeds":           []any{embed},
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// Triggers: the events the rest of the server reports, worded as data.

// ScanFinished reports a finished scan job: new books (an ok or partial scan that
// added some), a failed scan, or a library gone offline (once, when the scan before
// this one wasn't stopped the same way).
func (s *Service) ScanFinished(ctx context.Context, r library.RunReport) {
	if s == nil {
		return
	}
	lib := r.Library.Name
	switch r.Status {
	case catalog.RunOK, catalog.RunPartial:
		// A podcast library's new episodes are told per show (PodcastNotice).
		if r.Counts.Added > 0 && r.Library.Kind != catalog.KindPodcasts {
			titles := r.AddedTitles
			if titles == nil {
				titles = []string{}
			}
			s.Emit(ctx, Event{Kind: KindBookAdded, Data: map[string]any{
				"library": lib, "library_id": r.Library.ID, "count": r.Counts.Added, "titles": titles,
			}})
		}
	case catalog.RunFailed:
		s.Emit(ctx, Event{Kind: KindScanFailed, Data: map[string]any{
			"library": lib, "library_id": r.Library.ID, "run_id": r.RunID, "detail": r.Detail,
		}})
	case catalog.RunUnavailable:
		prev, err := s.cat.PreviousRunStatus(ctx, r.Library.ID, r.RunID)
		if err != nil || prev == catalog.RunUnavailable {
			return
		}
		s.Emit(ctx, Event{Kind: KindLibraryUnavailable, Data: map[string]any{
			"library": lib, "library_id": r.Library.ID,
		}})
	}
}

// BooksReleased reports new releases in the series the server holds that it
// doesn't hold (internal/upcoming): one event per run, naming the first few as
// "Title (Series #position)". dedup is the run's work ids, so a run repeated
// over the same releases says it once.
func (s *Service) BooksReleased(ctx context.Context, count int, titles []string, dedup string) {
	if s == nil || count == 0 {
		return
	}
	if titles == nil {
		titles = []string{}
	}
	s.Emit(ctx, Event{Kind: KindBookReleased, Data: map[string]any{"count": count, "titles": titles}, Dedup: dedup})
}

// SetDigest is a dedup key's digest of a set of ids, the same whatever their
// order (book_released's work ids, podcast_episodes_new's episode ids).
func SetDigest(ids []string) string {
	sum := sha256.Sum256([]byte(strings.Join(slices.Sorted(slices.Values(ids)), "\n")))
	return hex.EncodeToString(sum[:])
}

// PodcastEvent is the feed event a podcast show's news is recorded as
// (catalog.PodcastListener.Event, inside the write that found it): new
// episodes (once per set of episodes: dedup is the show and their ids, so the
// scheduled scan and the Bridge's rescan can't both announce them), a feed
// failing or back. The data name the library and the show, with counts and
// the failed poll's code: never the feed's address, host or the error's text.
// A private show is hidden from admins who don't follow it, so its notice
// says `private` instead of its title, folder and episode titles.
func PodcastEvent(n catalog.PodcastNotice) (kind string, data map[string]any, dedup string) {
	data = map[string]any{"library": n.Library, "library_id": n.LibraryID, "show_id": n.ShowID}
	if n.Private {
		data["private"] = true
	} else {
		// A feed without a title is named by its folder.
		data["show"], data["show_path"] = cmp.Or(plainLine(n.Show, 200), plainLine(n.ShowPath, 200)), n.ShowPath
	}
	switch n.Kind {
	case catalog.PodcastEpisodesNew:
		if n.Private {
			n.Titles = nil
		}
		titles := make([]string, 0, len(n.Titles))
		for _, t := range n.Titles {
			titles = append(titles, plainLine(t, 200))
		}
		data["count"], data["titles"] = len(n.EpisodeIDs), titles
		return KindPodcastEpisodesNew, data, fmt.Sprintf("%d:%s:%s", n.LibraryID, n.ShowID, SetDigest(n.EpisodeIDs))
	case catalog.PodcastFeedFailing:
		data["code"], data["failures"] = feedCode(n.Code), n.Failures
		return KindPodcastFeedFailing, data, ""
	case catalog.PodcastFeedRecovered:
		return KindPodcastFeedRecovered, data, ""
	}
	return "", nil, ""
}

// PodcastListener is the catalog's listener for podcast news: the event
// recorded with the write, delivered once it commits.
func (s *Service) PodcastListener() *catalog.PodcastListener {
	return &catalog.PodcastListener{Event: PodcastEvent, Deliver: s.Deliver}
}

// feedCode is a failed poll's code as a message may carry it: one the messages
// word (feedCodeText) or an HTTP status, else "failed". The code is the
// Bridge's own, so only known values go out, never a token it may hold.
func feedCode(code string) string {
	if _, ok := feedCodeText[code]; ok {
		return code
	}
	if _, ok := bridge.HTTPStatus(code); ok {
		return code
	}
	return "failed"
}

// oidcPendingText words why a single sign-on waits (oidc.PendingReasons); the
// console words the same codes.
var oidcPendingText = map[string]string{
	oidc.ReasonUnverifiedEmail: "Their email matches an account, but the provider hasn't verified it.",
	oidc.ReasonNoMatch:         "The provider sent no verified email to match an account on.",
	oidc.ReasonSeveralMatches:  "Their email matches more than one account.",
	oidc.ReasonNotEligible:     "Their email matches only accounts that can't be linked to a single sign-on.",
	oidc.ReasonNotAllowed:      "They aren't in the group allowed to get an account.",
	oidc.ReasonAutoCreateOff:   "No account has their email, and accounts aren't made on their own.",
	oidc.ReasonAdminAccount:    "Their email matches an admin's account, which is never linked on its own.",
}

// oidcPendingPerHour bounds the oidc_pending notifications the sign-ins from
// one address raise in an hour: a provider anyone can sign up at could
// otherwise raise one per throwaway subject. Per address, so one noisy source
// can't use up the hour for everyone: a person waiting from elsewhere is still
// announced. oidcPendingCeiling bounds them all together (a flood from many
// addresses). Every waiting row is in the console's list all the same.
const (
	oidcPendingPerHour = 5
	oidcPendingCeiling = 30
)

// OIDCPending reports a single sign-on no account took (auth.OIDCSignIn's
// Pending), once per waiting row while its event is in the feed (pendingID:
// the row keeps its id across the person's attempts, so one they keep alive
// past the feed's 90 days is announced again; a row dismissed and recorded
// again is a new one), at
// most oidcPendingPerHour an hour for the sign-ins from one address (from,
// which only counts: it is never in the event) and oidcPendingCeiling in all
// (server#408): a row over either is not announced then (logged, at most once
// an hour), but may be at the person's next attempt.
// Its data are the provider's name, the person's name or username and the
// reason's code: never their email, subject, groups or any token. A name or
// username that holds an "@" may be their email (providers often make the
// email the username), so it is left out.
func (s *Service) OIDCPending(ctx context.Context, pendingID int64, from, provider, name, username, reason string) {
	if s == nil || pendingID <= 0 {
		return
	}
	data := map[string]any{"provider": plainLine(provider, 100)}
	for _, who := range []string{name, username} {
		// Checked whole, before the cut: an "@" past it would leave its
		// leading part (an address's local part) behind.
		if strings.Contains(who, "@") {
			continue
		}
		if who = plainLine(who, 100); who != "" {
			data["person"] = who
			break
		}
	}
	if _, ok := oidcPendingText[reason]; ok {
		data["reason"] = reason
	}
	dedup := "pending:" + strconv.FormatInt(pendingID, 10)
	s.emit(ctx, Event{Kind: KindOIDCPending, Data: data, Dedup: dedup}, func() bool { return s.takeOIDCPending(from) })
}

// takeOIDCPending takes one of the hour's oidc_pending notifications for a
// sign-in from the address from: false when oidcPendingPerHour were raised
// from it within the last hour, or oidcPendingCeiling from anywhere.
func (s *Service) takeOIDCPending(from string) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	kept, same := s.oidcPending[:0], 0
	for _, t := range s.oidcPending {
		if now.Sub(t.at) < time.Hour {
			kept = append(kept, t)
			if t.from == from {
				same++
			}
		}
	}
	s.oidcPending = kept
	if same >= oidcPendingPerHour || len(kept) >= oidcPendingCeiling {
		if now.Sub(s.oidcPendingCapLogged) >= time.Hour {
			s.oidcPendingCapLogged = now
			s.log.Warn("notify: over the hour's waiting sign-in notifications; the rest are in People",
				"address", from, "per_address", oidcPendingPerHour, "in_all", oidcPendingCeiling)
		}
		return false
	}
	s.oidcPending = append(s.oidcPending, oidcPendingTaken{at: now, from: from})
	return true
}

// SignedIn reports a new session: a password sign-in or a device set up by
// pairing. viaInvite is set when the pairing came from an invite, which is also
// reported as the invite being used.
func (s *Service) SignedIn(ctx context.Context, user, device, app string, viaInvite bool) {
	if s == nil {
		return
	}
	// The device name is whatever the signing-in client sent: one plain line.
	device = plainLine(device, 100)
	data := map[string]any{"user": user, "device": device, "app": app}
	s.Emit(ctx, Event{Kind: KindNewDevice, Data: data})
	if viaInvite {
		s.Emit(ctx, Event{Kind: KindInviteRedeemed, Data: map[string]any{"user": user, "device": device}})
	}
}

// UpdateFound reports a newer release, once per version.
func (s *Service) UpdateFound(ctx context.Context, version, name, url string) {
	if s == nil {
		return
	}
	s.Emit(ctx, Event{Kind: KindUpdateAvailable, Dedup: version, Data: map[string]any{
		"version": version, "name": name, "url": url,
	}})
}

// BackupFailed reports a backup that failed (trigger: scheduled or manual; reason
// is backup.Result's short code, detail its Detail: the folder, the server's user,
// the folder's owner and the fix when the folder refused the server, else "").
func (s *Service) BackupFailed(ctx context.Context, trigger, reason, detail string) {
	if s == nil {
		return
	}
	data := map[string]any{"trigger": trigger, "error": reason}
	if detail != "" {
		data["detail"] = detail
	}
	s.Emit(ctx, Event{Kind: KindBackupFailed, Data: data})
}

// BridgeJob is a Bridge job that just finished, failed or parked, for
// BridgeJobEnded: its kind and new state, the book (library, path, title), the
// park code (progress.park.code, "" for none; only a park's is sent, as a failed
// job's progress may still name the park it was in before) and the error.
type BridgeJob struct {
	ID        int64
	Kind      string
	State     string
	Library   string
	LibraryID int64
	Path      string
	Title     string
	Code      string
	Error     string
	// ChaptersSource is where the book's chapters come from now
	// (catalog.ChaptersFrom*; "" when not known), so a finished chapters.detect
	// job says whether listeners get the chapters it found.
	ChaptersSource string
	// SharedNothing: a finished sidecars.contribute job filed nothing, as the
	// community already had all it was asked to share
	// (bridge.Job.SharedNothing), so its message says so.
	SharedNothing bool
	// Coverage is a finished readalong.align job's text coverage (0 to 1,
	// bridge.Job.ReadalongCoverage), nil when not known, so its message says
	// how much lined up.
	Coverage *float64
	// LibrivoxMatch is how a finished librivox.fetch's book is matched now
	// (catalog.LibrivoxFetchMatch; "" when not known), so its message claims
	// the recording only when the book is matched to it.
	LibrivoxMatch string
}

// BridgeJobEnded reports a Bridge job that moved into done (bridge_job_finished)
// or into failed or needs_attention (bridge_job_failed). The caller reports each
// move once (bridge.Service.OnTransition). The error is the Bridge's own text (it can name a
// folder on its disk), so it is the bell's detail and is never sent out.
func (s *Service) BridgeJobEnded(ctx context.Context, j BridgeJob) {
	if s == nil {
		return
	}
	data := map[string]any{
		"job_id": j.ID, "job_kind": j.Kind, "state": j.State,
		"library": j.Library, "library_id": j.LibraryID, "path": j.Path, "title": plainLine(j.Title, 200),
	}
	kind := KindBridgeJobFinished
	if j.Kind == bridge.KindDetectChapters && j.State == bridge.StateDone && j.ChaptersSource != "" {
		data["chapters_source"] = j.ChaptersSource
	}
	if j.Kind == bridge.KindContribute && j.State == bridge.StateDone && j.SharedNothing {
		data["shared_nothing"] = true
	}
	if j.Kind == bridge.KindReadalong && j.State == bridge.StateDone && j.Coverage != nil {
		data["coverage"] = *j.Coverage
	}
	if j.Kind == bridge.KindLibrivoxFetch && j.State == bridge.StateDone && j.LibrivoxMatch != "" {
		data["librivox_match"] = j.LibrivoxMatch
	}
	if j.State != bridge.StateDone {
		kind = KindBridgeJobFailed
		if j.State == bridge.StateNeedsAttention {
			data["code"] = plainLine(j.Code, 64)
		}
		data["detail"] = j.Error
	}
	s.Emit(ctx, Event{Kind: kind, Data: data})
}

// plainLine is s without control characters (line breaks included), at most n
// characters.
func plainLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return truncate(strings.TrimSpace(s), n)
}

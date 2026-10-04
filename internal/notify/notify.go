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
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
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
)

// Kinds lists the event kinds, in the order the console shows them.
var Kinds = []string{
	KindBookAdded, KindScanFailed, KindLibraryUnavailable, KindNewDevice,
	KindInviteRedeemed, KindUpdateAvailable, KindBackupFailed,
}

// kindTest is the "Send test" message's kind (sent, never recorded).
const kindTest = "test"

// Event is something that happened. Data are its facts (see each Emit caller).
type Event struct {
	Kind string
	Data map[string]any
	// Dedup, when set, announces the event once: a second one with the same kind
	// and key (still in the feed) is dropped.
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
}

type delivery struct {
	target catalog.NotifyTarget
	event  catalog.ServerEvent
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
	if s == nil {
		return
	}
	ctx = context.WithoutCancel(ctx) // the request that caused it may be ending
	if e.Dedup != "" {
		seen, err := s.cat.ServerEventSeen(ctx, e.Kind, e.Dedup)
		if err != nil || seen {
			return
		}
	}
	ev, err := s.cat.RecordServerEvent(ctx, e.Kind, e.Data, e.Dedup)
	if err != nil {
		s.log.Warn("recording an event failed", "kind", e.Kind, "err", err)
		return
	}
	targets, err := s.cat.ListNotifyTargets(ctx)
	if err != nil {
		s.log.Warn("loading notification destinations failed", "err", err)
		return
	}
	for _, t := range targets {
		if !t.Enabled || !slices.Contains(t.Events, e.Kind) {
			continue
		}
		select {
		case s.queue <- delivery{target: t, event: ev}:
		default:
			s.log.Warn("notification queue full; dropped a delivery", "target", t.ID, "kind", e.Kind)
		}
	}
}

// Test sends a test message to t now, once, and records the outcome on it. It
// returns the failure reason ("" when it arrived).
func (s *Service) Test(ctx context.Context, t catalog.NotifyTarget) string {
	ev := catalog.ServerEvent{Kind: kindTest, At: time.Now().UTC().Format(time.RFC3339), Data: map[string]any{}}
	reason := s.send(ctx, t, ev)
	s.record(ctx, t.ID, reason)
	return reason
}

// deliver sends one message, retrying a passing failure, and records the outcome.
func (s *Service) deliver(ctx context.Context, d delivery) {
	var reason string
	for attempt := 0; ; attempt++ {
		reason = s.send(ctx, d.target, d.event)
		if reason == "" || !retryable(reason) || attempt >= len(s.delays) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.delays[attempt]):
		}
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
		titles, _ := ev.Data["titles"].([]any)
		var lines []string
		for _, t := range titles {
			if s, ok := t.(string); ok {
				lines = append(lines, s)
			}
		}
		if more := n - len(lines); more > 0 && len(lines) > 0 {
			lines = append(lines, fmt.Sprintf("and %d more", more))
		}
		m.Text, m.Link, m.Tags = strings.Join(lines, "\n"), link("/library"), []string{"books"}
	case KindScanFailed:
		m.Title = "Scanning " + str("library") + " failed"
		m.Text = firstNonEmpty(str("detail"), "See the scan's log in Health > Jobs.")
		m.Link, m.Tags, m.Priority, m.Color = link("/health/jobs"), []string{"warning"}, 4, colorBad
	case KindLibraryUnavailable:
		m.Title = str("library") + " is offline"
		m.Text = "Its folder can't be read, so the scan stopped. Nothing was removed: books and progress are kept until it is back."
		m.Link, m.Tags, m.Priority, m.Color = link("/library/libraries"), []string{"electric_plug"}, 4, colorWarn
	case KindNewDevice:
		m.Title = "New sign-in: " + str("user")
		m.Text = strings.TrimSpace(strings.Join(nonEmpty(str("device"), str("app")), " · "))
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
		m.Text = firstNonEmpty(str("name"), "A new version of the server is out.")
		m.Link, m.Tags = str("url"), []string{"package"}
	case KindBackupFailed:
		m.Title = "A backup failed"
		m.Text = backupReason(str("error"))
		m.Link, m.Tags, m.Priority, m.Color = link("/server?topic=backups"), []string{"warning"}, 4, colorBad
	case kindTest:
		m.Title = "Test from " + srv.Name
		m.Text = "Notifications from this AudioSilo server will arrive here."
		m.Tags = []string{"white_check_mark"}
	default:
		m.Title = ev.Kind
	}
	return m
}

func backupReason(code string) string {
	switch code {
	case "disk_full":
		return "The disk is full. Free some space; the next backup runs on schedule."
	case "permission_denied":
		return "The server can't write to its backups folder."
	}
	return "See Server > Logs for the cause."
}

func plural(n int, one, many, name string) string {
	if n == 1 {
		return fmt.Sprintf(one, name)
	}
	return fmt.Sprintf(many, n, name)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
		"data":    ev.Data,
	}
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
	msg := map[string]any{"topic": topic, "title": m.Title, "message": firstNonEmpty(m.Text, m.Title),
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
		if r.Counts.Added > 0 {
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
// is backup.Result's short code).
func (s *Service) BackupFailed(ctx context.Context, trigger, reason string) {
	if s == nil {
		return
	}
	s.Emit(ctx, Event{Kind: KindBackupFailed, Data: map[string]any{"trigger": trigger, "error": reason}})
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

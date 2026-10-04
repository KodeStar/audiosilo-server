package notify

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Destination kinds.
const (
	TargetWebhook = "webhook"
	TargetNtfy    = "ntfy"
	TargetDiscord = "discord"
)

// TargetKinds are the destinations the server can send to (email comes later).
var TargetKinds = []string{TargetWebhook, TargetNtfy, TargetDiscord}

// Limits on a destination's fields.
const (
	maxNameLen   = 64
	maxURLLen    = 2048
	maxSecretLen = 256
)

// FieldError is a refused destination field: which one and why, in words a form
// can show under it.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

func fieldErr(field, msg string) error { return &FieldError{Field: field, Err: errors.New(msg)} }

var (
	ntfyTopicRE = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)
	// Discord webhooks: /api/webhooks/<id>/<token> on Discord's own hosts.
	discordPathRE = regexp.MustCompile(`^/api(/v\d+)?/webhooks/\d+/[A-Za-z0-9_-]+/?$`)
	discordHosts  = []string{"discord.com", "discordapp.com", "ptb.discord.com", "canary.discord.com"}
)

// Clean checks and normalizes a destination's settings in place (kind, name, URL,
// secret, events); a refusal is a *FieldError.
func Clean(kind *string, name, rawURL, secret *string, events *[]string) error {
	*kind = strings.TrimSpace(*kind)
	if !slices.Contains(TargetKinds, *kind) {
		return fieldErr("kind", "must be webhook, ntfy or discord")
	}
	*name = strings.TrimSpace(*name)
	switch {
	case *name == "":
		return fieldErr("name", "give it a name")
	case utf8.RuneCountInString(*name) > maxNameLen:
		return fieldErr("name", fmt.Sprintf("a name can be at most %d characters", maxNameLen))
	case strings.IndexFunc(*name, unicode.IsControl) >= 0:
		return fieldErr("name", "a name can't contain line breaks or control characters")
	}
	*rawURL = strings.TrimSpace(*rawURL)
	if err := checkURL(*kind, *rawURL); err != nil {
		return err
	}
	if len(*secret) > maxSecretLen {
		return fieldErr("secret", fmt.Sprintf("at most %d characters", maxSecretLen))
	}
	if strings.IndexFunc(*secret, unicode.IsControl) >= 0 {
		return fieldErr("secret", "can't contain line breaks or control characters")
	}
	if *kind == TargetDiscord && *secret != "" {
		return fieldErr("secret", "a Discord webhook takes no secret: its address is the secret")
	}
	clean := []string{}
	for _, e := range *events {
		if !slices.Contains(Kinds, e) {
			return fieldErr("events", "unknown event "+e)
		}
		if !slices.Contains(clean, e) {
			clean = append(clean, e)
		}
	}
	*events = clean
	return nil
}

func checkURL(kind, raw string) error {
	if raw == "" {
		return fieldErr("url", "enter the address to send to")
	}
	if len(raw) > maxURLLen {
		return fieldErr("url", "the address is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Fragment != "" ||
		strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return fieldErr("url", "must be an address starting with https:// or http://")
	}
	switch kind {
	case TargetDiscord:
		if u.Scheme != "https" || !slices.Contains(discordHosts, strings.ToLower(u.Hostname())) ||
			!discordPathRE.MatchString(u.Path) || u.User != nil || u.Port() != "" {
			return fieldErr("url", "must be a Discord webhook address (https://discord.com/api/webhooks/...)")
		}
	case TargetNtfy:
		if _, topic := ntfySplit(u); topic == "" || u.RawQuery != "" || u.User != nil {
			return fieldErr("url", "must be the topic's address, like https://ntfy.sh/your-topic")
		}
	}
	return nil
}

// ntfySplit splits an ntfy topic address into the server's address and the topic
// (the last path segment); topic is "" when it isn't a topic.
func ntfySplit(u *url.URL) (server, topic string) {
	p := strings.TrimSuffix(u.EscapedPath(), "/")
	i := strings.LastIndexByte(p, '/')
	if i < 0 || !ntfyTopicRE.MatchString(p[i+1:]) {
		return "", ""
	}
	base := *u
	base.RawPath, base.Path, base.RawQuery = "", "", ""
	return base.String() + p[:i], p[i+1:]
}

// SameOrigin reports whether two addresses are on the same server (scheme, host
// and port, ignoring case), so a destination's secret can follow a path change but
// not a move to another server.
func SameOrigin(a, b string) bool {
	ua, err := url.Parse(strings.TrimSpace(a))
	if err != nil {
		return false
	}
	ub, err := url.Parse(strings.TrimSpace(b))
	if err != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Hostname(), ub.Hostname()) &&
		effectivePort(ua) == effectivePort(ub)
}

// effectivePort is a URL's port, its scheme's default when none is written (as a
// browser's URL.origin reads it, so the console's warning and this rule agree).
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// Redact is how the console shows a destination's address: the host and the start
// of the path, with the last path segment (a Discord token, an ntfy topic, a
// webhook's own secret part) cut to its first four characters (none of a segment
// that short: an ntfy topic is its own password) and the query and any user name
// dropped. Never enough to send with.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	p := strings.TrimSuffix(u.EscapedPath(), "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 && len(p) > i+1 {
		last := p[i+1:]
		if len(last) > 4 {
			last = last[:4] + "…"
		} else {
			last = "…"
		}
		p = p[:i+1] + last
	}
	out := u.Scheme + "://" + u.Host + p
	if u.RawQuery != "" {
		out += "?…"
	}
	return out
}

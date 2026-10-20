package feed

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strings"
	"unicode"
)

var ErrInvalidFeedURL = errors.New("invalid_feed_url")

// NormalizeFeedURL preserves credential, escaped path and query bytes.
func NormalizeFeedURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > 2048 {
		return "", ErrInvalidFeedURL
	}
	u, err := url.Parse(s)
	if err != nil || u.Opaque != "" || u.Hostname() == "" {
		return "", ErrInvalidFeedURL
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", ErrInvalidFeedURL
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || strings.ContainsFunc(host, unicode.IsSpace) {
		return "", ErrInvalidFeedURL
	}
	port := u.Port()
	if port == "80" && u.Scheme == "http" || port == "443" && u.Scheme == "https" {
		port = ""
	}
	u.Host = host
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	u.Fragment, u.RawFragment = "", ""
	// net/url re-encodes userinfo. Preserve the original authority bytes instead.
	rawUser := ""
	if u.User != nil {
		authority := strings.SplitN(s, "://", 2)[1]
		if i := strings.IndexAny(authority, "/?#"); i >= 0 {
			authority = authority[:i]
		}
		rawUser = authority[:strings.LastIndex(authority, "@")+1]
		u.User = nil
	}
	if u.Path == "" {
		u.Path = "/"
	}
	norm := u.String()
	if rawUser != "" {
		prefix := u.Scheme + "://"
		norm = prefix + rawUser + strings.TrimPrefix(norm, prefix)
	}
	if len(norm) > 2048 {
		return "", ErrInvalidFeedURL
	}
	return norm, nil
}

func FeedURLHash(norm string) string {
	h := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(h[:])
}

func RedactURL(norm string) string {
	u, err := url.Parse(norm)
	if err != nil || u.Host == "" {
		return "[redacted URL]"
	}
	out := u.Scheme + "://" + u.Host
	if u.Path != "/" && u.Path != "" || u.RawQuery != "" || u.ForceQuery {
		out += "/..."
	}
	return out
}

func IsPrivateFeedURL(norm string) bool {
	u, err := url.Parse(norm)
	if err != nil {
		return true
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery {
		return true
	}
	host := strings.ToLower(u.Hostname())
	for _, domain := range []string{"patreon.com", "supercast.com", "supercast.tech", "memberfulcontent.com", "supportingcast.fm", "glow.fm", "steadyhq.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	if strings.HasSuffix(host, "substack.com") && strings.Contains(u.Path, "/private/") {
		return true
	}
	for _, segment := range strings.Split(u.Path, "/") {
		letter, digit, token := false, false, len(segment) >= 24
		for _, r := range segment {
			switch {
			case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
				letter = true
			case r >= '0' && r <= '9':
				digit = true
			case r != '_' && r != '-':
				token = false
			}
		}
		if token && letter && digit {
			return true
		}
	}
	return false
}

// Package oidc is Sign in with OpenID Connect's own logic (server#235): the
// provider (discovery with backoff, the authorization URL, the code exchange and
// the ID token's checks), the sealed flow cookie, the claims a sign-in carries
// and Decide, which says what a sign-in does with an account. It holds no
// storage (auth/identities.go) and no HTTP routes (api/handlers_oidc.go); every
// part but the provider is pure.
package oidc

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Claims are what a sign-in says of the person, from the ID token and, for what
// it lacks, the userinfo endpoint. Every string is trimmed and capped (clean).
type Claims struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Username      string
	Name          string
	Groups        []string
}

// Caps on what a sign-in carries into the database.
const (
	maxClaim      = 256
	maxGroups     = 100
	maxGroupClaim = 256
)

// MaxEmail bounds an email, in bytes (auth.MaxEmail, an account's).
const MaxEmail = 254

// claimSet is a decoded claims object (the ID token's or userinfo's).
type claimSet map[string]json.RawMessage

// str is the claim as a string ("" when absent or not a string).
func (c claimSet) str(name string) string {
	var s string
	if raw, ok := c[name]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// has reports whether the claim is present with a non-null value.
func (c claimSet) has(name string) bool {
	raw, ok := c[name]
	return ok && string(raw) != "null"
}

// verified reads email_verified: true, or the string "true" some providers send.
func (c claimSet) verified() bool {
	raw, ok := c["email_verified"]
	if !ok {
		return false
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && strings.EqualFold(strings.TrimSpace(s), "true")
}

// groups reads a groups claim: a list of strings, or one string (some providers
// send a single group bare). Other members of the list are skipped.
func (c claimSet) groups(name string) []string {
	raw, ok := c[name]
	if !ok {
		return nil
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		var one string
		if json.Unmarshal(raw, &one) == nil {
			return cleanGroups([]string{one})
		}
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		var s string
		if json.Unmarshal(item, &s) == nil {
			out = append(out, s)
		}
	}
	return cleanGroups(out)
}

// cleanGroups trims, drops empty and repeated names, caps each and the list.
func cleanGroups(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, g := range in {
		g = clean(g, maxGroupClaim)
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
		if len(out) == maxGroups {
			break
		}
	}
	return out
}

// clean trims s, drops control characters and cuts it to max bytes on a rune
// boundary.
func clean(s string, limit int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return strings.TrimSpace(s)
}

// fill sets what c lacks from set: the email with its verified flag (taken
// together, so one source's address is never vouched for by the other's
// flag), the username, the name and the groups. An email clean changed (cut
// to MaxEmail, trimmed, a control character dropped) is not the address the
// provider verified, so it counts as unverified: it never links by itself.
func (c *Claims) fill(set claimSet, usernameClaim, groupsClaim string) {
	if c.Email == "" {
		raw := set.str("email")
		if e := clean(raw, MaxEmail); e != "" {
			c.Email, c.EmailVerified = e, e == raw && set.verified()
		}
	}
	if c.Username == "" {
		c.Username = clean(set.str(usernameClaim), maxClaim)
	}
	if c.Name == "" {
		c.Name = clean(set.str("name"), maxClaim)
	}
	if c.Groups == nil && set.has(groupsClaim) {
		c.Groups = set.groups(groupsClaim)
	}
}

// complete reports whether c has everything userinfo could add.
func (c *Claims) complete() bool {
	return c.Email != "" && c.Username != "" && c.Groups != nil
}

// InGroup reports whether the person is in group (exact, case counts).
func (c Claims) InGroup(group string) bool {
	for _, g := range c.Groups {
		if g == group {
			return true
		}
	}
	return false
}

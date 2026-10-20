package auth

import (
	"strconv"
	"strings"
)

// PodcastPlayerMinVersion stays empty until a released player supports podcasts.
var PodcastPlayerMinVersion = ""

// SeesPodcasts uses the authenticated request's client, including a media token's
// stored client when no valid header was sent. An admin role grants no exception.
func SeesPodcasts(c ClientInfo) bool {
	if c.App == "AudioSilo Admin" {
		return true
	}
	if c.App != "AudioSilo" {
		return false
	}
	if c.Version == "dev" {
		return true
	}
	if PodcastPlayerMinVersion == "" {
		return false
	}
	got, ok := podcastVersion(c.Version)
	if !ok {
		return false
	}
	want, ok := podcastVersion(PodcastPlayerMinVersion)
	if !ok {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}
func podcastVersion(s string) ([3]uint64, bool) {
	var out [3]uint64
	s, _, _ = strings.Cut(s, "-")
	s, _, _ = strings.Cut(s, "+")
	parts := strings.Split(s, ".")
	if len(parts) < 3 {
		return out, false
	}
	for i, p := range parts[:3] {
		if p == "" {
			return out, false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return out, false
			}
		}
		n, e := strconv.ParseUint(p, 10, 64)
		if e != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

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
	want, wantOK := podcastVersion(PodcastPlayerMinVersion)
	if !ok || !wantOK {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}

// podcastVersion reads X.Y.Z[...]: the first three dot parts numeric, anything
// after a "-" or "+" ignored.
func podcastVersion(s string) ([3]uint64, bool) {
	var out [3]uint64
	s, _, _ = strings.Cut(s, "-")
	s, _, _ = strings.Cut(s, "+")
	parts := strings.Split(s, ".")
	if len(parts) < 3 {
		return out, false
	}
	for i, p := range parts[:3] {
		// ParseUint refuses an empty part and any sign or other non-digit.
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

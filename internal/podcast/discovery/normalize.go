package discovery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/netguard"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/podcast/opml"
)

// Normalization bounds.
const (
	maxEntries   = 1000 // provider rows inspected
	maxTextRunes = 500  // a title or author
	maxURLBytes  = 2048
	maxCount     = 2147483647
	// ArtworkPath is the relative proxy URL a result's artwork_url starts with.
	ArtworkPath = "/api/v1/podcasts/discovery/artwork?id="
)

// ProviderIDs are a result's directory ids, positive decimal strings or null.
type ProviderIDs struct {
	ITunes       *string `json:"itunes"`
	PodcastIndex *string `json:"podcast_index"`
}

// Result is one public directory entry. No local state: whether this server
// holds or the caller follows the feed is not part of it.
type Result struct {
	Title        string      `json:"title"`
	Author       string      `json:"author"`
	FeedURL      string      `json:"feed_url"`
	ArtworkURL   *string     `json:"artwork_url"`
	ProviderIDs  ProviderIDs `json:"provider_ids"`
	EpisodeCount *int64      `json:"episode_count"`
	ProviderURL  *string     `json:"provider_url"`
	art          string      // the upstream artwork URL ArtworkURL proxies
}

// Search is GET /podcasts/discovery/search's answer.
type Search struct {
	Provider Provider `json:"provider"`
	Query    string   `json:"query"`
	Country  *string  `json:"country"`
	Results  []Result `json:"results"`
}

// size approximates s's memory, for the result cache's byte bound.
func (s *Search) size() int {
	n := 256 + len(s.Query)
	for _, r := range s.Results {
		n += 256 + len(r.Title) + len(r.Author) + len(r.FeedURL) + len(r.art)*2
		if r.ProviderURL != nil {
			n += len(*r.ProviderURL)
		}
	}
	return n
}

// candidate is one provider row before the shared rules: the first valid value
// of each list wins.
type candidate struct {
	titles, authors []string
	feed            string
	art             []string
	itunesID, pIID  string
	count           *int64
	providerURL     string
}

// rules is what decides a public feed and a registrable image: the fixed
// public-address rule, with the test origin's exact address admitted in a
// podcastdiscoverytest build.
type rules struct{ origin netip.AddrPort }

// finish applies the shared rules to the rows in provider order: a title, a
// feed URL that is public and stable, the first valid row per normalized feed
// URL, then the limit.
func (r rules) finish(rows []candidate, limit int) []Result {
	out := []Result{}
	seen := map[string]bool{}
	for _, c := range rows {
		if len(out) == limit {
			break
		}
		title := firstText(c.titles)
		norm, ok := r.feedURL(c.feed)
		if title == "" || !ok || seen[norm] {
			continue
		}
		seen[norm] = true
		res := Result{Title: title, Author: firstText(c.authors), FeedURL: norm, EpisodeCount: c.count}
		if c.itunesID != "" {
			res.ProviderIDs.ITunes = &c.itunesID
		}
		if c.pIID != "" {
			res.ProviderIDs.PodcastIndex = &c.pIID
		}
		if c.providerURL != "" {
			res.ProviderURL = &c.providerURL
		}
		for _, a := range c.art {
			if r.artURL(a) {
				res.art = a
				proxied := ArtworkPath + ArtworkID(a)
				res.ArtworkURL = &proxied
				break
			}
		}
		out = append(out, res)
	}
	return out
}

func firstText(vals []string) string {
	for _, v := range vals {
		if t := opml.CleanText(v, maxTextRunes); t != "" {
			return t
		}
	}
	return ""
}

// ArtworkID is the registration id of an exact upstream artwork URL.
func ArtworkID(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// feedURL accepts a directory's feed URL as a follow would (feed.NormalizeFeedURL,
// stable under a second normalization, at most 2048 bytes), and only when it
// is plainly public: never one that looks private (credentials, a query, a
// token-like path, a member host), nor a literal non-public address, localhost
// or a .local name. Nothing is resolved or fetched.
func (r rules) feedURL(raw string) (string, bool) {
	norm, err := feed.NormalizeFeedURL(raw)
	if err != nil || len(norm) > maxURLBytes {
		return "", false
	}
	if again, err := feed.NormalizeFeedURL(norm); err != nil || again != norm || feed.IsPrivateFeedURL(norm) {
		return "", false
	}
	u, err := url.Parse(norm)
	if err != nil || u.User != nil || u.RawQuery != "" || !r.publicHost(u) {
		return "", false
	}
	return norm, true
}

// artURL reports whether raw may be registered as an artwork URL: absolute
// http(s), at most 2048 bytes, no user info or fragment, not a literal blocked
// host (a query is allowed; the URL never reaches a client).
func (r rules) artURL(raw string) bool {
	if raw == "" || len(raw) > maxURLBytes || strings.ContainsAny(raw, "# \t\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" {
		return false
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return false
	}
	return r.publicHost(u)
}

// publicHost refuses a literal non-public address (the test origin's exact
// address and port excepted), localhost and .local names.
func (r rules) publicHost(u *url.URL) bool {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "local" || strings.HasSuffix(host, ".local") {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return true // a name: the dial is guarded, and a feed is checked again by the Bridge
	}
	if r.origin.IsValid() && addr.Unmap() == r.origin.Addr() && portOf(u) == r.origin.Port() {
		return true
	}
	return netguard.PublicAddr(addr)
}

func portOf(u *url.URL) uint16 {
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return 0
		}
		return uint16(n)
	}
	if strings.EqualFold(u.Scheme, "https") {
		return 443
	}
	return 80
}

// jsonString is raw as a JSON string, "" when it is anything else.
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// jsonID is raw as a positive decimal id, read losslessly from a JSON integer
// (never through float64) or a string of digits; "" when it is neither.
func jsonID(raw json.RawMessage) string {
	s := string(bytes.TrimSpace(raw))
	if strings.HasPrefix(s, `"`) {
		s = jsonString(raw)
	}
	if s == "" || len(s) > 20 || s[0] == '0' {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return ""
		}
	}
	return s
}

// jsonCount is raw as an episode count: a JSON integer 0..2147483647, nil
// otherwise (zero is a known count).
func jsonCount(raw json.RawMessage) *int64 {
	s := string(bytes.TrimSpace(raw))
	if s == "" || len(s) > 10 {
		return nil
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > maxCount || len(s) > 1 && s[0] == '0' {
		return nil
	}
	return &n
}

// appleURL is raw when it is an HTTPS page on podcasts.apple.com or
// itunes.apple.com (no user info, default port), else "".
func appleURL(raw string) string {
	if raw == "" || len(raw) > maxURLBytes {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Port() != "" && u.Port() != "443" {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "podcasts.apple.com", "itunes.apple.com":
		return raw
	}
	return ""
}

// decodeRows reads the provider's array of rows (at most maxEntries looked at);
// a missing or non-array value fails, [] succeeds, a row that isn't an object
// is skipped.
func decodeRows(raw json.RawMessage) ([]map[string]json.RawMessage, bool) {
	var rows []json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' || json.Unmarshal(raw, &rows) != nil {
		return nil, false
	}
	out := []map[string]json.RawMessage{}
	for i, row := range rows {
		if i == maxEntries {
			break
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(row, &m) == nil && m != nil {
			out = append(out, m)
		}
	}
	return out, true
}

// itunesRows normalizes an iTunes Search API envelope ({"results": [...]}).
func itunesRows(body []byte) ([]candidate, bool) {
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil {
		return nil, false
	}
	rows, ok := decodeRows(env["results"])
	if !ok {
		return nil, false
	}
	out := make([]candidate, 0, len(rows))
	for _, m := range rows {
		if jsonString(m["kind"]) != "podcast" {
			continue
		}
		id := jsonID(m["collectionId"])
		if id == "" {
			id = jsonID(m["trackId"])
		}
		pageURL := appleURL(jsonString(m["collectionViewUrl"]))
		if pageURL == "" {
			pageURL = appleURL(jsonString(m["trackViewUrl"]))
		}
		out = append(out, candidate{
			titles:      []string{jsonString(m["collectionName"]), jsonString(m["trackName"])},
			authors:     []string{jsonString(m["artistName"])},
			feed:        jsonString(m["feedUrl"]),
			art:         []string{jsonString(m["artworkUrl600"]), jsonString(m["artworkUrl100"]), jsonString(m["artworkUrl60"])},
			itunesID:    id,
			count:       jsonCount(m["trackCount"]),
			providerURL: pageURL,
		})
	}
	return out, true
}

// indexRows normalizes a Podcast Index search envelope ({"status": "true",
// "feeds": [...]}; a boolean true status is tolerated).
func indexRows(body []byte) ([]candidate, bool) {
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil {
		return nil, false
	}
	if st := bytes.TrimSpace(env["status"]); string(st) != `"true"` && string(st) != "true" {
		return nil, false
	}
	rows, ok := decodeRows(env["feeds"])
	if !ok {
		return nil, false
	}
	out := make([]candidate, 0, len(rows))
	for _, m := range rows {
		id := jsonID(m["id"])
		c := candidate{
			titles:   []string{jsonString(m["title"])},
			authors:  []string{jsonString(m["author"])},
			feed:     jsonString(m["url"]),
			art:      []string{jsonString(m["image"]), jsonString(m["artwork"])},
			itunesID: jsonID(m["itunesId"]),
			pIID:     id,
			count:    jsonCount(m["episodeCount"]),
		}
		if id != "" {
			c.providerURL = "https://podcastindex.org/podcast/" + id
		}
		out = append(out, c)
	}
	return out, true
}

package discovery

import (
	"net/netip"
	"net/url"
	"os"
	"strconv"
)

// TestOriginEnv names the fixture origin a podcastdiscoverytest build sends
// every directory request to (http://127.0.0.1:<port>, nothing else).
const TestOriginEnv = "AUDIOSILO_TEST_PODCAST_DISCOVERY_ORIGIN"

// TestOriginBuild reports whether this binary honours TestOriginEnv.
func TestOriginBuild() bool { return testOriginBuild }

// The fixed origins an ordinary build asks.
const (
	itunesSearchURL = "https://itunes.apple.com/search"
	indexSearchURL  = "https://api.podcastindex.org/api/1.0/search/byterm"
)

// testOrigin is the fixture origin of a podcastdiscoverytest build, the zero
// value otherwise (or for a value that isn't exactly http://127.0.0.1:<port>).
func testOrigin() netip.AddrPort {
	if !testOriginBuild {
		return netip.AddrPort{}
	}
	return parseTestOrigin(os.Getenv(TestOriginEnv))
}

// parseTestOrigin accepts only http://127.0.0.1:<port> with an explicit port
// and no user info, path, query or fragment.
func parseTestOrigin(s string) netip.AddrPort {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Hostname() != "127.0.0.1" {
		return netip.AddrPort{}
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || port == 0 || u.Host != "127.0.0.1:"+u.Port() {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))
}

// endpoints are where a service sends each provider's searches and how a
// fixture art URL is recognised: the fixed origins, or the test origin's
// /itunes/search and /index/api/1.0/search/byterm.
type endpoints struct {
	itunes, index string
	origin        netip.AddrPort // valid only in a test build with the variable set
}

func endpointsFor(origin netip.AddrPort) endpoints {
	if !origin.IsValid() {
		return endpoints{itunes: itunesSearchURL, index: indexSearchURL}
	}
	base := "http://" + origin.String()
	return endpoints{itunes: base + "/itunes/search", index: base + "/index/api/1.0/search/byterm", origin: origin}
}

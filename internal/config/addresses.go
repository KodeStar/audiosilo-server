package config

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Addresses are the server's two addresses for a request that arrived as
// scheme://host (the `addresses` capability). away is the configured public_url,
// the address that works from anywhere. home is the configured lan_url or, when
// none is set and the request itself came in on a home-network address, that
// address. Either may be "": no public_url, or no home address known. A home
// address equal to the away one is dropped (there is only one address then).
func (c *Config) Addresses(scheme, host string) (home, away string) {
	away = strings.TrimRight(c.PublicURL, "/")
	home = strings.TrimRight(c.LANURL, "/")
	if home == "" && isHomeNetworkHost(host) {
		if ip, err := netip.ParseAddr(host); err == nil && ip.Is6() {
			host = "[" + host + "]" // a bare IPv6 Host: a URL needs it bracketed
		}
		home = scheme + "://" + host
	}
	if strings.EqualFold(home, away) {
		home = ""
	}
	return home, away
}

// isHomeNetworkHost reports whether a request's Host ("host" or "host:port", an
// IPv6 address in brackets) names this server on a home network: a private IP
// (RFC 1918, IPv6 ULA fc00::/7), a link-local one, or a name ending in .local,
// .lan, .home.arpa or .internal, or a single-label name. Loopback (127.0.0.0/8, ::1,
// localhost) is not: no other device can reach it. Nor is carrier-grade NAT
// space (100.64.0.0/10), which Go's IsPrivate leaves out: it is the ISP's
// network, not the household's.
func isHomeNetworkHost(hostport string) bool {
	host := hostport
	if h, port, err := net.SplitHostPort(hostport); err == nil {
		if !isPort(port) {
			return false
		}
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	ip, err := netip.ParseAddr(host)
	if err == nil {
		ip = ip.Unmap()
		return !ip.IsLoopback() && (ip.IsPrivate() || ip.IsLinkLocalUnicast())
	}
	if strings.HasPrefix(hostport, "[") { // brackets hold an IPv6 address, never a name
		return false
	}
	name := strings.TrimSuffix(strings.ToLower(host), ".")
	if !isHostName(name) || name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return false
	}
	for _, suffix := range []string{".local", ".lan", ".home.arpa", ".internal"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return !strings.Contains(name, ".")
}

// isPort reports whether s is a port number, 1 to 65535.
func isPort(s string) bool {
	n, err := strconv.ParseUint(s, 10, 16)
	return err == nil && n > 0
}

// isHostName reports whether s is a DNS name of letters, digits and hyphens in
// non-empty dot-separated labels (lowercase already).
func isHostName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

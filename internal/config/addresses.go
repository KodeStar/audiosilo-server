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
//
// A derived home address takes the request's scheme, except on a server that
// serves home HTTP (ServesHomeHTTP): there it is http, which the native apps can
// use where they can't trust the self-signed certificate, even when the request
// came over https; it keeps the port the request came in on, so a Host without
// one (https on 443) gives http://host:443. A configured lan_url is used as it is.
func (c *Config) Addresses(scheme, host string) (home, away string) {
	away = strings.TrimRight(c.PublicURL, "/")
	home = strings.TrimRight(c.LANURL, "/")
	if home == "" && isHomeNetworkHost(host) {
		if ip, err := netip.ParseAddr(host); err == nil && ip.Is6() {
			host = "[" + host + "]" // a bare IPv6 Host: a URL needs it bracketed
		}
		if c.ServesHomeHTTP() && scheme == "https" {
			scheme = "http"
			host = HostWithPort(host, "443") // https's default port, which now answers http, never 80
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
	if ip, err := netip.ParseAddr(host); err == nil {
		return HomeNetworkIP(ip)
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

// HostWithPort is a request's Host ("name", "1.2.3.4", "[::1]" or a bare IPv6
// address, with or without a port) with port added when it has none, an IPv6
// address bracketed.
func HostWithPort(host, port string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), port)
}

// ServesHomeHTTP reports whether the listening port also answers plain HTTP to
// home-network clients: tls.home_http on a self-signed server using its own
// generated certificate. A certificate the admin supplied (tls.cert_file) may be
// one the devices trust (a private CA, mkcert), so they keep https.
func (c *Config) ServesHomeHTTP() bool {
	return c.TLS.Mode == TLSSelfSigned && c.TLS.HomeHTTP && c.TLS.CertFile == ""
}

// HomeNetworkHost reports whether a request's Host names the server on a home
// network (isHomeNetworkHost): a private or link-local IP, a .local/.lan/
// .home.arpa/.internal name or a single-label one.
func HomeNetworkHost(hostport string) bool { return isHomeNetworkHost(hostport) }

// HomeNetworkIP reports whether ip is a home-network address: private (RFC 1918,
// IPv6 ULA fc00::/7) or link-local, an IPv4-mapped IPv6 address judged as its
// IPv4 one. Loopback is not, and neither is carrier-grade NAT space
// (100.64.0.0/10), which is the ISP's network, not the household's.
func HomeNetworkIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsPrivate() || ip.IsLinkLocalUnicast()
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

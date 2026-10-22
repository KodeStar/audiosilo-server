// Package netguard keeps the server's own outbound fetches (community covers,
// podcast directory searches and their artwork) on the public internet. It
// checks the address every connection actually dials, after the name was
// resolved, so a public name cannot lead a fetch into the server's own network,
// and it never goes through a proxy, where the check would see only the
// proxy's address. It is a leaf: no other server package is imported.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlockedAddress is a connection to an address the guard refuses.
var ErrBlockedAddress = errors.New("netguard: not a public address")

// notPublic are the ranges IsGlobalUnicast and IsPrivate leave in that are still
// no public host's.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved
	netip.MustParsePrefix("::/96"),          // IPv4-compatible (deprecated), which embeds any IPv4 address
	netip.MustParsePrefix("2002::/16"),      // 6to4, which embeds any IPv4 address
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64 (RFC 8215), a network's own translator
	netip.MustParsePrefix("fec0::/10"),      // site-local (deprecated)
}

// nat64 is the well-known NAT64 prefix: on an IPv6-only network every IPv4 host
// is reached through it, so the IPv4 address it carries is what is checked.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// PublicAddr reports whether a is an address on the public internet: not
// loopback, private, link-local, multicast, unspecified or reserved (an
// IPv4-mapped or well-known NAT64 address is read as the IPv4 address it
// carries).
func PublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if nat64.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte(b[12:]))
	}
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// PublicAddrPort is the usual rule for a dial: a public address, any port.
func PublicAddrPort(ap netip.AddrPort) bool { return PublicAddr(ap.Addr()) }

// Control is a net.Dialer Control refusing every connection whose resolved
// address allowed refuses (ErrBlockedAddress). It sees the address actually
// dialled, so DNS rebinding cannot slip past a check made before the dial.
func Control(allowed func(netip.AddrPort) bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		if !allowed(ap) {
			return fmt.Errorf("%w: %s", ErrBlockedAddress, ap.Addr())
		}
		return nil
	}
}

// Dialer is a net.Dialer with the guard's Control and a connect timeout.
func Dialer(allowed func(netip.AddrPort) bool, timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: Control(allowed)}
}

package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34":          true, // a public IPv4 host
		"2606:2800:220:1::248":   true, // a public IPv6 host
		"::ffff:93.184.216.34":   true, // IPv4-mapped, read as the IPv4 address
		"64:ff9b::5db8:d822":     true, // NAT64 of a public IPv4 host
		"127.0.0.1":              false,
		"::1":                    false,
		"10.1.2.3":               false,
		"172.16.0.1":             false,
		"192.168.0.10":           false,
		"169.254.169.254":        false, // cloud metadata
		"fe80::1":                false,
		"fd00::1":                false,
		"0.0.0.0":                false,
		"0.1.2.3":                false,
		"100.64.0.1":             false, // carrier-grade NAT
		"198.18.0.1":             false,
		"224.0.0.1":              false,
		"255.255.255.255":        false,
		"::ffff:127.0.0.1":       false,
		"64:ff9b::a00:1":         false, // NAT64 of 10.0.0.1
		"2002:c0a8:1::1":         false, // 6to4 of 192.168.0.1
		"::7f00:1":               false, // IPv4-compatible 127.0.0.1
		"::5db8:d822":            false, // IPv4-compatible, deprecated whatever it embeds
		"64:ff9b:1::a00:1":       false, // local-use NAT64 of 10.0.0.1
		"fec0::1":                false, // site-local
		"::":                     false,
		"ff02::1":                false,
		"240.0.0.1":              false,
		"192.0.0.8":              false,
		"100.127.255.255":        false,
		"100.128.0.1":            true, // just past carrier-grade NAT
		"2001:4860:4860::8888":   true,
		"::ffff:192.168.100.200": false,
	} {
		if got := PublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("PublicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

// The Control sees the dialled address: loopback is refused by the public
// rule and admitted only by a rule that names it, port included.
func TestDialerChecksTheDialledAddress(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	defer srv.Close()
	target := netip.MustParseAddrPort(srv.Listener.Addr().String())
	get := func(allowed func(netip.AddrPort) bool) error {
		c := &http.Client{Transport: &http.Transport{DialContext: Dialer(allowed, time.Second).DialContext}}
		resp, err := c.Get(srv.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := get(PublicAddrPort); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("public rule on loopback = %v", err)
	}
	other := netip.AddrPortFrom(target.Addr(), target.Port()+1)
	if err := get(func(ap netip.AddrPort) bool { return ap == other }); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("other port = %v", err)
	}
	if hits != 0 {
		t.Fatal("a refused dial reached the server")
	}
	if err := get(func(ap netip.AddrPort) bool { return ap == target }); err != nil || hits != 1 {
		t.Fatalf("exact address = %v, %d hits", err, hits)
	}
	// A mapped address is unmapped before the rule sees it.
	var seen netip.AddrPort
	ctl := Control(func(ap netip.AddrPort) bool { seen = ap; return false })
	if err := ctl("tcp6", "[::ffff:127.0.0.1]:80", nil); !errors.Is(err, ErrBlockedAddress) || seen.Addr() != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("mapped = %v %v", err, seen)
	}
	if err := ctl("tcp", "not an address", nil); err == nil || errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("unparsable = %v", err)
	}
	var d net.Dialer = *Dialer(PublicAddrPort, time.Second)
	if _, err := d.DialContext(context.Background(), "tcp", "10.255.255.1:443"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("private dial = %v", err)
	}
}

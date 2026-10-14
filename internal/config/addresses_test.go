package config

import (
	"net/netip"
	"reflect"
	"slices"
	"testing"
)

func TestHomeNetworkHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		// RFC 1918, with and without a port.
		{"10.0.0.5", true},
		{"10.0.0.5:8080", true},
		{"172.16.4.2:8080", true},
		{"172.31.255.255", true},
		{"192.168.1.20:8080", true},
		{"192.168.1.20", true},
		// Just outside the private ranges.
		{"172.32.0.1:8080", false},
		{"192.169.1.1", false},
		// Carrier-grade NAT is the ISP's network, not the household's.
		{"100.64.0.1:8080", false},
		{"100.127.255.254", false},
		// Link-local.
		{"169.254.10.20:8080", true},
		{"[fe80::1]:8080", true},
		{"[fe80::1%25en0]:8080", true},
		// IPv6 ULA, bracketed with and without a port, and bare.
		{"[fd12:3456::20]:8080", true},
		{"[fd12:3456::20]", true},
		{"fd12:3456::20", true},
		{"[fc00::1]:443", true},
		// IPv4-mapped IPv6 is judged as the IPv4 address.
		{"[::ffff:192.168.1.20]:8080", true},
		{"[::ffff:8.8.8.8]:8080", false},
		// Loopback and unspecified: no other device can reach them.
		{"127.0.0.1:8080", false},
		{"127.0.0.1", false},
		{"127.8.9.10", false},
		{"[::1]:8080", false},
		{"[::1]", false},
		{"::1", false},
		{"0.0.0.0:8080", false},
		{"localhost:8080", false},
		{"localhost", false},
		{"LOCALHOST", false},
		{"app.localhost:8080", false},
		// Public addresses.
		{"8.8.8.8", false},
		{"203.0.113.9:8080", false},
		{"[2001:db8::1]:8080", false},
		{"books.example.com", false},
		{"books.example.com:8443", false},
		// Home-network names.
		{"nas.local:8080", true},
		{"nas.local", true},
		{"NAS.Local.", true},
		{"books.lan", true},
		{"server.home.arpa:8080", true},
		{"nas.internal", true},
		{"nas.internal:8080", true},
		{"internal", true},
		{"foo.internal.example.com", false},
		{"nas", true},
		{"nas:8080", true},
		{"my-nas:8080", true},
		// Names that are not hostnames, and bad ports.
		{"", false},
		{":8080", false},
		{"nas:0", false},
		{"nas:99999", false},
		{"nas:http", false},
		{"bad_name:8080", false},
		{"-nas:8080", false},
		{"nas..local", false},
		{"[nas]:8080", false},
	}
	for _, tc := range cases {
		if got := isHomeNetworkHost(tc.host); got != tc.want {
			t.Errorf("isHomeNetworkHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// HomeNetworkIP is the address predicate under isHomeNetworkHost, and what the
// listener's home HTTP gate builds on (adding loopback there).
func TestHomeNetworkIP(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"10.1.2.3": true, "172.16.0.1": true, "192.168.1.205": true, "169.254.1.1": true,
		"fd00::1": true, "fe80::1": true, "::ffff:192.168.1.5": true,
		"100.64.0.1": false, "100.127.255.254": false, "127.0.0.1": false, "::1": false,
		"8.8.8.8": false, "2001:db8::1": false, "0.0.0.0": false, "::": false,
	}
	for addr, want := range cases {
		if got := HomeNetworkIP(netip.MustParseAddr(addr)); got != want {
			t.Errorf("HomeNetworkIP(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestAddresses(t *testing.T) {
	cases := []struct {
		name            string
		public, lan     string
		scheme, host    string
		wantHome, wantA string
	}{
		{"nothing configured, public request", "", "", "https", "books.example.com", "", ""},
		{"nothing configured, loopback request", "", "", "http", "127.0.0.1:8080", "", ""},
		{"public only, public request", "https://books.example.com/", "", "https", "books.example.com", "", "https://books.example.com"},
		{"derived from a LAN request", "https://books.example.com", "", "http", "192.168.1.20:8080",
			"http://192.168.1.20:8080", "https://books.example.com"},
		{"derived keeps the request's scheme", "", "", "https", "nas.local:8443", "https://nas.local:8443", ""},
		{"a bare IPv6 host is bracketed", "", "", "http", "fd12:3456::20", "http://[fd12:3456::20]", ""},
		{"a bracketed IPv6 host is kept", "", "", "http", "[fd12:3456::20]:8080", "http://[fd12:3456::20]:8080", ""},
		{"configured lan wins over the request", "https://books.example.com", "http://10.0.0.2:8080/", "http", "192.168.1.20:8080",
			"http://10.0.0.2:8080", "https://books.example.com"},
		{"configured lan on a public request", "https://books.example.com", "http://10.0.0.2:8080", "https", "books.example.com",
			"http://10.0.0.2:8080", "https://books.example.com"},
		{"home equal to away is dropped", "http://192.168.1.20:8080", "", "http", "192.168.1.20:8080", "", "http://192.168.1.20:8080"},
		{"configured home equal to away is dropped", "https://books.example.com", "HTTPS://books.example.com/", "https", "x",
			"", "https://books.example.com"},
	}
	for _, tc := range cases {
		c := Default(t.TempDir())
		c.PublicURL, c.LANURL = tc.public, tc.lan
		c.TLS.HomeHTTP = false // home HTTP's own cases are TestAddressesHomeHTTP
		home, away := c.Addresses(tc.scheme, tc.host)
		if home != tc.wantHome || away != tc.wantA {
			t.Errorf("%s: Addresses = (%q, %q), want (%q, %q)", tc.name, home, away, tc.wantHome, tc.wantA)
		}
	}
}

// lan_url is a console setting checked like public_url, and AUDIOSILO_LAN_URL
// sets and locks it.
func TestLANURLSetting(t *testing.T) {
	c := Default(t.TempDir())
	next, err := c.WithSettings(patch(map[string]string{"general.lan_url": `" http://192.168.1.20:8080/ "`}), ok)
	if err != nil {
		t.Fatal(err)
	}
	if next.LANURL != "http://192.168.1.20:8080" {
		t.Fatalf("LANURL = %q, want it normalized", next.LANURL)
	}
	if got := next.Settings()["general"]["lan_url"]; got != "http://192.168.1.20:8080" {
		t.Fatalf("Settings general.lan_url = %v", got)
	}
	for _, bad := range []string{`"192.168.1.20:8080"`, `"ftp://nas"`, `"http://nas/?a=1"`} {
		_, err := c.WithSettings(patch(map[string]string{"general.lan_url": bad}), ok)
		if se := settingErr(t, err); se.Reason != ReasonInvalid || se.Setting != "general.lan_url" {
			t.Errorf("%s: got %s/%s, want invalid general.lan_url", bad, se.Setting, se.Reason)
		}
	}
	if slices.Contains(RestartSettings(), "general.lan_url") {
		t.Fatal("lan_url applies without a restart")
	}

	t.Setenv("AUDIOSILO_LAN_URL", "http://nas.local:8080")
	loaded, _, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LANURL != "http://nas.local:8080" {
		t.Fatalf("env not applied: %q", loaded.LANURL)
	}
	if !reflect.DeepEqual(loaded.Locked(), map[string]string{"general.lan_url": "AUDIOSILO_LAN_URL"}) {
		t.Fatalf("Locked = %v", loaded.Locked())
	}
	_, err = loaded.WithSettings(patch(map[string]string{"general.lan_url": `"http://x.lan"`}), ok)
	if se := settingErr(t, err); se.Reason != ReasonLocked {
		t.Fatalf("an env lan_url must be locked, got %s", se.Reason)
	}
}

// With home HTTP (tls.home_http on a self-signed server) a derived home address
// is http even for a request that came over https; a configured lan_url, a
// public request and the other TLS modes are as before.
func TestAddressesHomeHTTP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		mode            TLSMode
		homeHTTP        bool
		public, lan     string
		scheme, host    string
		wantHome, wantA string
	}{
		{"https LAN request derives an http home", TLSSelfSigned, true, "", "", "https", "192.168.1.205:8080",
			"http://192.168.1.205:8080", ""},
		{"https's default port is kept, never http's", TLSSelfSigned, true, "", "", "https", "192.168.1.205",
			"http://192.168.1.205:443", ""},
		{"a bare IPv6 host on 443", TLSSelfSigned, true, "", "", "https", "fd12::20", "http://[fd12::20]:443", ""},
		{"a bracketed IPv6 host on 443", TLSSelfSigned, true, "", "", "https", "[fd12::20]", "http://[fd12::20]:443", ""},
		{"a name on 443", TLSSelfSigned, true, "", "", "https", "nas.local", "http://nas.local:443", ""},
		{"a plain request without a port stays on 80", TLSSelfSigned, true, "", "", "http", "nas.local", "http://nas.local", ""},
		{"a plain LAN request too", TLSSelfSigned, true, "", "", "http", "nas.local:8080", "http://nas.local:8080", ""},
		{"IPv6 ULA host", TLSSelfSigned, true, "", "", "https", "[fd12::20]:8080", "http://[fd12::20]:8080", ""},
		{"an https public_url on the LAN host keeps the http home", TLSSelfSigned, true,
			"https://192.168.1.205:8080", "", "https", "192.168.1.205:8080",
			"http://192.168.1.205:8080", "https://192.168.1.205:8080"},
		{"a configured lan_url is used as it is", TLSSelfSigned, true, "", "https://nas.local:8080", "https", "192.168.1.205:8080",
			"https://nas.local:8080", ""},
		{"a public request derives no home", TLSSelfSigned, true, "", "", "https", "books.example.com", "", ""},
		{"a loopback request derives no home", TLSSelfSigned, true, "", "", "https", "127.0.0.1:8080", "", ""},
		{"home_http off keeps the scheme", TLSSelfSigned, false, "", "", "https", "192.168.1.205:8080",
			"https://192.168.1.205:8080", ""},
		{"autocert ignores home_http", TLSAutocert, true, "", "", "https", "192.168.1.205:443",
			"https://192.168.1.205:443", ""},
		{"mode off ignores home_http", TLSOff, true, "", "", "https", "192.168.1.205:8080",
			"https://192.168.1.205:8080", ""},
	}
	for _, tc := range cases {
		c := Default(t.TempDir())
		c.TLS.Mode, c.TLS.HomeHTTP = tc.mode, tc.homeHTTP
		c.PublicURL, c.LANURL = tc.public, tc.lan
		home, away := c.Addresses(tc.scheme, tc.host)
		if home != tc.wantHome || away != tc.wantA {
			t.Errorf("%s: Addresses = (%q, %q), want (%q, %q)", tc.name, home, away, tc.wantHome, tc.wantA)
		}
	}
}

// Home HTTP needs the server's own generated certificate: with a supplied one
// (tls.cert_file, perhaps trusted by the devices) or another TLS mode it is off.
func TestServesHomeHTTP(t *testing.T) {
	t.Parallel()
	c := Default(t.TempDir())
	if !c.ServesHomeHTTP() {
		t.Fatal("the default self-signed server must serve home HTTP")
	}
	c.TLS.CertFile = "/certs/nas.lan.pem"
	if c.ServesHomeHTTP() {
		t.Fatal("a supplied certificate must keep the devices on https")
	}
	c = Default(t.TempDir())
	c.TLS.Mode = TLSOff
	if c.ServesHomeHTTP() {
		t.Fatal("tls.mode off has no TLS port to share")
	}
}

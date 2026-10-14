package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// selfSignedTLS is a self-signed tls.Config as the server builds it.
func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	dir := t.TempDir()
	cfg, err := tlsConfig(&config.Config{DataDir: dir, TLS: config.TLSConfig{
		Mode: config.TLSSelfSigned, CertFile: filepath.Join(dir, "c.pem"), KeyFile: filepath.Join(dir, "k.pem"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// protoHandler answers which protocol a request came over, and its HTTP version.
var protoHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if r.TLS != nil {
		_, _ = fmt.Fprintf(w, "tls %s", r.Proto)
		return
	}
	_, _ = fmt.Fprintf(w, "plain %s", r.Proto)
})

// get fetches url with client and returns the status and body.
func get(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// tlsClient trusts any certificate (the server's is self-signed) and offers HTTP/2.
func tlsClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the test server's self-signed cert
			ForceAttemptHTTP2: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// plainClient speaks plain HTTP and doesn't follow redirects.
func plainClient() *http.Client {
	return &http.Client{
		Transport:     &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// One port, real TCP: a TLS client (negotiating HTTP/2, as with ServeTLS) and a
// plain one are each served on their own protocol, a client that sends nothing
// holds up neither, and is closed once its peek times out.
func TestSplitListenerServesBothProtocols(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := selfSignedTLS(t)
	tlsCfg.NextProtos = []string{"h2", "http/1.1"}
	srv := &http.Server{Handler: protoHandler, TLSConfig: tlsCfg, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(newSplitListener(ln, tlsCfg, 300*time.Millisecond)) }()
	addr := ln.Addr().String()

	// A silent client first: it must not block the accept loop.
	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()

	if code, body := get(t, tlsClient(), "https://"+addr+"/x"); code != 200 || body != "tls HTTP/2.0" {
		t.Fatalf("TLS client: %d %q, want 200 \"tls HTTP/2.0\"", code, body)
	}
	if code, body := get(t, plainClient(), "http://"+addr+"/x"); code != 200 || body != "plain HTTP/1.1" {
		t.Fatalf("plain client: %d %q, want 200 \"plain HTTP/1.1\"", code, body)
	}

	// The silent client is closed after the peek timeout (300ms), not left open.
	_ = silent.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := silent.Read(make([]byte, 1))
	var ne net.Error
	if n != 0 || err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("silent client: read %d, %v; want the server to have closed it", n, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve = %v, want ErrServerClosed", err)
	}
}

// Closing the listener (a shutdown) closes a connection still waiting to send its
// first byte at once, rather than when its peek times out.
func TestSplitListenerCloseDropsPeekingConn(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sl := newSplitListener(ln, selfSignedTLS(t), time.Minute)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(50 * time.Millisecond) // let the accept loop take it
	if err := sl.Close(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Read(make([]byte, 1))
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("read after Close = %v, want the connection closed", err)
	}
	if _, err := sl.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
	}
}

// The gate serves TLS requests and plain ones from the home network (by the
// connection's own address); every other plain request goes to https.
func TestHomeHTTPGate(t *testing.T) {
	t.Parallel()
	served := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "served") })
	gate := homeHTTPGate(served)
	cases := []struct {
		name, remote, host, target string
		xff                        string // X-Forwarded-For, never trusted here
		tls                        bool
		wantCode                   int
		wantLocation               string
	}{
		// Allowed: home-network clients over plain HTTP.
		{name: "RFC 1918", remote: "192.168.1.20:5000", wantCode: 200},
		{name: "10/8", remote: "10.0.0.7:5000", wantCode: 200},
		{name: "172.16/12 (a Docker bridge gateway)", remote: "172.17.0.1:5000", wantCode: 200},
		{name: "link-local", remote: "169.254.3.4:5000", wantCode: 200},
		{name: "IPv6 ULA", remote: "[fd12:3456::20]:5000", wantCode: 200},
		{name: "IPv6 link-local with a zone", remote: "[fe80::1%en0]:5000", wantCode: 200},
		{name: "IPv4-mapped private", remote: "[::ffff:192.168.1.20]:5000", wantCode: 200},
		{name: "loopback", remote: "127.0.0.1:5000", wantCode: 200},
		{name: "IPv6 loopback", remote: "[::1]:5000", wantCode: 200},
		{name: "loopback, any Host", remote: "127.0.0.1:5000", host: "books.example.com", wantCode: 200},
		{name: "a home name", remote: "192.168.1.20:5000", host: "nas.local:8080", wantCode: 200},
		// Allowed: anyone over TLS.
		{name: "public over TLS", remote: "203.0.113.9:5000", tls: true, wantCode: 200},
		// Denied: redirected to the same URL on https.
		{name: "public", remote: "203.0.113.9:5000", wantCode: 308,
			wantLocation: "https://192.168.1.205:8080/api/v1/server?a=1"},
		{name: "CGNAT is not home", remote: "100.64.0.1:5000", wantCode: 308,
			wantLocation: "https://192.168.1.205:8080/api/v1/server?a=1"},
		{name: "public IPv6", remote: "[2001:db8::1]:5000", wantCode: 308,
			wantLocation: "https://192.168.1.205:8080/api/v1/server?a=1"},
		{name: "a forged X-Forwarded-For is ignored", remote: "203.0.113.9:5000", xff: "192.168.1.20", wantCode: 308,
			wantLocation: "https://192.168.1.205:8080/api/v1/server?a=1"},
		{name: "IPv4-mapped public", remote: "[::ffff:8.8.8.8]:5000", wantCode: 308,
			wantLocation: "https://192.168.1.205:8080/api/v1/server?a=1"},
		{name: "a bracketed IPv6 Host", remote: "203.0.113.9:5000", host: "[2001:db8::5]:8080", wantCode: 308,
			wantLocation: "https://[2001:db8::5]:8080/api/v1/server?a=1"},
		{name: "unparseable remote address", remote: "nonsense", wantCode: 308,
			wantLocation: "https://192.168.1.205:8080/api/v1/server?a=1"},
		// Denied: a private address naming the server publicly is an internet client
		// a gateway handed over (Docker's IPv6 proxy, a router rewriting the source).
		{name: "a gateway's private address, a public Host", remote: "172.17.0.1:5000", host: "books.example.com:8080",
			wantCode: 308, wantLocation: "https://books.example.com:8080/api/v1/server?a=1"},
		{name: "a gateway's private address, a public IP Host", remote: "10.0.0.1:5000", host: "203.0.113.50:8080",
			wantCode: 308, wantLocation: "https://203.0.113.50:8080/api/v1/server?a=1"},
		// A Host without a port came in on 80, which answers TLS too.
		{name: "a port-less public Host", remote: "203.0.113.9:5000", host: "books.example.com", wantCode: 308,
			wantLocation: "https://books.example.com:80/api/v1/server?a=1"},
		// Denied, and no usable Host to redirect to.
		{name: "no Host", remote: "203.0.113.9:5000", host: "-", wantCode: 400},
		{name: "a Host with userinfo", remote: "203.0.113.9:5000", host: "evil@example.com", wantCode: 400},
		{name: "a Host with a path", remote: "203.0.113.9:5000", host: "example.com/x", wantCode: 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://192.168.1.205:8080/api/v1/server?a=1", nil)
			r.RemoteAddr = tc.remote
			r.TLS = nil
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			switch tc.host {
			case "":
			case "-":
				r.Host = ""
			default:
				r.Host = tc.host
			}
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, r)
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d (body %q)", rec.Code, tc.wantCode, rec.Body)
			}
			if tc.wantCode == 200 && rec.Body.String() != "served" {
				t.Fatalf("body = %q, want the handler's", rec.Body)
			}
			if tc.wantCode != 200 && strings.Contains(rec.Body.String(), "served") {
				t.Fatal("a denied request reached the handler")
			}
			if got := rec.Header().Get("Location"); got != tc.wantLocation {
				t.Fatalf("Location = %q, want %q", got, tc.wantLocation)
			}
		})
	}
}

// freeAddr is a loopback address with a port nothing listens on now.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// startRun runs Run for cfg until the test ends, waiting until it accepts.
func startRun(t *testing.T, cfg *config.Config) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, protoHandler, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v, want nil after a graceful shutdown", err)
		}
	})
	for range 100 {
		if c, err := net.Dial("tcp", cfg.Bind); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server never accepted")
}

// Run on a self-signed server with home HTTP: the port serves both protocols
// (the loopback client is home), and shuts down gracefully on ctx.
func TestRunHomeHTTP(t *testing.T) {
	t.Parallel()
	cfg := config.Default(t.TempDir())
	cfg.Bind = freeAddr(t)
	startRun(t, cfg)
	if code, body := get(t, tlsClient(), "https://"+cfg.Bind+"/"); code != 200 || body != "tls HTTP/2.0" {
		t.Fatalf("TLS: %d %q", code, body)
	}
	if code, body := get(t, plainClient(), "http://"+cfg.Bind+"/"); code != 200 || body != "plain HTTP/1.1" {
		t.Fatalf("plain: %d %q", code, body)
	}
}

// With home_http off the port is TLS only: a plain request gets Go's "HTTP
// request to an HTTPS server" 400, never the handler.
func TestRunHomeHTTPOff(t *testing.T) {
	t.Parallel()
	cfg := config.Default(t.TempDir())
	cfg.Bind = freeAddr(t)
	cfg.TLS.HomeHTTP = false
	startRun(t, cfg)
	if code, body := get(t, tlsClient(), "https://"+cfg.Bind+"/"); code != 200 || body != "tls HTTP/2.0" {
		t.Fatalf("TLS: %d %q", code, body)
	}
	if code, body := get(t, plainClient(), "http://"+cfg.Bind+"/"); code != 400 || strings.Contains(body, "plain") {
		t.Fatalf("plain with home_http off: %d %q, want 400 from the TLS server", code, body)
	}
}

// A peeked connection still offers CloseWrite (http.Server half-closes before
// hanging up on a refused upload): the client sees EOF and can still send.
func TestPeekedConnCloseWrite(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	raw, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var c interface{ CloseWrite() error } = &peekedConn{Conn: raw, first: 'G', unread: true}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := client.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("client read after CloseWrite = %d, %v; want EOF", n, err)
	}
	if _, err := client.Write([]byte("body")); err != nil {
		t.Fatalf("client write after the server's half-close: %v", err)
	}
}

// A peeked connection still offers io.ReaderFrom (http.Server's sendfile path
// for file responses), writing through to the connection it wraps.
func TestPeekedConnReadFrom(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	var c io.ReaderFrom = &peekedConn{Conn: a, first: 'G', unread: true}
	go func() { _, _ = c.ReadFrom(strings.NewReader("audio")) }()
	got := make([]byte, 5)
	if _, err := io.ReadFull(b, got); err != nil || string(got) != "audio" {
		t.Fatalf("read %q, %v; want audio", got, err)
	}
}

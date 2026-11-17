package launcher

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/store"
)

func TestHealthAddr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ bind, want string }{
		{bind: "0.0.0.0:8080", want: "127.0.0.1:8080"},
		{bind: ":9000", want: "127.0.0.1:9000"},
		{bind: "[::]:8443", want: "127.0.0.1:8443"},
		{bind: "192.168.1.20:8080", want: "192.168.1.20:8080"},
		{bind: "[::1]:8080", want: "[::1]:8080"},
		{bind: "localhost:8080", want: "localhost:8080"},
	} {
		cfg := config.Default(t.TempDir())
		cfg.Bind = tc.bind
		if got, err := healthAddr(cfg); err != nil || got != tc.want {
			t.Errorf("healthAddr(%s) = %q, %v; want %q", tc.bind, got, err, tc.want)
		}
	}
}

// healthDataDir writes a config.yaml naming the test server's address and mode.
func healthDataDir(t *testing.T, addr string, mode config.TLSMode) string {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default(dir)
	cfg.Bind, cfg.TLS.Mode = addr, mode
	if mode == config.TLSAutocert {
		cfg.TLS.Hosts = []string{"books.example.com"}
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func healthServer(t *testing.T, tlsOn bool, h http.HandlerFunc) string {
	t.Helper()
	var srv *httptest.Server
	if tlsOn {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func healthz(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}
}

// Healthy: /healthz answers 200 on the configured port, over plain HTTP with TLS
// off and over https (a self-signed certificate, not checked) with TLS on.
func TestHealthcheckHealthy(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []config.TLSMode{config.TLSOff, config.TLSSelfSigned} {
		addr := healthServer(t, mode != config.TLSOff, healthz(http.StatusOK))
		var out bytes.Buffer
		if err := Healthcheck(ctx, healthDataDir(t, addr, mode), &out); err != nil {
			t.Fatalf("%s: Healthcheck = %v, want healthy", mode, err)
		}
		if !strings.HasPrefix(out.String(), "ok: ") {
			t.Errorf("%s: output %q, want ok", mode, out.String())
		}
	}
}

// Unhealthy: a 503 (the database probe failed), a redirect, nothing listening,
// the wrong scheme for the port and a config that doesn't read.
func TestHealthcheckUnhealthy(t *testing.T) {
	ctx := context.Background()
	for name, dir := range map[string]string{
		"503":            healthDataDir(t, healthServer(t, false, healthz(http.StatusServiceUnavailable)), config.TLSOff),
		"redirect":       healthDataDir(t, healthServer(t, false, http.RedirectHandler("https://example.com/healthz", http.StatusFound).ServeHTTP), config.TLSOff),
		"not listening":  healthDataDir(t, freeAddr(t), config.TLSOff),
		"https on http":  healthDataDir(t, healthServer(t, false, healthz(http.StatusOK)), config.TLSSelfSigned),
		"unreadable cfg": healthDataDir(t, "no-port", config.TLSOff),
	} {
		if err := Healthcheck(ctx, dir, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: Healthcheck = nil, want unhealthy", name)
		}
	}
}

// autocertServer is a TLS listener answering as the server does under
// autocert, before any certificate is issued, and the server names of the
// hellos it was sent.
func autocertServer(t *testing.T) (addr string, names func() []string) {
	t.Helper()
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist("books.example.com"),
		Cache:      autocert.DirCache(t.TempDir()),
		Client:     &acme.Client{DirectoryURL: "http://127.0.0.1:1/directory"}, // never Let's Encrypt
	}
	var mu sync.Mutex
	var seen []string
	tlsCfg := m.TLSConfig()
	tlsCfg.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		mu.Lock()
		seen = append(seen, hello.ServerName)
		mu.Unlock()
		return m.GetCertificate(hello)
	}
	// Not httptest's TLS server: it adds a certificate of its own, which Go
	// serves to a hello naming no host, where autocert's has none.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: healthz(http.StatusOK), ErrorLog: log.New(io.Discard, "", 0), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// Under autocert the check sends a hello naming no host, which autocert
// refuses before any order: the refusal is healthy, and no hello the server
// saw named a host (one would start an issuance).
func TestHealthcheckAutocert(t *testing.T) {
	t.Parallel()
	addr, names := autocertServer(t)
	var out bytes.Buffer
	if err := Healthcheck(context.Background(), healthDataDir(t, addr, config.TLSAutocert), &out); err != nil {
		t.Fatalf("Healthcheck = %v, want healthy", err)
	}
	if got := names(); len(got) != 1 || got[0] != "" {
		t.Errorf("server names sent = %q, want one hello naming no host", got)
	}
	if !strings.HasPrefix(out.String(), "ok: ") {
		t.Errorf("output %q, want ok", out.String())
	}
}

// Under autocert: nothing listening, a plain HTTP server on the port, and a
// listener that never answers the hello are unhealthy.
func TestHealthcheckAutocertUnhealthy(t *testing.T) {
	t.Parallel()
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() }) // accepts in the kernel, never reads
	for name, addr := range map[string]string{
		"not listening": freeAddr(t),
		"plain http":    healthServer(t, false, healthz(http.StatusOK)),
		"silent":        silent.Addr().String(),
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := Healthcheck(ctx, healthDataDir(t, addr, config.TLSAutocert), io.Discard)
		cancel()
		if err == nil {
			t.Errorf("%s: Healthcheck = nil, want unhealthy", name)
		}
	}
}

// The environment wins over config.yaml, as at the server's start: AUDIOSILO_BIND
// and AUDIOSILO_TLS_MODE name the server that answers.
func TestHealthcheckEnvironment(t *testing.T) {
	dir := healthDataDir(t, freeAddr(t), config.TLSSelfSigned)
	t.Setenv("AUDIOSILO_BIND", healthServer(t, false, healthz(http.StatusOK)))
	t.Setenv("AUDIOSILO_TLS_MODE", string(config.TLSOff))
	if err := Healthcheck(context.Background(), dir, io.Discard); err != nil {
		t.Fatalf("Healthcheck = %v, want the environment's server asked", err)
	}
}

// A data folder with no config.yaml is unhealthy even when the environment
// names a server that answers: no server runs from that folder.
func TestHealthcheckNoConfig(t *testing.T) {
	t.Setenv("AUDIOSILO_BIND", healthServer(t, false, healthz(http.StatusOK)))
	t.Setenv("AUDIOSILO_TLS_MODE", string(config.TLSOff))
	err := Healthcheck(context.Background(), t.TempDir(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), config.ConfigFileName) {
		t.Fatalf("Healthcheck = %v, want no config.yaml", err)
	}
}

// A real start answers the healthcheck, and setup.skip finishes the console's
// first-run flow at start (server#184) while a start without it leaves the
// flow to the console.
func TestRunHealthcheckAndSetupSkip(t *testing.T) {
	for _, skip := range []bool{true, false} {
		data := t.TempDir()
		addr := freeAddr(t)
		cfgYAML := fmt.Sprintf("bind: %q\ntls:\n  mode: \"off\"\nupdate_check: false\nsetup:\n  skip: %t\n", addr, skip)
		if err := os.WriteFile(config.Path(data), []byte(cfgYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Run(ctx, Options{DataDir: data, Log: discardLog()}) }()
		waitFor(t, "the healthcheck", func() bool { return Healthcheck(ctx, data, io.Discard) == nil })
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not stop")
		}
		db, err := store.Open(context.Background(), filepath.Join(data, "audiosilo.db"))
		if err != nil {
			t.Fatal(err)
		}
		st, err := catalog.New(db, time.Now).SetupState(context.Background())
		_ = db.Close()
		if err != nil || st.Finished != skip || st.Dismissed != skip {
			t.Errorf("setup.skip %t: state = %+v, %v", skip, st, err)
		}
	}
}

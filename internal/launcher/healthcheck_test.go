package launcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/store"
)

func TestHealthURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		bind       string
		mode       config.TLSMode
		hosts      []string
		want, name string
	}{
		{bind: "0.0.0.0:8080", mode: config.TLSOff, want: "http://127.0.0.1:8080/healthz"},
		{bind: ":9000", mode: config.TLSOff, want: "http://127.0.0.1:9000/healthz"},
		{bind: "[::]:8443", mode: config.TLSSelfSigned, want: "https://127.0.0.1:8443/healthz"},
		{bind: "192.168.1.20:8080", mode: config.TLSSelfSigned, want: "https://192.168.1.20:8080/healthz"},
		{bind: "[::1]:8080", mode: config.TLSOff, want: "http://[::1]:8080/healthz"},
		{bind: "localhost:8080", mode: config.TLSOff, want: "http://localhost:8080/healthz"},
		{bind: "0.0.0.0:443", mode: config.TLSAutocert, hosts: []string{"books.example.com", "b.example.com"},
			want: "https://127.0.0.1:443/healthz", name: "books.example.com"},
	} {
		cfg := config.Default(t.TempDir())
		cfg.Bind, cfg.TLS.Mode, cfg.TLS.Hosts = tc.bind, tc.mode, tc.hosts
		got, name, err := healthURL(cfg)
		if err != nil || got != tc.want || name != tc.name {
			t.Errorf("healthURL(%s, %s) = %q, %q, %v; want %q, %q", tc.bind, tc.mode, got, name, err, tc.want, tc.name)
		}
	}
}

// healthDataDir writes a config.yaml naming the test server's address and mode.
func healthDataDir(t *testing.T, addr string, mode config.TLSMode) string {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default(dir)
	cfg.Bind, cfg.TLS.Mode = addr, mode
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
		if status == http.StatusFound {
			http.Redirect(w, r, "https://example.com/healthz", status)
			return
		}
		w.WriteHeader(status)
	}
}

// Healthy: /healthz answers 200 on the configured port, over plain HTTP with TLS
// off and over https (a self-signed certificate, not checked) with TLS on.
func TestHealthcheckHealthy(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		mode  config.TLSMode
		tlsOn bool
	}{{config.TLSOff, false}, {config.TLSSelfSigned, true}} {
		addr := healthServer(t, tc.tlsOn, healthz(http.StatusOK))
		var out bytes.Buffer
		if err := Healthcheck(ctx, healthDataDir(t, addr, tc.mode), &out); err != nil {
			t.Fatalf("%s: Healthcheck = %v, want healthy", tc.mode, err)
		}
		if !strings.HasPrefix(out.String(), "ok: ") {
			t.Errorf("%s: output %q, want ok", tc.mode, out.String())
		}
	}
}

// Unhealthy: a 503 (the database probe failed), a redirect, nothing listening,
// the wrong scheme for the port and a config that doesn't read.
func TestHealthcheckUnhealthy(t *testing.T) {
	ctx := context.Background()
	closed := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		return addr
	}
	for name, dir := range map[string]string{
		"503":            healthDataDir(t, healthServer(t, false, healthz(http.StatusServiceUnavailable)), config.TLSOff),
		"redirect":       healthDataDir(t, healthServer(t, false, healthz(http.StatusFound)), config.TLSOff),
		"not listening":  healthDataDir(t, closed(), config.TLSOff),
		"https on http":  healthDataDir(t, healthServer(t, false, healthz(http.StatusOK)), config.TLSSelfSigned),
		"unreadable cfg": badConfigDir(t),
	} {
		if err := Healthcheck(ctx, dir, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: Healthcheck = nil, want unhealthy", name)
		}
	}
}

func badConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default(dir)
	cfg.Bind = "no-port"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return dir
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

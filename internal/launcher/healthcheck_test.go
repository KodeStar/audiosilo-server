package launcher

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/server"
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

// A starting server (503 with server.StartingStatus) is healthy; a 503 with
// any other body (the database probe failed) is not (TestHealthcheckUnhealthy).
func TestHealthcheckStarting(t *testing.T) {
	t.Parallel()
	starting := healthServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		(&server.StartGate{}).ServeHTTP(w, r)
	})
	var out bytes.Buffer
	if err := Healthcheck(context.Background(), healthDataDir(t, starting, config.TLSOff), &out); err != nil {
		t.Fatalf("Healthcheck = %v, want a starting server healthy", err)
	}
	if !strings.HasPrefix(out.String(), "starting: ") {
		t.Errorf("output %q, want starting", out.String())
	}
	other := healthServer(t, false, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"status":"ok","error":"database unavailable"}`)
	})
	if err := Healthcheck(context.Background(), healthDataDir(t, other, config.TLSOff), io.Discard); err == nil {
		t.Error("Healthcheck of a 503 not starting = nil, want unhealthy")
	}
}

// writeCachedCert writes the certificate autocert keeps for name in cacheDir
// under key (name for ECDSA, name+"+rsa" for RSA), as autocert writes one: its
// private key's block, then the certificate's, valid until notAfter.
func writeCachedCert(t *testing.T, cacheDir, key, name string, rsaKey bool, notAfter time.Time) {
	t.Helper()
	var (
		priv   crypto.Signer
		keyPEM *pem.Block
		err    error
	)
	if rsaKey {
		k, genErr := rsa.GenerateKey(rand.Reader, 2048)
		priv, err = k, genErr
		if err == nil {
			keyPEM = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}
		}
	} else {
		k, genErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		priv, err = k, genErr
		if err == nil {
			der, mErr := x509.MarshalECPrivateKey(k)
			keyPEM, err = &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}, mErr
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-24 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		t.Fatal(err)
	}
	data := append(pem.EncodeToMemory(keyPEM), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	if err := autocert.DirCache(cacheDir).Put(context.Background(), key, data); err != nil {
		t.Fatal(err)
	}
}

// Under autocert, once a valid ECDSA certificate for a host is cached, the
// check asks /healthz naming that host (so a database that doesn't answer reads
// unhealthy) and autocert answers from its cache, ordering nothing (an order
// fails here). With none it can use (expired, about to, RSA only, another host)
// it sends only the hello naming no host.
func TestHealthcheckAutocertCachedCert(t *testing.T) {
	t.Parallel()
	const host = "books.example.com"
	day := 24 * time.Hour
	for _, tc := range []struct {
		name      string
		hosts     []string
		key       string
		certFor   string // the name on the certificate; host when empty
		rsa       bool
		notAfter  time.Duration
		wantNamed bool
	}{
		{name: "valid", hosts: []string{host}, key: host, notAfter: 60 * day, wantNamed: true},
		{name: "host in capitals", hosts: []string{"Books.Example.COM."}, key: host, notAfter: 60 * day, wantNamed: true},
		{name: "second host", hosts: []string{"other.example.com", host}, key: host, notAfter: 60 * day, wantNamed: true},
		{name: "expired", hosts: []string{host}, key: host, notAfter: -time.Hour},
		{name: "about to expire", hosts: []string{host}, key: host, notAfter: 10 * time.Minute},
		{name: "rsa only", hosts: []string{host}, key: host + "+rsa", rsa: true, notAfter: 60 * day},
		{name: "another host's", hosts: []string{host}, key: host, certFor: "other.example.com", notAfter: 60 * day},
		{name: "none for this host", hosts: []string{"other.example.com"}, key: host, notAfter: 60 * day},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			cacheDir := filepath.Join(dir, "certs")
			certFor := cmp.Or(tc.certFor, host)
			writeCachedCert(t, cacheDir, tc.key, certFor, tc.rsa, time.Now().Add(tc.notAfter))
			addr, names := autocertServer(t, cacheDir)
			cfg := config.Default(dir)
			cfg.Bind, cfg.TLS.Mode, cfg.TLS.Hosts = addr, config.TLSAutocert, tc.hosts
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := Healthcheck(context.Background(), dir, &out); err != nil {
				t.Fatalf("Healthcheck = %v, want healthy", err)
			}
			want, wantOut := []string{""}, "ok: TLS listener at "+addr
			if tc.wantNamed {
				_, port, _ := net.SplitHostPort(addr)
				want, wantOut = []string{host}, "ok: https://"+net.JoinHostPort(host, port)+"/healthz"
			}
			if got := names(); !slices.Equal(got, want) {
				t.Errorf("server names sent = %q, want %q", got, want)
			}
			if got := strings.TrimSpace(out.String()); got != wantOut {
				t.Errorf("output %q, want %q", got, wantOut)
			}
		})
	}
}

// With a cached certificate the check reaches the database probe: a 503 from
// it is unhealthy, as under the other modes.
func TestHealthcheckAutocertCachedCertUnhealthy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "certs")
	writeCachedCert(t, cacheDir, "books.example.com", "books.example.com", false, time.Now().Add(60*24*time.Hour))
	addr, _ := autocertServerWith(t, cacheDir, healthz(http.StatusServiceUnavailable))
	cfg := config.Default(dir)
	cfg.Bind, cfg.TLS.Mode, cfg.TLS.Hosts = addr, config.TLSAutocert, []string{"books.example.com"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := Healthcheck(context.Background(), dir, io.Discard); err == nil {
		t.Fatal("Healthcheck = nil, want the database probe's 503 unhealthy")
	}
}

// autocertServer is a TLS listener answering as the server does under
// autocert, its certificates cached in cacheDir, and the server names of the
// hellos it was sent. An order can't succeed (its ACME directory answers
// nothing), so a hello that starts one fails.
func autocertServer(t *testing.T, cacheDir string) (addr string, names func() []string) {
	t.Helper()
	return autocertServerWith(t, cacheDir, healthz(http.StatusOK))
}

// autocertServerWith is autocertServer serving h.
func autocertServerWith(t *testing.T, cacheDir string, h http.Handler) (addr string, names func() []string) {
	t.Helper()
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist("books.example.com"),
		Cache:      autocert.DirCache(cacheDir),
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
	srv := &http.Server{Handler: h, ErrorLog: log.New(io.Discard, "", 0), ReadHeaderTimeout: time.Second}
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
	addr, names := autocertServer(t, t.TempDir())
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

// A check that reads config.yaml while the console saves it (config.Save
// rewrites it in place: empty for a moment) reads it again rather than taking
// the defaults for the server's address.
func TestHealthcheckConfigMidSave(t *testing.T) {
	t.Parallel()
	dir := healthDataDir(t, healthServer(t, false, healthz(http.StatusOK)), config.TLSOff)
	saved, err := os.ReadFile(config.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() {
		time.Sleep(configSettle / 5)
		wrote <- os.WriteFile(config.Path(dir), saved, 0o600)
	}()
	err = Healthcheck(context.Background(), dir, io.Discard)
	if werr := <-wrote; werr != nil {
		t.Fatal(werr)
	}
	if err != nil {
		t.Fatalf("Healthcheck across a save = %v, want the saved config's server asked", err)
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

// writeOffConfig writes data's config.yaml for a plain-HTTP start on addr,
// with extra appended.
func writeOffConfig(t *testing.T, data, addr, extra string) {
	t.Helper()
	cfgYAML := fmt.Sprintf("bind: %q\ntls:\n  mode: \"off\"\nupdate_check: false\n%s", addr, extra)
	if err := os.WriteFile(config.Path(data), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
}

// waitRun waits for a stopped Run's result, which must be nil.
func waitRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// A real start answers the healthcheck, and setup.skip finishes the console's
// first-run flow at start (server#184) while a start without it leaves the
// flow to the console.
func TestRunHealthcheckAndSetupSkip(t *testing.T) {
	for _, skip := range []bool{true, false} {
		data := t.TempDir()
		addr := freeAddr(t)
		writeOffConfig(t, data, addr, fmt.Sprintf("setup:\n  skip: %t\n", skip))
		ctx, stop := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Run(ctx, Options{DataDir: data, Log: discardLog()}) }()
		waitFor(t, "the healthcheck", func() bool {
			var out bytes.Buffer
			return Healthcheck(ctx, data, &out) == nil && strings.HasPrefix(out.String(), "ok: ")
		})
		stop()
		waitRun(t, done)
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

// A start listens before it opens the database (server#217): while the
// database is still to open (the listening hook holds the start there, as a
// long migration would), every request is answered 503 "starting", which the
// healthcheck reads as healthy, and once the start finishes the server answers.
func TestRunAnswersStartingBeforeTheDatabase(t *testing.T) {
	data := t.TempDir()
	addr := freeAddr(t)
	writeOffConfig(t, data, addr, "")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	reached, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{DataDir: data, Log: discardLog(), listening: func() {
			close(reached)
			<-release
		}})
	}()
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("Run = %v before it listened", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run never listened")
	}
	if _, err := os.Stat(filepath.Join(data, "audiosilo.db")); !os.IsNotExist(err) {
		t.Fatalf("the database exists before the start reached it: %v", err)
	}

	var out bytes.Buffer
	if err := Healthcheck(ctx, data, &out); err != nil || !strings.HasPrefix(out.String(), "starting: ") {
		t.Fatalf("Healthcheck while starting = %v, %q; want healthy, starting", err, out.String())
	}
	resp, err := http.Get("http://" + addr + "/api/v1/server")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" ||
		!strings.Contains(string(body), `"status":"starting"`) {
		t.Fatalf("API while starting = %d %q (Retry-After %q), want 503 starting", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}

	close(release)
	waitFor(t, "the started server", func() bool {
		out.Reset()
		return Healthcheck(ctx, data, &out) == nil && strings.HasPrefix(out.String(), "ok: ")
	})
	stop()
	waitRun(t, done)
}

// A port another process holds stops the start before the database opens, and
// says so.
func TestRunPortTakenStopsBeforeTheDatabase(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	data := t.TempDir()
	writeOffConfig(t, data, taken.Addr().String(), "")
	reached := false
	err = Run(context.Background(), Options{DataDir: data, Log: discardLog(), listening: func() { reached = true }})
	if err == nil || !strings.Contains(err.Error(), "address already in use") && !strings.Contains(err.Error(), "bind") {
		t.Fatalf("Run on a taken port = %v, want the listen error", err)
	}
	if reached {
		t.Error("the start went on past a listen that failed")
	}
	if _, err := os.Stat(filepath.Join(data, "audiosilo.db")); !os.IsNotExist(err) {
		t.Errorf("the database was opened on a taken port: %v", err)
	}
}

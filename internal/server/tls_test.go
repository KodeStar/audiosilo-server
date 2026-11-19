package server

import (
	"bytes"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// TestTLSConfigOff asserts mode "off" yields no *tls.Config (plain HTTP).
func TestTLSConfigOff(t *testing.T) {
	cfg := &config.Config{TLS: config.TLSConfig{Mode: config.TLSOff}}
	got, err := tlsConfig(cfg)
	if err != nil {
		t.Fatalf("tlsConfig(off): %v", err)
	}
	if got != nil {
		t.Fatalf("tlsConfig(off) = %v, want nil (plain HTTP)", got)
	}
}

// TestTLSConfigSelfSigned asserts mode "selfsigned" yields a config with a
// modern MinVersion and at least one usable certificate.
func TestTLSConfigSelfSigned(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		DataDir: dir,
		TLS: config.TLSConfig{
			Mode:     config.TLSSelfSigned,
			CertFile: filepath.Join(dir, "cert.pem"),
			KeyFile:  filepath.Join(dir, "key.pem"),
		},
	}
	got, err := tlsConfig(cfg)
	if err != nil {
		t.Fatalf("tlsConfig(selfsigned): %v", err)
	}
	if got == nil {
		t.Fatal("tlsConfig(selfsigned) = nil, want non-nil config")
	}
	if got.MinVersion < tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want >= TLS 1.2 (%#x)", got.MinVersion, tls.VersionTLS12)
	}
	if len(got.Certificates) == 0 {
		t.Fatal("selfsigned config has no certificates")
	}
	// The certificate must carry a usable parsed leaf / private key.
	leaf := got.Certificates[0]
	if leaf.PrivateKey == nil {
		t.Fatal("selfsigned certificate has no private key")
	}
	if len(leaf.Certificate) == 0 {
		t.Fatal("selfsigned certificate has no DER chain")
	}
}

// TestTLSConfigAutocert asserts mode "autocert" yields a config whose
// GetCertificate is wired by the autocert manager (host policy enforced there).
func TestTLSConfigAutocert(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		DataDir: dir,
		TLS: config.TLSConfig{
			Mode:     config.TLSAutocert,
			Hosts:    []string{"example.com"},
			CacheDir: filepath.Join(dir, "certs"),
		},
	}
	got, err := tlsConfig(cfg)
	if err != nil {
		t.Fatalf("tlsConfig(autocert): %v", err)
	}
	if got == nil {
		t.Fatal("tlsConfig(autocert) = nil, want non-nil config")
	}
	if got.GetCertificate == nil {
		t.Fatal("autocert config has no GetCertificate (host policy/whitelist not wired)")
	}
}

// TestLoadOrCreateSelfSignedGenerateAndPersist verifies the first call
// generates and persists a cert/key pair to the configured paths, and that the
// key file is written with restrictive 0o600 permissions.
func TestLoadOrCreateSelfSignedGenerateAndPersist(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	cfg := &config.Config{
		DataDir: dir,
		TLS:     config.TLSConfig{Mode: config.TLSSelfSigned, CertFile: certPath, KeyFile: keyPath},
	}

	cert, err := loadOrCreateSelfSigned(cfg)
	if err != nil {
		t.Fatalf("loadOrCreateSelfSigned (first): %v", err)
	}
	if cert.Leaf == nil {
		// X509KeyPair populates Leaf on newer Go; parse defensively if not.
		if len(cert.Certificate) == 0 {
			t.Fatal("generated certificate has no DER chain")
		}
	}

	// Both files must have been persisted.
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("cert file not persisted: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("key file not persisted: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file perms = %o, want 0600 (private key must not be world/group readable)", perm)
	}
}

// TestLoadOrCreateSelfSignedReusesPersisted verifies a second call reuses the
// persisted pair rather than generating a fresh certificate.
func TestLoadOrCreateSelfSignedReusesPersisted(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		DataDir: dir,
		TLS: config.TLSConfig{
			Mode:     config.TLSSelfSigned,
			CertFile: filepath.Join(dir, "cert.pem"),
			KeyFile:  filepath.Join(dir, "key.pem"),
		},
	}

	first, err := loadOrCreateSelfSigned(cfg)
	if err != nil {
		t.Fatalf("loadOrCreateSelfSigned (first): %v", err)
	}
	second, err := loadOrCreateSelfSigned(cfg)
	if err != nil {
		t.Fatalf("loadOrCreateSelfSigned (second): %v", err)
	}

	// Reuse means the identical leaf DER bytes come back the second time. A
	// freshly generated cert would have a different (random) serial and DER.
	firstLeaf, err := leafDER(first)
	if err != nil {
		t.Fatalf("first leaf: %v", err)
	}
	secondLeaf, err := leafDER(second)
	if err != nil {
		t.Fatalf("second leaf: %v", err)
	}
	if string(firstLeaf) != string(secondLeaf) {
		t.Fatal("second call regenerated the certificate; expected the persisted pair to be reused")
	}
}

// leafDER returns the DER bytes of a certificate's leaf for byte comparison.
func leafDER(c tls.Certificate) ([]byte, error) {
	if len(c.Certificate) == 0 {
		return nil, errors.New("certificate has no leaf DER")
	}
	return c.Certificate[0], nil
}

// lockedBuffer is a bytes.Buffer the server's goroutines may write while the
// test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A TLS hello naming no host (the healthcheck's under autocert) is refused
// with an alert, and the line net/http logs for it is the one quietNoSNI
// drops, while it keeps every other line.
func TestAutocertNoSNILogDropped(t *testing.T) {
	tlsCfg, err := tlsConfig(&config.Config{TLS: config.TLSConfig{
		Mode: config.TLSAutocert, Hosts: []string{"books.example.com"}, CacheDir: t.TempDir(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	var raw lockedBuffer
	srv := &http.Server{Handler: http.NotFoundHandler(), ErrorLog: log.New(&raw, "", log.LstdFlags), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	err = tls.Client(conn, &tls.Config{InsecureSkipVerify: true}).Handshake() //nolint:gosec // a test hello
	_ = conn.Close()
	if err == nil || !strings.Contains(err.Error(), "remote error") {
		t.Fatalf("handshake = %v, want a TLS alert", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(raw.String(), noSNIRefusal) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q logged; log: %q", noSNIRefusal, raw.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	var out bytes.Buffer
	// A server bound to 192.0.2.7: its healthcheck asks from that address.
	quiet := autocertErrorLog("192.0.2.7:443").Writer().(quietNoSNI)
	quiet.w = &out
	if _, err := quiet.Write([]byte(raw.String())); err != nil || out.Len() != 0 {
		t.Errorf("quietNoSNI passed %q (%v), want it dropped", out.String(), err)
	}
	if _, err := quiet.Write([]byte("2026/11/17 10:00:00 http: TLS handshake error from [::1]:5000: " + noSNIRefusal + "\n")); err != nil || out.Len() != 0 {
		t.Errorf("quietNoSNI passed %q (%v) from ::1, want it dropped", out.String(), err)
	}
	if _, err := quiet.Write([]byte("2026/11/17 10:00:00 http: TLS handshake error from 192.0.2.7:5000: " + noSNIRefusal + "\n")); err != nil || out.Len() != 0 {
		t.Errorf("quietNoSNI passed %q (%v) from the bind address, want it dropped", out.String(), err)
	}
	// Every other line is kept, the same refusal from an address off this
	// machine included (something on the network asking without a name).
	for _, other := range []string{
		"2026/11/17 10:00:00 http: TLS handshake error from 127.0.0.1:5000: EOF\n",
		"2026/11/17 10:00:00 http: TLS handshake error from 203.0.113.5:5000: " + noSNIRefusal + "\n",
		"2026/11/17 10:00:00 http: TLS handshake error from [2001:db8::1]:5000: " + noSNIRefusal + "\n",
		"2026/11/17 10:00:00 something else: " + noSNIRefusal + "\n",
	} {
		out.Reset()
		if _, err := quiet.Write([]byte(other)); err != nil || out.String() != other {
			t.Errorf("quietNoSNI wrote %q (%v), want %q", out.String(), err, other)
		}
	}
}

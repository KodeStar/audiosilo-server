package server

import (
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"testing"

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

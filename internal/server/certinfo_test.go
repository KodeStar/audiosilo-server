package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
)

func TestCertificatesSelfSigned(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, TLS: config.TLSConfig{Mode: config.TLSSelfSigned}}
	if _, err := Certificates(cfg); err == nil {
		t.Fatal("no certificate file yet must be an error, not a made-up certificate")
	}
	if _, err := tlsConfig(cfg); err != nil { // writes the pair, as at start
		t.Fatal(err)
	}
	certs, err := Certificates(cfg)
	if err != nil || len(certs) != 1 {
		t.Fatalf("Certificates = %v, %v", certs, err)
	}
	c := certs[0]
	if !c.Issued || !c.SelfSigned || c.Subject != "AudioSilo" || c.NotAfter.Before(time.Now().AddDate(9, 0, 0)) {
		t.Fatalf("self-signed certificate = %+v", c)
	}
}

func TestCertificatesAutocertAndOff(t *testing.T) {
	cache := t.TempDir()
	cfg := &config.Config{DataDir: t.TempDir(), TLS: config.TLSConfig{
		Mode: config.TLSAutocert, Hosts: []string{"a.example.com", "b.example.com"}, CacheDir: cache,
	}}
	// Copy a real certificate in as a.example.com's cache entry.
	src := &config.Config{DataDir: t.TempDir(), TLS: config.TLSConfig{Mode: config.TLSSelfSigned}}
	if _, err := tlsConfig(src); err != nil {
		t.Fatal(err)
	}
	pem, _ := os.ReadFile(filepath.Join(src.DataDir, "selfsigned-cert.pem"))
	if err := os.WriteFile(filepath.Join(cache, "a.example.com"), pem, 0o600); err != nil {
		t.Fatal(err)
	}
	certs, err := Certificates(cfg)
	if err != nil || len(certs) != 2 {
		t.Fatalf("Certificates = %v, %v", certs, err)
	}
	if certs[0].Host != "a.example.com" || !certs[0].Issued {
		t.Errorf("cached host = %+v", certs[0])
	}
	if certs[1].Host != "b.example.com" || certs[1].Issued || certs[1].DNSNames == nil {
		t.Errorf("a host with no certificate yet = %+v", certs[1])
	}

	off := &config.Config{TLS: config.TLSConfig{Mode: config.TLSOff}}
	if certs, err := Certificates(off); err != nil || certs != nil {
		t.Fatalf("mode off has no certificates: %v %v", certs, err)
	}
}

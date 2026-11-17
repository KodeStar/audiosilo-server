package launcher

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// healthcheckTimeout bounds one healthcheck: the server's /healthz answers within
// its own two-second database probe, so this leaves room for a slow handshake and
// still ends well inside the image's ten-second HEALTHCHECK timeout (the
// Dockerfile's --timeout; Docker's own default is thirty).
const healthcheckTimeout = 5 * time.Second

// Healthcheck asks the server running from dataDir whether it is healthy: a GET
// of /healthz on the address config.yaml and the AUDIOSILO_* environment give
// it (bind and tls.mode, read as the server reads them at start), answered 200.
// It backs `audiosilo healthcheck`, the Docker image's HEALTHCHECK (server#217):
// nil is healthy, an error says why not. It opens nothing in the data folder
// but config.yaml, writes nothing and needs no curl in the image. A data folder
// without a config.yaml is unhealthy: a server saves one before it listens, so
// none there means no server is running from that folder (a --data that isn't
// the server's), never the defaults guessed at.
//
// An unspecified bind host (0.0.0.0, ::, empty) is asked on 127.0.0.1. With TLS
// on (selfsigned, autocert) it speaks https without checking the certificate:
// it is the server asking itself, it sends nothing secret, and a self-signed
// certificate must not make a live server look dead. Under autocert the hello
// names tls.hosts' first host, since autocert answers no handshake without a
// name it may serve; until that host's certificate is issued autocert refuses
// the handshake (and tries the issuance again, at most once a minute), so the
// check fails until Let's Encrypt can reach the server. A bind or TLS change
// saved in the console takes effect at the next start, so until then this asks
// the new address (the saved one) and fails: a restart applies it.
func Healthcheck(ctx context.Context, dataDir string, w io.Writer) error {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	cfg, firstRun, err := config.Load(abs)
	if err != nil {
		return fmt.Errorf("reading the config: %w", err)
	}
	if firstRun {
		return fmt.Errorf("no %s: no server has started from %s", config.Path(abs), abs)
	}
	target, serverName, err := healthURL(cfg)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: healthcheckTimeout,
		Transport: &http.Transport{
			Proxy: nil, // never through an HTTP(S)_PROXY the container sets: this is the server asking itself
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // the server's own loopback probe; see the doc comment
				ServerName:         serverName,
			},
			DisableKeepAlives: true,
		},
		// A redirect (home HTTP's https redirect, a proxy's) is not the answer.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("no answer from %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", target, resp.Status)
	}
	_, _ = fmt.Fprintln(w, "ok:", target)
	return nil
}

// healthURL is the /healthz URL the server cfg describes listens on, with the
// TLS server name to send (autocert's first host, else none).
func healthURL(cfg *config.Config) (target, serverName string, err error) {
	host, port, err := net.SplitHostPort(cfg.Bind)
	if err != nil {
		return "", "", fmt.Errorf("invalid bind address %q: %w", cfg.Bind, err)
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	scheme := "https"
	switch cfg.TLS.Mode {
	case config.TLSOff:
		scheme = "http"
	case config.TLSAutocert:
		if len(cfg.TLS.Hosts) > 0 {
			serverName = cfg.TLS.Hosts[0]
		}
	}
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port), Path: "/healthz"}
	return u.String(), serverName, nil
}

package launcher

import (
	"context"
	"crypto/tls"
	"errors"
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
// An unspecified bind host (0.0.0.0, ::, empty) is asked on 127.0.0.1. With
// tls.mode selfsigned it speaks https without checking the certificate: it is
// the server asking itself, it sends nothing secret, and a self-signed
// certificate must not make a live server look dead.
//
// Under autocert it never asks /healthz: a handshake naming one of tls.hosts
// starts an issuance whenever that host has no valid certificate, and every
// failed one counts against Let's Encrypt's failed-validation limit, while the
// check would read unhealthy for a state no restart fixes. Instead it connects
// to the listener and sends a TLS hello naming no host, which autocert refuses
// at once, before its cache or any order; the refusal (a TLS alert) or a
// finished handshake is healthy, since the server accepted the connection and
// answered the hello. That proves the server is up and serving, not that its
// database answers.
//
// A bind or TLS change saved in the console takes effect at the next start, so
// until then this asks the new address (the saved one) and fails: a restart
// applies it.
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
	addr, err := healthAddr(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	if cfg.TLS.Mode == config.TLSAutocert {
		if err := helloWithoutName(ctx, addr); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w, "ok: TLS listener at", addr)
		return nil
	}
	return getHealthz(ctx, addr, cfg.TLS.Mode != config.TLSOff, w)
}

// getHealthz asks http(s)://addr/healthz for a 200.
func getHealthz(ctx context.Context, addr string, tlsOn bool, w io.Writer) error {
	scheme := "http"
	if tlsOn {
		scheme = "https"
	}
	target := (&url.URL{Scheme: scheme, Host: addr, Path: "/healthz"}).String()
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: nil, // never through an HTTP(S)_PROXY the container sets: this is the server asking itself
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // the server's own loopback probe; see Healthcheck
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

// helloWithoutName connects to addr and sends a TLS hello with no server name
// (see Healthcheck): nil when the handshake finishes or the server refuses it
// with a TLS alert, an error when nothing listens, nothing answers or what
// answers doesn't speak TLS.
func helloWithoutName(ctx context.Context, addr string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("no answer from %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	// No ServerName, and tls.Client never takes one from the address: no SNI.
	err = tls.Client(conn, &tls.Config{InsecureSkipVerify: true}).HandshakeContext(ctx) //nolint:gosec // nothing is sent past the hello
	if alert := (*net.OpError)(nil); err == nil || errors.As(err, &alert) && alert.Op == "remote error" {
		return nil
	}
	return fmt.Errorf("no TLS answer from %s: %w", addr, err)
}

// healthAddr is the host:port the server cfg describes listens on, an
// unspecified host asked on 127.0.0.1.
func healthAddr(cfg *config.Config) (string, error) {
	host, port, err := net.SplitHostPort(cfg.Bind)
	if err != nil {
		return "", fmt.Errorf("invalid bind address %q: %w", cfg.Bind, err)
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

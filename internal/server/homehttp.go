package server

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// Home HTTP (config.ServesHomeHTTP): a self-signed server's port answers plain
// HTTP as well as TLS, because the native apps can't trust a self-signed
// certificate. splitListener tells the two apart per connection, and
// homeHTTPGate serves plain HTTP only to clients on the home network.

// tlsRecordHandshake is the first byte of every TLS connection: the record type
// of the ClientHello. No plain HTTP request starts with it (a method is letters).
const tlsRecordHandshake = 0x16

// splitListener is one TCP listener for both protocols. It accepts connections
// and, each in its own goroutine so a silent client never holds up the others,
// reads the first byte: a TLS handshake record makes the connection a TLS server
// connection (http.Server serves it as https), anything else stays plain. The
// read is bounded by peek; a connection that sends nothing in time is closed.
type splitListener struct {
	net.Listener                 // the TCP listener: Addr, and the connections
	tls          *tls.Config     // the server's certificate, for the TLS ones
	peek         time.Duration   // how long a connection may take to send its first byte
	conns        chan accepted   // classified connections (or accept errors), for Accept
	ctx          context.Context // done once Close is called
	cancel       context.CancelFunc
}

// accepted is one result of the TCP listener, classified.
type accepted struct {
	conn net.Conn
	err  error
}

// newSplitListener starts splitting ln's connections; Close stops it.
func newSplitListener(ln net.Listener, cfg *tls.Config, peek time.Duration) *splitListener {
	ctx, cancel := context.WithCancel(context.Background())
	l := &splitListener{Listener: ln, tls: cfg, peek: peek, conns: make(chan accepted), ctx: ctx, cancel: cancel}
	go l.acceptLoop()
	return l
}

// Accept returns the next classified connection: a *tls.Conn for a TLS client,
// else the plain connection. After Close it returns net.ErrClosed.
func (l *splitListener) Accept() (net.Conn, error) {
	select {
	case a := <-l.conns:
		return a.conn, a.err
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

// Close stops accepting and closes the connections still being classified.
func (l *splitListener) Close() error {
	l.cancel()
	return l.Listener.Close()
}

// acceptLoop accepts until Close. An accept error other than the listener being
// closed goes to Accept, so http.Server decides as it would on its own listener:
// it backs off before asking again on a temporary one (and the hand-over waits
// for it to ask, so the loop never spins), and stops, closing this listener, on
// any other.
func (l *splitListener) acceptLoop() {
	for {
		c, err := l.Listener.Accept()
		if err == nil {
			go l.classify(c)
			continue
		}
		if l.ctx.Err() != nil {
			return
		}
		select {
		case l.conns <- accepted{err: err}:
		case <-l.ctx.Done():
			return
		}
	}
}

// classify reads c's first byte and hands c to Accept as TLS or plain.
func (l *splitListener) classify(c net.Conn) {
	// Close (a shutdown) closes a connection still on its first byte.
	stop := context.AfterFunc(l.ctx, func() { _ = c.Close() })
	var first [1]byte
	_ = c.SetReadDeadline(time.Now().Add(l.peek))
	_, err := io.ReadFull(c, first[:])
	_ = c.SetReadDeadline(time.Time{})
	if !stop() || err != nil {
		_ = c.Close()
		return
	}
	var conn net.Conn = &peekedConn{Conn: c, first: first[0], unread: true}
	if first[0] == tlsRecordHandshake {
		conn = tls.Server(conn, l.tls)
	}
	select {
	case l.conns <- accepted{conn: conn}:
	case <-l.ctx.Done():
		_ = c.Close()
	}
}

// peekedConn is a connection whose first byte was read to classify it: Read
// returns that byte first, then reads on.
type peekedConn struct {
	net.Conn
	first  byte
	unread bool
}

func (c *peekedConn) Read(p []byte) (int, error) {
	if c.unread && len(p) > 0 {
		p[0], c.unread = c.first, false
		return 1, nil
	}
	return c.Conn.Read(p)
}

// ReadFrom writes r to the connection through the TCP connection's own ReadFrom,
// so a plain-HTTP file response (audio streaming) keeps the kernel's sendfile
// path: http.Server looks for io.ReaderFrom on the connection it was handed.
func (c *peekedConn) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(c.Conn, r)
}

// CloseWrite half-closes the TCP connection. http.Server looks for it on the
// connection it was handed before hanging up on a plain-HTTP client still
// sending a body it refused (a 413 on an over-limit upload, a 431): the client
// then reads the response instead of a reset that can drop it.
func (c *peekedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// homeHTTPGate serves a request that came over TLS, or over plain HTTP from home
// (homeRequest); any other plain HTTP request is redirected to the same URL on
// https (308, so a POST stays a POST), or refused with a 400 when its Host can't
// make that URL.
func homeHTTPGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || homeRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		target, ok := httpsURL(r)
		if !ok {
			http.Error(w, "This server needs HTTPS.", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
}

// homeRequest reports whether a plain HTTP request comes from home: from this
// machine (loopback), or from a home-network address (config.HomeNetworkIP) that
// names the server by a home-network host (config.HomeNetworkHost). The address
// is the connection's own, never a forwarding header, which the client writes
// itself. The Host check keeps an internet client on https when a NAT or proxy
// gateway hands its connection over from a private address (Docker's IPv6 proxy,
// a router rewriting a forwarded port's source): it names the server by its
// public name or address.
func homeRequest(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	return ip.IsLoopback() || config.HomeNetworkIP(ip) && config.HomeNetworkHost(r.Host)
}

// httpsURL is r's URL on https: the same Host and port, which answers TLS too
// (a Host without one came in on 80, as api.baseURL's links say), and request URI. ok is false when the Host is missing or
// isn't a plain host[:port], or the request isn't for a path.
func httpsURL(r *http.Request) (target string, ok bool) {
	if r.Host == "" || !strings.HasPrefix(r.URL.Path, "/") {
		return "", false
	}
	u, err := url.Parse("https://" + r.Host + "/")
	if err != nil || u.Host != r.Host || u.User != nil || u.Hostname() == "" || u.Path != "/" {
		return "", false
	}
	return "https://" + config.HostWithPort(r.Host, "80") + r.URL.RequestURI(), true
}

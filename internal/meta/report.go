package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// The report of unknown Audible ASINs (LibraForge C3, internal/coverage): a
// lookup that tells "the community database doesn't know it" from "the lookup
// failed", and the POST to metaserve's intake queue. The POST goes to the
// remote service only (never the mirror's local copy, which has no intake) and
// carries the ASINs and nothing else.

// ErrReportNotOffered is the service saying it takes no reports: 404 (an older
// metaserve, or intake off), 405, a redirect (never followed), or a 2xx that is
// not an intake answer. The job then stays quiet for a week.
var ErrReportNotOffered = errors.New("meta: the metadata service takes no reports")

// ErrReportNeedsHTTPS is a base_url the report is never sent to: plain http to
// anything but this machine (the address would cross the network unencrypted,
// and a plain-http front door may log it).
var ErrReportNeedsHTTPS = errors.New("meta: the report needs an https metadata URL")

// reportPath is metaserve's intake route.
const reportPath = "/api/v1/intake/unknown-asins"

// reportUserAgent is the report's whole User-Agent: no version, no server.
const reportUserAgent = "AudioSilo"

// maxReportAnswer bounds what is read of an intake answer.
const maxReportAnswer = 64 << 10

// ReportAnswer is metaserve's 202: how many of the ASINs it queued, how many it
// already knew and how many it refused as malformed.
type ReportAnswer struct {
	Accepted int `json:"accepted"`
	Known    int `json:"known"`
	Invalid  int `json:"invalid"`
}

// newReportClient is the intake POST's client: its own connections (as
// newClient's), the client's timeout, and no redirect followed (a 3xx is the
// answer, read as ErrReportNotOffered), so a POST can never be turned into a
// request elsewhere or downgraded to http. SetMirror never touches it.
func newReportClient() *http.Client {
	return &http.Client{
		Timeout:       clientTimeout,
		Transport:     ownTransport(http.DefaultTransport),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// CanReport reports whether base_url may receive the report: https, or plain
// http to a loopback host (a local metaserve, the tests).
func (s *Service) CanReport() bool { return reportAllowed(s.baseURL) }

func reportAllowed(base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

// KnownASIN asks the community database whether any recording has asin (GET
// /api/v1/lookup, through the client: the local copy in mirror mode). Only a
// 404 is "unknown" (false, nil); any other failure is an error, never an
// answer. A memory-cached hit counts as known without asking.
func (s *Service) KnownASIN(ctx context.Context, asin string) (bool, error) {
	asin = match.NormalizeASIN(asin)
	if !match.ValidASIN(asin) {
		return false, fmt.Errorf("meta: %q is not an ASIN", asin)
	}
	if w, hit := s.cachedWorkID(cacheKey(asin, "")); hit && !w.Failed && w.ID != "" {
		return true, nil
	}
	_, err := s.client.lookup(ctx, asin, "")
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	}
	return false, err
}

// ReportUnknownASINs POSTs asins (already upper-cased and in the order to send)
// to the remote service's intake: {"asins": [...]} with Content-Type
// application/json and a bare User-Agent, nothing else of this server. A 202
// with an intake answer is the answer; ErrReportNotOffered and
// ErrReportNeedsHTTPS as above; anything else (429, 5xx, a transport failure)
// is an error the caller retries another day.
func (s *Service) ReportUnknownASINs(ctx context.Context, asins []string) (ReportAnswer, error) {
	if !s.CanReport() {
		return ReportAnswer{}, ErrReportNeedsHTTPS
	}
	body, err := json.Marshal(struct {
		ASINs []string `json:"asins"`
	}{asins})
	if err != nil {
		return ReportAnswer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+reportPath, bytes.NewReader(body))
	if err != nil {
		return ReportAnswer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", reportUserAgent)
	resp, err := s.report.Do(req)
	if err != nil {
		return ReportAnswer{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		_ = resp.Body.Close()
	}()
	switch code := resp.StatusCode; {
	case code == http.StatusNotFound || code == http.StatusMethodNotAllowed || (code >= 300 && code < 400):
		return ReportAnswer{}, ErrReportNotOffered
	case code < 200 || code >= 300:
		return ReportAnswer{}, &statusError{code: code, path: reportPath}
	}
	// A 2xx: an intake answer names "accepted"; anything else (a site's HTML,
	// a proxy's page) is not the intake.
	var ans struct {
		Accepted *int `json:"accepted"`
		Known    int  `json:"known"`
		Invalid  int  `json:"invalid"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxReportAnswer)).Decode(&ans) != nil || ans.Accepted == nil {
		return ReportAnswer{}, ErrReportNotOffered
	}
	if resp.StatusCode != http.StatusAccepted {
		return ReportAnswer{}, &statusError{code: resp.StatusCode, path: reportPath}
	}
	return ReportAnswer{Accepted: *ans.Accepted, Known: ans.Known, Invalid: ans.Invalid}, nil
}

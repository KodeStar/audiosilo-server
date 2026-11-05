package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// The unknown-ASIN report's setting and status in GET/PATCH /admin/settings,
// and its gate (metadata on AND the switch, never a demo server).
func TestReportUnknownSettings(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	tok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	metadata := func(body string) map[string]json.RawMessage {
		t.Helper()
		var env struct {
			Metadata map[string]json.RawMessage `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatal(err)
		}
		return env.Metadata
	}

	resp, body := e.do(t, "GET", "/api/v1/admin/settings", tok, "")
	m := metadata(body)
	if resp.StatusCode != http.StatusOK || string(m["report_unknown"]) != "false" ||
		string(m["report_status"]) != `{"state":"never","last_sent":0}` {
		t.Fatalf("settings = %d %s", resp.StatusCode, body)
	}
	if e.api.reportUnknownOn() {
		t.Fatal("the report is on by default")
	}

	resp, body = e.do(t, "PATCH", "/api/v1/admin/settings", tok, `{"metadata":{"report_unknown":true}}`)
	if resp.StatusCode != http.StatusOK || string(metadata(body)["report_unknown"]) != "true" || !e.api.reportUnknownOn() {
		t.Fatalf("switch on = %d %s", resp.StatusCode, body)
	}
	if err := e.cat.SaveUnknownASINReportState(ctx, catalog.UnknownASINReport{State: "not_offered",
		LastRunAt: "2026-11-05T04:00:00.000Z", LastSent: 0, QuietUntil: "2026-11-12T04:00:00.000Z"}); err != nil {
		t.Fatal(err)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/settings", tok, "")
	if got := string(metadata(body)["report_status"]); got != `{"state":"not_offered","last_run_at":"2026-11-05T04:00:00.000Z","last_sent":0}` {
		t.Fatalf("report_status = %s", got)
	}

	if r, b := e.do(t, "PATCH", "/api/v1/admin/settings", tok, `{"metadata":{"enabled":false}}`); r.StatusCode != http.StatusOK {
		t.Fatalf("metadata off = %d %s", r.StatusCode, b)
	}
	if e.api.reportUnknownOn() {
		t.Fatal("the report is on with metadata off")
	}

	// Only an admin changes it.
	if r, _ := e.do(t, "PATCH", "/api/v1/admin/settings", "", `{"metadata":{"report_unknown":false}}`); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous PATCH = %d", r.StatusCode)
	}
}

// A demo server never builds the report, whatever the switch says.
func TestReportUnknownNeverOnDemo(t *testing.T) {
	t.Parallel()
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Demo.Enabled = true
		c.Metadata.ReportUnknown = true
	})
	if e.api.coverage != nil || e.api.reportUnknownOn() {
		t.Fatal("a demo server has the report")
	}
	e.api.StartCoverage(context.Background()) // nothing to start
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	_, body := e.do(t, "GET", "/api/v1/admin/settings", tok, "")
	var env struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.Metadata["report_status"]; ok {
		t.Fatalf("a demo server reports a status: %s", body)
	}
}

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

// heartbeatWith is a heartbeat body mapping n libraries, the env's first and
// then ids the server doesn't have, each as a Bridge sends it (a mount path
// and a checked book), about 200 bytes a library.
func (b *bridgeEnv) heartbeatWith(n int) string {
	libs := make([]string, n)
	for i := range libs {
		id := b.lib.ID
		if i > 0 {
			id = int64(100000 + i)
		}
		libs[i] = fmt.Sprintf(`{"library_id":%d,"mount_path":"/mnt/user/media/audiobooks/library-%06d","mapped":true,"writable":true,"checked_path":"Some Author/Some Series/Book %06d - A Long Enough Title.m4b","error":""}`, id, i, i)
	}
	return `{"version":"0.6.0","kinds":["bridge.verify"],"libraries":[` + strings.Join(libs, ",") + `],"workers":[],"ui_url":""}`
}

// A heartbeat mapping more libraries than it used to hold (512 of them, or
// 64 KiB of body: server#445) is accepted, and a list past the cap is cut to
// it rather than refused, so a Bridge on a server with thousands of libraries
// never reads as offline; a later, shorter one is accepted too.
func TestBridgeHeartbeatManyLibraries(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	for _, n := range []int{600, 5000, 10} {
		body := b.heartbeatWith(n)
		resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("heartbeat of %d libraries (%d bytes): %d %s", n, len(body), resp.StatusCode, out)
		}
		resp, out = b.do(t, "GET", "/api/v1/admin/bridge", b.adminTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("admin bridge: %d %s", resp.StatusCode, out)
		}
		var got adminBridge
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Online || len(got.Libraries) != 1 || got.Libraries[0].Mapping == nil || !got.Libraries[0].Mapping.Mapped {
			t.Fatalf("after %d libraries: online %v, libraries %+v", n, got.Online, got.Libraries)
		}
		// Only the mapping of the library the server has is stored: the row a
		// heartbeat rewrites, and GET /server reads, stays small whatever the
		// Bridge lists.
		var stored string
		if err := b.db.QueryRowContext(context.Background(), `SELECT status FROM bridge_status WHERE id = 1`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if len(stored) > 1024 || strings.Count(stored, `"library_id"`) != 1 {
			t.Fatalf("after %d libraries the stored status is %d bytes: %.300s", n, len(stored), stored)
		}
	}
}

// A heartbeat whose library list can't be read is still saved, as sent: a
// catalog read fault answers the beat with the fault but never takes a
// healthy Bridge offline (its mappings stay as Validate bounded them).
func TestBridgeHeartbeatSavedWhenLibrariesUnreadable(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	if _, err := b.db.ExecContext(ctx, `UPDATE bridge_status SET heartbeat_at = '2000-01-01T00:00:00Z', status = '{}' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(ctx, `ALTER TABLE libraries RENAME TO libraries_away`); err != nil {
		t.Fatal(err)
	}
	resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, b.heartbeatWith(2))
	if _, err := b.db.ExecContext(ctx, `ALTER TABLE libraries_away RENAME TO libraries`); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("heartbeat with the libraries unreadable = %d %s, want the read's fault", resp.StatusCode, out)
	}
	var at, stored string
	if err := b.db.QueryRowContext(ctx, `SELECT heartbeat_at, status FROM bridge_status WHERE id = 1`).Scan(&at, &stored); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(at, "2000") || strings.Count(stored, `"library_id"`) != 2 {
		t.Fatalf("the beat was not saved as sent: at %s, status %.300s", at, stored)
	}
}

// A heartbeat refused for its size (the body, a list over its bound) is 400
// too_large, so the Bridge tells it apart from a block this server doesn't
// read; a malformed body, or a field this server doesn't know, stays a plain
// 400 with no code.
func TestBridgeHeartbeatTooLarge(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	code := func(body string) (int, string) {
		t.Helper()
		resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, body)
		var env struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(out), &env)
		return resp.StatusCode, env.Code
	}
	kinds := make([]string, 65)
	for i := range kinds {
		kinds[i] = fmt.Sprintf(`"bridge.k%d"`, i)
	}
	workers := "[" + strings.Repeat(`{"name":"w"},`, 64) + `{"name":"w"}]`
	for name, body := range map[string]string{
		"body":    `{"version":"` + strings.Repeat("x", bridge.MaxStatusBytes) + `"}`,
		"kinds":   `{"version":"0.6.0","kinds":[` + strings.Join(kinds, ",") + `]}`,
		"workers": `{"version":"0.6.0","workers":` + workers + `}`,
	} {
		if status, c := code(body); status != http.StatusBadRequest || c != codeTooLarge {
			t.Errorf("too large a %s = %d %q, want 400 %s", name, status, c, codeTooLarge)
		}
	}
	for name, body := range map[string]string{
		"malformed":     `{"version":`,
		"unknown field": `{"version":"0.6.0","setup2":{}}`,
		"bad kind":      `{"version":"0.6.0","kinds":["Not A Kind"]}`,
	} {
		if status, c := code(body); status != http.StatusBadRequest || c != "" {
			t.Errorf("%s = %d %q, want a plain 400", name, status, c)
		}
	}
	if status, c := code(b.heartbeatWith(1)); status != http.StatusOK {
		t.Fatalf("a heartbeat in bounds = %d %q", status, c)
	}
}

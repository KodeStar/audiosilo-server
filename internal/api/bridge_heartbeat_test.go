package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
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
	}
}

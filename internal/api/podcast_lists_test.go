package api

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

// A whole-list replace from a player that can't see podcasts (2.1.x) keeps the
// episodes on the list; one from a player that sees them still deletes what it
// leaves out.
func TestPodcastListReplaceKeepsEpisodesForOldPlayers(t *testing.T) {
	b, _, f := podcastEnv(t, false)
	newer := podcastHeaders()
	released := map[string]string{auth.ClientHeader: "AudioSilo/2.2.0 (ios)"}
	old := map[string]string{auth.ClientHeader: "AudioSilo/2.1.0 (ios)"}
	item := func(i int) string {
		return fmt.Sprintf(`{"library_id":%d,"path":%q}`, b.lib.ID, f.Episodes[i].Path)
	}
	call := func(method, path, body string, headers map[string]string) string {
		t.Helper()
		resp, out := b.doHeaders(t, method, path, b.userTok, body, headers)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s %d %s", method, path, resp.StatusCode, out)
		}
		return out
	}
	has := func(what, body string, want ...bool) {
		t.Helper()
		for i, w := range want {
			if strings.Contains(body, strconv.Quote(f.Episodes[i].Path)) != w {
				t.Fatalf("%s: episode %d listed=%t, want %t: %s", what, i, !w, w, body)
			}
		}
	}

	t.Run("queue", func(t *testing.T) {
		call("POST", "/api/v1/me/queue", item(0), newer)
		call("POST", "/api/v1/me/queue", item(1), newer)
		has("old player's answer", call("PUT", "/api/v1/me/queue", `{"items":[]}`, old), false, false)
		has("after the old player's replace", call("GET", "/api/v1/me/queue", "", newer), true, true)
		call("PUT", "/api/v1/me/queue", `{"items":[`+item(1)+`]}`, released)
		has("after the released player's replace", call("GET", "/api/v1/me/queue", "", newer), false, true)
	})

	t.Run("collection", func(t *testing.T) {
		col := b.createCollection(t, b.userTok, "Mixed")
		items := colURL(col.ID, "/items")
		call("POST", items, item(0), newer)
		has("old player's answer", call("PUT", items, `{"items":[]}`, old), false)
		has("after the old player's replace", call("GET", colURL(col.ID, ""), "", newer), true)
		call("PUT", items, `{"items":[]}`, released)
		has("after the released player's replace", call("GET", colURL(col.ID, ""), "", newer), false)
	})
}

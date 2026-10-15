package bridge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStatusValidate(t *testing.T) {
	t.Parallel()
	good := Status{Version: "0.1.0", Kinds: []string{KindVerify, "sidecars.generate"},
		Libraries: []LibraryStatus{{LibraryID: 1, MountPath: "/media/books", Mapped: true}},
		UIURL:     "http://192.168.1.10:8095/"}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid status: %v", err)
	}
	bad := map[string]Status{
		"javascript link":  {UIURL: "javascript:alert(1)"},
		"no host":          {UIURL: "http:///x"},
		"credentials":      {UIURL: "http://user:pw@host/"},
		"bad kind":         {Kinds: []string{"Bridge.Verify"}},
		"kind twice":       {Kinds: []string{KindVerify, KindVerify}},
		"library twice":    {Libraries: []LibraryStatus{{LibraryID: 1}, {LibraryID: 1}}},
		"library zero":     {Libraries: []LibraryStatus{{LibraryID: 0}}},
		"control in error": {Libraries: []LibraryStatus{{LibraryID: 1, Error: "a\nb"}}},
		"long version":     {Version: strings.Repeat("v", maxShortText+1)},
	}
	for name, st := range bad {
		if err := st.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

func TestFeaturesAreKnownFamilies(t *testing.T) {
	t.Parallel()
	st := Status{Kinds: []string{"sidecars.generate", KindVerify, "bogus.kind"}}
	// sidecars.generate isn't known to the server in Phase 0, so it's not offered.
	if got := st.Features(); !slices.Equal(got, []string{"bridge"}) {
		t.Fatalf("Features = %v", got)
	}
}

func TestHeartbeatOnlineWindow(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	if err := q.SaveHeartbeat(ctx, Status{}); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("heartbeat before pairing: %v, want ErrNotPaired", err)
	}
	if _, err := q.db.ExecContext(ctx, `INSERT INTO bridge_status(id, paired_at) VALUES(1, '2026-10-15T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if h, err := q.Online(ctx); err != nil || h != nil {
		t.Fatalf("paired, no heartbeat: %v, %v", h, err)
	}
	st := Status{Version: "0.1.0", Kinds: []string{KindVerify}}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveHeartbeat(ctx, st); err != nil {
		t.Fatal(err)
	}
	h, err := q.Online(ctx)
	if err != nil || h == nil || h.Version != "0.1.0" || !h.Offers(KindVerify) {
		t.Fatalf("online = %+v, %v", h, err)
	}
	q.advance(OnlineWindow - time.Second)
	if h, _ := q.Online(ctx); h == nil {
		t.Fatal("offline inside the window")
	}
	q.advance(2 * time.Second)
	if h, _ := q.Online(ctx); h != nil {
		t.Fatal("online past the window")
	}
	if h, _ := q.LastHeartbeat(ctx); h == nil || h.Version != "0.1.0" {
		t.Fatal("the last heartbeat is kept past the window")
	}
}

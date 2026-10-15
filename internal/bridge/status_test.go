package bridge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
		"javascript link": {UIURL: "javascript:alert(1)"},
		"no host":         {UIURL: "http:///x"},
		"credentials":     {UIURL: "http://user:pw@host/"},
		"bad kind":        {Kinds: []string{"Bridge.Verify"}},
		"kind twice":      {Kinds: []string{KindVerify, KindVerify}},
		"library twice":   {Libraries: []LibraryStatus{{LibraryID: 1}, {LibraryID: 1}}},
		"library zero":    {Libraries: []LibraryStatus{{LibraryID: 0}}},
	}
	for name, st := range bad {
		if err := st.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
	// Free text is partly the outside world's (a file's name, an OS error): it is
	// cleaned, never the reason a heartbeat is refused.
	messy := Status{Version: strings.Repeat("v", maxShortText+1),
		Libraries: []LibraryStatus{{LibraryID: 1, CheckedPath: "Author/\tBook", Error: "size\r\ndiffers\x1b[0m " + strings.Repeat("é", maxLongText)}},
		Workers:   []Worker{{Name: "gpu\x00box"}}}
	if err := messy.Validate(); err != nil {
		t.Fatalf("messy text: Validate = %v, want it cleaned", err)
	}
	l := messy.Libraries[0]
	if len(messy.Version) != maxShortText || l.CheckedPath != "Author/ Book" || !strings.HasPrefix(l.Error, "size  differs [0m é") ||
		len(l.Error) > maxLongText || !utf8.ValidString(l.Error) || messy.Workers[0].Name != "gpu box" {
		t.Fatalf("cleaned = %q / %+v / %q", messy.Version, l, messy.Workers[0].Name)
	}
}

func TestFeaturesAreKnownFamilies(t *testing.T) {
	t.Parallel()
	st := Status{Kinds: []string{"files.delete", KindVerify, "bogus.kind"}}
	// A kind the server doesn't know is never offered.
	if got := st.Features(); !slices.Equal(got, []string{"bridge"}) {
		t.Fatalf("Features = %v", got)
	}
	// None known: an empty list, never nil (the wire's features is a list; the
	// console reads its length).
	if got := (Status{Kinds: []string{"bogus.kind"}}).Features(); got == nil || len(got) != 0 {
		t.Fatalf("Features with none known = %#v, want []string{}", got)
	}
}

func TestHeartbeatOnlineWindow(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	// Unpaired (auth's UnpairBridge deletes the row): no heartbeat is kept.
	if _, err := q.db.ExecContext(ctx, `DELETE FROM bridge_status`); err != nil {
		t.Fatal(err)
	}
	q.Forget()
	if err := q.SaveHeartbeat(ctx, q.token, Status{}); !errors.Is(err, ErrNotPaired) {
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
	if err := q.SaveHeartbeat(ctx, q.token, st); err != nil {
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

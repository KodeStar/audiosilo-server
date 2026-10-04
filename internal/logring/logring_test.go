package logring

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func newLogger(ring *Ring) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(NewHandler(inner, ring, slog.LevelInfo)), &buf
}

func TestHandlerTeesAndFlattens(t *testing.T) {
	ring := NewRing(10)
	log, out := newLogger(ring)
	log.With("library", "Fiction").WithGroup("scan").Info("detected move", "from", "a", "to", "b",
		slog.Group("counts", "added", 2))
	log.Debug("not recorded")
	log.Warn("failed", "err", errors.New("boom"))

	if !strings.Contains(out.String(), "detected move") || strings.Contains(out.String(), "not recorded") {
		t.Fatalf("the wrapped handler must still get every record at its level:\n%s", out)
	}
	res := ring.Query(Query{})
	if len(res.Entries) != 2 || res.LastSeq != 2 {
		t.Fatalf("got %d entries (last %d), want 2", len(res.Entries), res.LastSeq)
	}
	e := res.Entries[0]
	got := map[string]string{}
	for _, a := range e.Attrs {
		got[a.Key] = a.Value
	}
	want := map[string]string{"library": "Fiction", "scan.from": "a", "scan.to": "b", "scan.counts.added": "2"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("attr %s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if e.Level != "info" || res.Entries[1].Level != "warn" || res.Entries[1].Attrs[0].Value != "boom" {
		t.Fatalf("levels/err value wrong: %+v", res.Entries)
	}
}

// A secret-named attribute never reaches the ring, even though the wrapped
// handler (the operator's own log) is untouched.
func TestRedactsSecrets(t *testing.T) {
	ring := NewRing(10)
	log, _ := newLogger(ring)
	log.Info("x", "token", "abc", "auth_code", "123", "api-key", "k", "codec", "aac", "password", "p")
	vals := map[string]string{}
	for _, a := range ring.Query(Query{}).Entries[0].Attrs {
		vals[a.Key] = a.Value
	}
	for _, k := range []string{"token", "auth_code", "api-key", "password"} {
		if vals[k] != "[redacted]" {
			t.Errorf("%s = %q, want redacted", k, vals[k])
		}
	}
	if vals["codec"] != "aac" {
		t.Errorf("codec is not a secret, got %q", vals["codec"])
	}
}

func TestQueryFiltersAndCursor(t *testing.T) {
	ring := NewRing(3)
	log, _ := newLogger(ring)
	for i, m := range []string{"one", "two", "three", "four", "five"} {
		if i == 3 {
			log.Error(m, "where", "disk")
			continue
		}
		log.Info(m)
	}
	all := ring.Query(Query{})
	if len(all.Entries) != 3 || all.Entries[0].Message != "three" || all.LastSeq != 5 {
		t.Fatalf("ring must keep the newest 3 in order: %+v", all)
	}
	if res := ring.Query(Query{After: 1}); !res.Truncated {
		t.Fatal("a cursor the ring moved past must report truncated")
	}
	if res := ring.Query(Query{After: 4}); len(res.Entries) != 1 || res.Entries[0].Message != "five" || res.Truncated {
		t.Fatalf("after 4: %+v", res)
	}
	if res := ring.Query(Query{Level: slog.LevelError}); len(res.Entries) != 1 || res.Entries[0].Message != "four" {
		t.Fatalf("level filter: %+v", res)
	}
	if res := ring.Query(Query{Search: "DISK"}); len(res.Entries) != 1 {
		t.Fatalf("search matches attributes, any case: %+v", res)
	}
	if res := ring.Query(Query{Limit: 2}); len(res.Entries) != 2 || res.Entries[1].Message != "five" || !res.Truncated {
		t.Fatalf("limit keeps the newest: %+v", res)
	}
}

func TestCutKeepsRunes(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	if got := cut(s, 5); got != "éé…" {
		t.Fatalf("cut = %q", got)
	}
	if got := cut("short", 10); got != "short" {
		t.Fatalf("cut = %q", got)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"": slog.LevelDebug, "info": slog.LevelInfo, "WARN": slog.LevelWarn, "error": slog.LevelError} {
		if got, ok := ParseLevel(in); !ok || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v", in, got, ok)
		}
	}
	if _, ok := ParseLevel("loud"); ok {
		t.Error("unknown level must not parse")
	}
}

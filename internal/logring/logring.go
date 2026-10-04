// Package logring keeps the server's newest log records in memory for the admin
// console's log viewer (Server > Logs). Handler tees every record the server
// logs into a Ring of bounded size; nothing is written to disk, so the viewer
// shows what happened since the server started, and the full log stays wherever
// the process's output goes (stderr, docker logs, the desktop app's log).
//
// Records are copied out as plain text, so a later change to a logged value
// can't reach the ring. Attributes whose key names a secret are redacted, and
// long values are cut, on the way in: the console shows these lines in a browser.
package logring

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Entry is one log record as the console shows it.
type Entry struct {
	Seq     uint64    `json:"seq"`
	Time    time.Time `json:"time"`
	Level   string    `json:"level"` // debug | info | warn | error
	Message string    `json:"message"`
	Attrs   []Attr    `json:"attrs"`
}

// Attr is one key=value of an Entry; a grouped key is dotted ("req.path").
type Attr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Limits on what one entry holds.
const (
	maxMessage = 2000
	maxValue   = 1000
	maxAttrs   = 32
)

// Ring holds the newest records, oldest first. Safe for concurrent use.
type Ring struct {
	mu      sync.Mutex
	entries []Entry // a circular buffer of cap(entries) once full
	start   int     // index of the oldest entry once full
	seq     uint64  // the newest entry's Seq (0: none yet)
}

// NewRing returns a ring that keeps the newest capacity records.
func NewRing(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{entries: make([]Entry, 0, capacity)}
}

// Capacity is how many records the ring keeps.
func (r *Ring) Capacity() int { return cap(r.entries) }

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e.Seq = r.seq
	if len(r.entries) < cap(r.entries) {
		r.entries = append(r.entries, e)
		return
	}
	r.entries[r.start] = e
	r.start = (r.start + 1) % len(r.entries)
}

// Query selects entries. Zero values select everything.
type Query struct {
	After  uint64     // only entries with Seq > After (the live tail's cursor)
	Level  slog.Level // only entries at or above this level
	Search string     // only entries whose message or attributes contain it (any case)
	Limit  int        // at most this many, the newest ones (<= 0: all)
}

// Result is a query's answer.
type Result struct {
	Entries []Entry `json:"entries"` // oldest first
	// LastSeq is the newest entry's Seq in the ring (whether or not it matched), the
	// cursor for the next poll's After.
	LastSeq uint64 `json:"last_seq"`
	// Truncated reports that matching entries were left out: older ones past Limit,
	// or entries after After that the ring already dropped.
	Truncated bool `json:"truncated"`
}

// Query returns the matching entries, oldest first.
func (r *Ring) Query(q Query) Result {
	needle := strings.ToLower(strings.TrimSpace(q.Search))
	r.mu.Lock()
	all := make([]Entry, 0, len(r.entries))
	all = append(all, r.entries[r.start:]...)
	all = append(all, r.entries[:r.start]...)
	last := r.seq
	r.mu.Unlock()

	res := Result{Entries: []Entry{}, LastSeq: last}
	if len(all) > 0 && q.After > 0 && all[0].Seq > q.After+1 {
		res.Truncated = true // the ring moved past the cursor
	}
	for _, e := range all {
		if e.Seq <= q.After || levelOf(e.Level) < q.Level || (needle != "" && !e.contains(needle)) {
			continue
		}
		res.Entries = append(res.Entries, e)
	}
	if q.Limit > 0 && len(res.Entries) > q.Limit {
		res.Entries = slices.Clone(res.Entries[len(res.Entries)-q.Limit:])
		res.Truncated = true
	}
	return res
}

func (e Entry) contains(needle string) bool {
	if strings.Contains(strings.ToLower(e.Message), needle) {
		return true
	}
	for _, a := range e.Attrs {
		if strings.Contains(strings.ToLower(a.Key+"="+a.Value), needle) {
			return true
		}
	}
	return false
}

// ParseLevel reads a level name ("debug", "info", "warn", "error"; "" = debug).
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(s) {
	case "", "debug", "all":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	}
	return "debug"
}

func levelOf(name string) slog.Level {
	l, _ := ParseLevel(name)
	return l
}

// Handler is a slog.Handler that passes every record to the handler it wraps
// and copies those at or above its level into a Ring.
type Handler struct {
	next  slog.Handler
	ring  *Ring
	level slog.Level
	attrs []Attr // from WithAttrs, already flattened under their groups
	group string // the WithGroup prefix ("a.b.")
}

// NewHandler wraps next, recording records at or above level into ring.
func NewHandler(next slog.Handler, ring *Ring, level slog.Level) *Handler {
	return &Handler{next: next, ring: ring, level: level}
}

// Enabled reports whether either the wrapped handler or the ring wants records
// at this level.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.level || h.next.Enabled(ctx, l)
}

// Handle records r in the ring (at or above the ring's level) and passes it on
// (when the wrapped handler takes its level).
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= h.level {
		e := Entry{Time: r.Time, Level: levelName(r.Level), Message: cut(r.Message, maxMessage)}
		e.Attrs = append(e.Attrs, h.attrs...)
		r.Attrs(func(a slog.Attr) bool {
			e.Attrs = flatten(e.Attrs, h.group, a)
			return len(e.Attrs) < maxAttrs
		})
		if len(e.Attrs) > maxAttrs {
			e.Attrs = e.Attrs[:maxAttrs]
		}
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		h.ring.add(e)
	}
	if h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

// WithAttrs returns a handler whose records carry attrs.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(attrs)
	c.attrs = slices.Clone(h.attrs)
	for _, a := range attrs {
		c.attrs = flatten(c.attrs, h.group, a)
	}
	return &c
}

// WithGroup returns a handler that puts later attributes under name.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.next = h.next.WithGroup(name)
	c.group = h.group + name + "."
	return &c
}

// flatten appends a (and a group's members, dotted) to out.
func flatten(out []Attr, prefix string, a slog.Attr) []Attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, m := range v.Group() {
			out = flatten(out, p, m)
		}
		return out
	}
	if a.Key == "" {
		return out
	}
	key := prefix + a.Key
	val := "[redacted]"
	if !secretKey(a.Key) {
		val = cut(valueString(v), maxValue)
	}
	return append(out, Attr{Key: key, Value: val})
}

func valueString(v slog.Value) string {
	if v.Kind() == slog.KindAny {
		if err, ok := v.Any().(error); ok {
			return err.Error()
		}
		return fmt.Sprint(v.Any())
	}
	return v.String()
}

// secretWords mark an attribute as a secret when they are a word of its key
// ("token", "auth_code", "api-key"; not "codec"). No log call in the server logs
// one; this keeps a future one out of a browser.
var secretWords = []string{"token", "password", "passwd", "secret", "authorization", "cookie", "code", "key", "apikey"}

func secretKey(key string) bool {
	words := strings.FieldsFunc(strings.ToLower(key), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, w := range words {
		if slices.Contains(secretWords, w) {
			return true
		}
	}
	return false
}

// cut shortens s to at most n bytes on a rune boundary, marking the cut.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…"
}

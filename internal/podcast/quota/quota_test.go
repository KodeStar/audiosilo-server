package quota

import (
	"context"
	"strconv"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// A user's bucket: burst then one token per 6 s at 10/minute, Retry-After the
// whole seconds until the next token.
func TestTakeRefillAndRetryAfter(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	l := New(10, 3, 0, 10*time.Minute, c.now)
	for i := range 3 {
		if d := l.Take("u"); !d.OK {
			t.Fatalf("burst %d refused", i)
		}
	}
	if d := l.Take("u"); d.OK || d.RetryAfter != 6 || d.Full {
		t.Fatalf("empty = %+v", d)
	}
	if d := l.Take("other"); !d.OK {
		t.Fatal("keys share a bucket")
	}
	c.t = c.t.Add(5 * time.Second)
	if d := l.Take("u"); d.OK || d.RetryAfter != 1 {
		t.Fatalf("almost = %+v", d)
	}
	c.t = c.t.Add(time.Second)
	if d := l.Take("u"); !d.OK {
		t.Fatalf("refilled = %+v", d)
	}
}

// Refund gives a spent token back, never past the burst, and leaves a key it
// holds no bucket for alone.
func TestRefund(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	l := New(10, 2, 0, 10*time.Minute, c.now)
	l.Take("u")
	l.Take("u")
	l.Refund("u")
	if d := l.Take("u"); !d.OK {
		t.Fatalf("after a refund = %+v", d)
	}
	l.Refund("u")
	l.Refund("u")
	l.Refund("u") // past the burst: still two
	for i := range 2 {
		if d := l.Take("u"); !d.OK {
			t.Fatalf("token %d after refunds = %+v", i, d)
		}
	}
	if d := l.Take("u"); d.OK {
		t.Fatal("a refund went past the burst")
	}
	l.Refund("nobody")
	if d := l.Take("nobody"); !d.OK {
		t.Fatalf("a new key = %+v", d)
	}
}

// A full map refuses a new key (busy) while every bucket is active, and admits
// it once an idle one can go; an active bucket is never evicted.
func TestMapBound(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	l := New(10, 3, 3, 10*time.Minute, c.now)
	for i := range 3 {
		l.Take(strconv.Itoa(i))
	}
	if d := l.Take("new"); d.OK || !d.Full || d.RetryAfter != 1 {
		t.Fatalf("full = %+v", d)
	}
	c.t = c.t.Add(9 * time.Minute)
	l.Take("0") // keeps 0 active
	c.t = c.t.Add(time.Minute)
	if d := l.Take("new"); !d.OK {
		t.Fatalf("after idle = %+v", d)
	}
	if n := len(l.buckets); n != 2 {
		t.Fatalf("len %d", n)
	}
}

func TestSlots(t *testing.T) {
	s := NewSlots(2)
	first, second, third := s.TryAcquire(), s.TryAcquire(), s.TryAcquire()
	if !first || !second || third {
		t.Fatal("two slots")
	}
	s.Release()
	if !s.TryAcquire() {
		t.Fatal("released")
	}
}

// Acquire waits for a released slot, and gives up when its context ends.
func TestSlotsAcquire(t *testing.T) {
	s := NewSlots(1)
	if !s.Acquire(context.Background()) {
		t.Fatal("a free slot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if s.Acquire(ctx) {
		t.Fatal("a busy slot past the deadline")
	}
	got := make(chan bool)
	go func() { got <- s.Acquire(context.Background()) }()
	time.Sleep(10 * time.Millisecond)
	s.Release()
	if !<-got {
		t.Fatal("the released slot")
	}
}

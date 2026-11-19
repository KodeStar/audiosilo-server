package pool

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

var errOff = errors.New("switched off")

func TestWhileOnCancelsWithItsCauseOnceOff(t *testing.T) {
	var on atomic.Bool
	on.Store(true)
	ctx, stop := WhileOn(context.Background(), time.Millisecond, on.Load, errOff)
	defer stop()
	time.Sleep(10 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("cancelled while on: %v", context.Cause(ctx))
	}
	on.Store(false)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("not cancelled once off")
	}
	if !errors.Is(context.Cause(ctx), errOff) {
		t.Fatalf("cause = %v, want the switch's", context.Cause(ctx))
	}
}

func TestWhileOnStopReleasesWithoutTheCause(t *testing.T) {
	ctx, stop := WhileOn(context.Background(), time.Hour, func() bool { return true }, errOff)
	stop()
	if ctx.Err() == nil || errors.Is(context.Cause(ctx), errOff) {
		t.Fatalf("after stop: err %v, cause %v", ctx.Err(), context.Cause(ctx))
	}
}

func TestJitteredStaysWithinItsBounds(t *testing.T) {
	for range 200 {
		d := Jittered(24*time.Hour, time.Hour)
		if d < 23*time.Hour || d >= 25*time.Hour {
			t.Fatalf("Jittered = %v, want within an hour of a day", d)
		}
	}
	if d := Jittered(time.Minute, 0); d != time.Minute {
		t.Fatalf("no jitter = %v", d)
	}
}

func TestWaitReadyEndsOnReadyDeadlineOrCancel(t *testing.T) {
	var n atomic.Int32
	if isReady, live := WaitReady(t.Context(), func() bool { return n.Add(1) > 2 }, time.Hour, time.Millisecond); !isReady || !live {
		t.Fatalf("ready after polls: isReady=%v live=%v", isReady, live)
	}
	never := func() bool { return false }
	if isReady, live := WaitReady(t.Context(), never, 20*time.Millisecond, time.Millisecond); isReady || !live {
		t.Fatalf("past the wait: isReady=%v live=%v", isReady, live)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, live := WaitReady(ctx, never, time.Hour, time.Millisecond); live {
		t.Fatal("a cancelled wait must report ctx ended")
	}
}

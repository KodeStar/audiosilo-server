package testwait

import (
	"testing"
	"time"
)

func TestScaled(t *testing.T) {
	t.Parallel()
	want := time.Second
	if Race {
		want = raceFactor * time.Second
	}
	if got := Scaled(time.Second); got != want {
		t.Fatalf("Scaled(1s) = %v, want %v (race %t)", got, want, Race)
	}
}

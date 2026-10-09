package api

import (
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

func TestMain(m *testing.M) {
	auth.UseCheapHashingForTests()
	m.Run()
}

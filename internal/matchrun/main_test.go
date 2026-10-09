package matchrun

import (
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

// Every test env creates a password admin; hash it cheaply.
func TestMain(m *testing.M) {
	auth.UseCheapHashingForTests()
	m.Run()
}

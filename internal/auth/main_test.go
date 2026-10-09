package auth

import "testing"

func TestMain(m *testing.M) {
	UseCheapHashingForTests()
	m.Run()
}

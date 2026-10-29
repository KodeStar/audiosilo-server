package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// database.mmap: empty (auto) unless set, one of auto/on/off, from config.yaml
// or AUDIOSILO_DB_MMAP, read at start and no console setting; an unset value is
// not written to config.yaml.
func TestDatabaseMmap(t *testing.T) {
	c := Default(t.TempDir())
	if c.Database.Mmap != "" {
		t.Fatalf("default = %q", c.Database.Mmap)
	}
	if _, ok := c.Settings()["database"]; ok {
		t.Fatal("database.mmap is a console setting")
	}
	if f := fieldByKey("database.mmap"); f == nil || !f.restart {
		t.Fatal("database.mmap is not a restart field")
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(Path(c.DataDir)); err != nil || strings.Contains(string(b), "database") {
		t.Fatalf("an unset database.mmap was saved: %v\n%s", err, b)
	}

	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("database:\n  mmap: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _, err := Load(dir); err != nil || c.Database.Mmap != "off" {
		t.Fatalf("from config.yaml = %+v, %v", c.Database, err)
	}
	for _, m := range []string{"on", "auto", "off"} {
		t.Setenv("AUDIOSILO_DB_MMAP", m)
		c, _, err := Load(dir)
		if err != nil || c.Database.Mmap != m {
			t.Fatalf("from the environment %q = %+v, %v", m, c.Database, err)
		}
	}
	for _, bad := range []string{"yes", "ON", "1"} {
		t.Setenv("AUDIOSILO_DB_MMAP", bad)
		var fe *FieldError
		if _, _, err := Load(dir); !errors.As(err, &fe) || fe.Key != "database.mmap" {
			t.Fatalf("mmap %q loaded: %v", bad, err)
		}
	}
}

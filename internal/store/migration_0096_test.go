package store

import (
	"slices"
	"testing"
)

func TestMigration0096RechecksMKACovers(t *testing.T) {
	t.Parallel()
	dsn, exec, closeRaw := openBefore(t, "0096")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1,'Books','/books','t')`)
	exec(`INSERT INTO books(library_id, rel_path, title, format, has_cover, indexed_at) VALUES
 (1,'a','a','mka',0,'t'), (1,'b','b','mka',1,'t'), (1,'c','c','mp3',0,'t')`)
	closeRaw()
	db, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got := lines(t, db, `SELECT rel_path || ':' || coalesce(has_cover,'unknown') FROM books ORDER BY rel_path`)
	if !slices.Equal(got, []string{"a:unknown", "b:1", "c:0"}) {
		t.Fatalf("cover flags %v", got)
	}
}

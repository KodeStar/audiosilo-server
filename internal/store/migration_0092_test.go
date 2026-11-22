package store

import (
	"context"
	"slices"
	"testing"
)

func TestMigration0092CarriesHealthIgnores(t *testing.T) {
	t.Parallel()
	dsn, exec, closeRaw := openBefore(t, "0092")
	exec(`INSERT INTO libraries(id,name,root,created_at) VALUES(1,'L','/l','t'),(2,'M','/m','t')`)
	exec(`INSERT INTO books(library_id,rel_path,title,indexed_at,split_parent) VALUES
		(1,'Box/Disc 10','Box','t','Box'),(1,'Box/Disc 2','Box','t','Box')`)
	exec(`INSERT INTO issue_ignores(library_id,path,kind,created_at) VALUES
		(1,'Box/Disc 10','split_discs','t1'),(1,'Box/Disc 2','split_discs','t2'),
		(2,'Missing/Book/CD2','split_discs','t3'),(1,'unplaced','split_discs','t4'),(2,'Moved/Title','split_discs','t4'),
		(1,'A & B','duplicate','t5'),(1,'Z','duplicate','t5'),(2,'A & B','duplicate','t5'),
		(1,'Orphan','duplicate','t6'),(1,'A & B','no_cover','t7')`)
	closeRaw()
	db, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	got := lines(t, db, `SELECT library_id || ' ' || path || ' ' || kind || ' ' || created_at FROM issue_ignores ORDER BY 1`)
	want := []string{"1 A & B duplicate t5", "1 A & B no_cover t7", "1 Box split_disc_set t1", "1 Orphan duplicate t6", "1 Z duplicate t5", "1 unplaced split_discs t4", "2 A & B duplicate t5", "2 Missing/Book split_disc_set t3", "2 Moved/Title split_discs t4"}
	if !slices.Equal(got, want) {
		t.Fatalf("member and disc ignores = %q, want %q", got, want)
	}
	got = lines(t, db, `SELECT library_id || ' ' || other_library_id || ' ' || paths || ' ' || created_at FROM duplicate_decisions ORDER BY 1`)
	want = []string{`1 1 ["A & B","Z"] t5`, `1 2 ["A & B","A & B"] t5`, `1 2 ["Z","A & B"] t5`}
	if !slices.Equal(got, want) {
		t.Fatalf("decisions = %q, want %q", got, want)
	}
	if got := lines(t, db, `SELECT value FROM server_state WHERE key='map_duplicate_ignores_0092'`); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("migration marker %v", got)
	}
}

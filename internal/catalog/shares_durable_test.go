package catalog

import (
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// S260-REVOKE: every write that takes share access away commits durably (one
// store.WithDurableTx commit each, synchronous=FULL), so a power cut right after
// it answered can't bring the access back; and the access is gone. The writes
// that give access stay ordinary commits.
func TestShareRevocationsAreDurable(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	db := c.db.(*store.DB)
	user := seedUser(t, c, ctx)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	durable := func(name string, op func() error) {
		t.Helper()
		before := db.DurableCommits()
		if err := op(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := db.DurableCommits() - before; got != 1 {
			t.Fatalf("%s: %d durable commits, want 1", name, got)
		}
	}
	ordinary := func(name string, op func() error) {
		t.Helper()
		before := db.DurableCommits()
		if err := op(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := db.DurableCommits() - before; got != 0 {
			t.Fatalf("%s: %d durable commits, want none", name, got)
		}
	}
	paths := func() []string {
		t.Helper()
		sc, err := c.UserScope(ctx, user, lib.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		return sc.Paths
	}
	rule := func(p string) PathRule { return PathRule{LibraryID: lib.ID, Path: p} }

	var share *Share
	ordinary("create share", func() error {
		share, err = c.CreateShare(ctx, Share{Name: "S", Paths: []PathRule{rule("A")}})
		return err
	})
	ordinary("add path", func() error { return c.AddSharePath(ctx, share.ID, rule("B")) })
	ordinary("grant", func() error { return c.GrantShare(ctx, user, share.ID) })
	if got := paths(); len(got) != 2 {
		t.Fatalf("granted paths = %q, want A and B", got)
	}

	durable("remove path", func() error { return c.RemoveSharePath(ctx, share.ID, rule("B")) })
	if got := paths(); len(got) != 1 || got[0] != "A" {
		t.Fatalf("after removing B: %q, want [A]", got)
	}
	durable("revoke", func() error { return c.RevokeShare(ctx, user, share.ID) })
	if got := paths(); len(got) != 0 {
		t.Fatalf("after revoking: %q, want none", got)
	}
	ordinary("grant again", func() error { return c.GrantShare(ctx, user, share.ID) })
	durable("delete share", func() error { return c.DeleteShare(ctx, share.ID) })
	if got := paths(); len(got) != 0 {
		t.Fatalf("after deleting the share: %q, want none", got)
	}
}

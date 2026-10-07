package catalog

import (
	"testing"
	"time"
)

// userStateFixture is the base of the per-user state tests (ratings,
// annotations): a file-backed catalog whose clock the test sets, one library and
// two users.
type userStateFixture struct {
	c        *Catalog
	clock    time.Time
	lib      int64
	ann, bob int64
}

func newUserStateFixture(t *testing.T, start time.Time) *userStateFixture {
	t.Helper()
	c, ctx := newTestCatalog(t)
	f := &userStateFixture{c: c, clock: start}
	c.now = func() time.Time { return f.clock }
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	f.lib = lib.ID
	f.ann, f.bob = seedNamedUser(t, c, "ann"), seedNamedUser(t, c, "bob")
	return f
}

func (f *userStateFixture) ref(p string) Ref { return Ref{LibraryID: f.lib, Path: p} }

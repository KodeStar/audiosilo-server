package library

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
	"github.com/kodestar/audiosilo-server/internal/testwait"
)

// TestWaitReadsLeavesAHeldRead: a book read detached from its request and
// held where no context reaches it (the close of a file whose read was given
// up on, which a FUSE share holds until it answers) does not hold shutdown
// past WaitReads' wait: it is counted as left, the database closes, and when
// the read returns at last it writes nothing, to the closed database or the
// file. Not parallel: metadata.HoldClosesForTests's hook is global.
func TestWaitReadsLeavesAHeldRead(t *testing.T) {
	ctx := context.Background()
	path := storetest.Path(t)
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cat := catalog.New(db, time.Now)
	s := NewScanner(cat, "", slog.Default())
	root := t.TempDir()
	writeFile(t, root, "A/Book/01.mp3", []byte("x"))
	created, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	lib := *created
	base, stop := context.WithCancel(ctx)
	defer stop()
	s.SetBaseContext(base)

	held, release := metadata.HoldClosesForTests(root)
	defer release()
	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	asked := make(chan error, 1)
	go func() {
		_, err := s.IndexPath(reqCtx, lib, "A/Book")
		asked <- err
	}()
	select {
	case <-held:
	case <-time.After(testwait.Scaled(20 * time.Second)):
		t.Fatal("the read never reached its file's close")
	}
	waitFlight(t, s, lib, "A/Book", true)
	cancelReq() // the request goes; the read is held on
	<-asked

	stop() // the server stops
	wait, cancelWait := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelWait()
	left := make(chan int, 1)
	go func() { left <- s.WaitReads(wait) }()
	select {
	case n := <-left:
		if n != 1 {
			t.Errorf("WaitReads left %d reads under way, want the held one", n)
		}
	case <-time.After(testwait.Scaled(5 * time.Second)):
		t.Fatal("WaitReads waited on the held read past its 100ms wait")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	release() // the share answers
	waitFlight(t, s, lib, "A/Book", false)
	again, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if _, err := catalog.New(again, time.Now).GetBookByPath(ctx, lib.ID, "A/Book"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("a read that returned after shutdown's wait was written: %v", err)
	}
}

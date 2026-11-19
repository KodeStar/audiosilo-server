package bridge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestStatusValidate(t *testing.T) {
	t.Parallel()
	good := Status{Version: "0.1.0", Kinds: []string{KindVerify, "sidecars.generate"},
		Libraries: []LibraryStatus{{LibraryID: 1, MountPath: "/media/books", Mapped: true}},
		UIURL:     "http://192.168.1.10:8095/"}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid status: %v", err)
	}
	bad := map[string]Status{
		"javascript link": {UIURL: "javascript:alert(1)"},
		"no host":         {UIURL: "http:///x"},
		"credentials":     {UIURL: "http://user:pw@host/"},
		"bad kind":        {Kinds: []string{"Bridge.Verify"}},
		"kind twice":      {Kinds: []string{KindVerify, KindVerify}},
		"library twice":   {Libraries: []LibraryStatus{{LibraryID: 1}, {LibraryID: 1}}},
		"library zero":    {Libraries: []LibraryStatus{{LibraryID: 0}}},
	}
	for name, st := range bad {
		if err := st.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
	// Free text is partly the outside world's (a file's name, an OS error): it is
	// cleaned, never the reason a heartbeat is refused.
	messy := Status{Version: strings.Repeat("v", maxShortText+1),
		Libraries: []LibraryStatus{{LibraryID: 1, CheckedPath: "Author/\tBook", Error: "size\r\ndiffers\x1b[0m " + strings.Repeat("é", maxLongText)}},
		Workers:   []Worker{{Name: "gpu\x00box"}}}
	if err := messy.Validate(); err != nil {
		t.Fatalf("messy text: Validate = %v, want it cleaned", err)
	}
	l := messy.Libraries[0]
	if len(messy.Version) != maxShortText || l.CheckedPath != "Author/ Book" || !strings.HasPrefix(l.Error, "size  differs [0m é") ||
		len(l.Error) > maxLongText || !utf8.ValidString(l.Error) || messy.Workers[0].Name != "gpu box" {
		t.Fatalf("cleaned = %q / %+v / %q", messy.Version, l, messy.Workers[0].Name)
	}
}

func TestFeaturesAreKnownFamilies(t *testing.T) {
	t.Parallel()
	st := Status{Kinds: []string{"files.delete", KindVerify, "bogus.kind"}}
	// A kind the server doesn't know is never offered.
	if got := st.Features(); !slices.Equal(got, []string{"bridge"}) {
		t.Fatalf("Features = %v", got)
	}
	// None known: an empty list, never nil (the wire's features is a list; the
	// console reads its length).
	if got := (Status{Kinds: []string{"bogus.kind"}}).Features(); got == nil || len(got) != 0 {
		t.Fatalf("Features with none known = %#v, want []string{}", got)
	}
}

func TestHeartbeatOnlineWindow(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	// Unpaired (auth's UnpairBridge deletes the row): no heartbeat is kept.
	if _, err := q.db.ExecContext(ctx, `DELETE FROM bridge_status`); err != nil {
		t.Fatal(err)
	}
	q.Forget()
	if err := q.SaveHeartbeat(ctx, q.token, Status{}); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("heartbeat before pairing: %v, want ErrNotPaired", err)
	}
	if _, err := q.db.ExecContext(ctx, `INSERT INTO bridge_status(id, paired_at) VALUES(1, '2026-10-15T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if h, err := q.Online(ctx); err != nil || h != nil {
		t.Fatalf("paired, no heartbeat: %v, %v", h, err)
	}
	st := Status{Version: "0.1.0", Kinds: []string{KindVerify}}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveHeartbeat(ctx, q.token, st); err != nil {
		t.Fatal(err)
	}
	h, err := q.Online(ctx)
	if err != nil || h == nil || h.Version != "0.1.0" || !h.Offers(KindVerify) {
		t.Fatalf("online = %+v, %v", h, err)
	}
	q.advance(OnlineWindow - time.Second)
	if h, _ := q.Online(ctx); h == nil {
		t.Fatal("offline inside the window")
	}
	q.advance(2 * time.Second)
	if h, _ := q.Online(ctx); h != nil {
		t.Fatal("online past the window")
	}
	if h, _ := q.LastHeartbeat(ctx); h == nil || h.Version != "0.1.0" {
		t.Fatal("the last heartbeat is kept past the window")
	}
}

// A heartbeat listing more libraries than the server keeps is cut to the
// first maxLibraries, never refused (server#445): a refused one reads as the
// Bridge offline, and it would send the same list again.
func TestStatusValidateCutsLibraries(t *testing.T) {
	t.Parallel()
	st := Status{Libraries: make([]LibraryStatus, maxLibraries+100)}
	for i := range st.Libraries {
		st.Libraries[i].LibraryID = int64(i + 1)
	}
	if err := st.Validate(); err != nil {
		t.Fatalf("Validate = %v, want the list cut", err)
	}
	if n := len(st.Libraries); n != maxLibraries || st.Libraries[n-1].LibraryID != maxLibraries {
		t.Fatalf("kept %d libraries, last %d; want the first %d", n, st.Libraries[n-1].LibraryID, maxLibraries)
	}
}

// A heartbeat is stored without the mappings of libraries the server doesn't
// have, or their ids in a setup item, and as plain JSON (no HTML escaping, which would store a "<" as six
// bytes): what a Bridge sends can't grow the stored row past its own body.
func TestHeartbeatStoredSmall(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	st := Status{Version: "0.6.0", Kinds: []string{KindVerify}}
	for i := range maxLibraries {
		id := int64(100000 + i)
		if i == 7 {
			id = q.lib
		}
		st.Libraries = append(st.Libraries, LibraryStatus{LibraryID: id, MountPath: "/media/<books> & more", Mapped: true})
	}
	// A setup item's library ids are held to the server's libraries too: as
	// many of them as a body holds, most repeated, are told apart without a
	// scan of the ids kept for each.
	ids := make([]int64, 0, 200_000)
	for i := range cap(ids) {
		ids = append(ids, int64(100000+i%(maxLibraries-2)))
	}
	ids[7] = q.lib // with the others, one under the bound: every id is looked at
	st.Setup = &Setup{Items: []SetupItem{{ID: "libraries", LibraryIDs: ids}, {ID: "paired", Done: true}}}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	if n := len(st.Setup.Items[0].LibraryIDs); n != maxLibraries-1 {
		t.Fatalf("the setup item keeps %d library ids, want each once (%d)", n, maxLibraries-1)
	}
	st.KeepLibraries(func(id int64) bool { return id == q.lib })
	if len(st.Libraries) != 1 || cap(st.Libraries) >= maxLibraries || st.Libraries[0].LibraryID != q.lib {
		t.Fatalf("kept %d mappings (cap %d), want the server's one in a list of its own", len(st.Libraries), cap(st.Libraries))
	}
	if kept := st.Setup.Items[0].LibraryIDs; len(kept) != 1 || cap(kept) >= maxLibraries || kept[0] != q.lib || st.Setup.Items[1].LibraryIDs != nil {
		t.Fatalf("setup items keep %v and %v, want the server's library alone", kept, st.Setup.Items[1].LibraryIDs)
	}
	if err := q.SaveHeartbeat(ctx, q.token, st); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := q.db.QueryRowContext(ctx, `SELECT status FROM bridge_status WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, `"mount_path":"/media/<books> & more"`) || strings.Contains(stored, "100000") || strings.HasSuffix(stored, "\n") {
		t.Fatalf("stored status = %s", stored)
	}
	q.Forget()
	if h, err := q.LastHeartbeat(ctx); err != nil || h == nil || h.Library(q.lib) == nil || h.Library(q.lib).MountPath != "/media/<books> & more" {
		t.Fatalf("read back = %+v, %v", h, err)
	}
	// None kept is a list, not null.
	st.KeepLibraries(func(int64) bool { return false })
	if st.Libraries == nil || len(st.Libraries) != 0 {
		t.Fatalf("no mapping kept = %#v, want an empty list", st.Libraries)
	}
}

// Callers that miss the heartbeat cache at once share one read of the row;
// when that read fails (its request ended), one of those waiting reads again
// and the others share that.
func TestLastHeartbeatSharesOneRead(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	q.advance(heartbeatTTL) // the heartbeat newTestQueue saved is no longer fresh
	var (
		mu      sync.Mutex
		reads   int
		entered = make(chan struct{}, 16)
		release = make(chan struct{})
	)
	q.readHeartbeat = func(ctx context.Context) (*Heartbeat, error) {
		mu.Lock()
		reads++
		mu.Unlock()
		entered <- struct{}{}
		select {
		case <-release:
			return readHeartbeat(ctx, q.db)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return reads
	}
	// The first caller's request ends mid-read: it alone gets its error.
	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := q.LastHeartbeat(leaderCtx)
		leaderErr <- err
	}()
	<-entered
	const waiters = 8
	type answer struct {
		h   *Heartbeat
		err error
	}
	answers := make(chan answer, waiters)
	for range waiters {
		go func() {
			h, err := q.LastHeartbeat(context.Background())
			answers <- answer{h, err}
		}()
	}
	time.Sleep(50 * time.Millisecond) // let them reach the read under way
	if n := count(); n != 1 {
		t.Fatalf("%d reads with one under way, want the others waiting on it", n)
	}
	cancel()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled caller's read = %v, want its context's error", err)
	}
	<-entered // one waiter reads again
	close(release)
	var first *Heartbeat
	for range waiters {
		a := <-answers
		if a.err != nil || a.h == nil || !a.h.Offers(KindVerify) {
			t.Fatalf("a waiter's heartbeat = %+v, %v", a.h, a.err)
		}
		if first == nil {
			first = a.h
		} else if a.h != first {
			t.Fatal("the waiters got different reads")
		}
	}
	if n := count(); n != 2 {
		t.Fatalf("%d reads, want the cancelled one and one more", n)
	}
	// A caller that ends while waiting leaves with its own error.
	q.advance(heartbeatTTL)
	release = make(chan struct{})
	go func() {
		_, err := q.LastHeartbeat(context.Background())
		leaderErr <- err
	}()
	<-entered
	gone, stop := context.WithCancel(context.Background())
	stop()
	if _, err := q.LastHeartbeat(gone); !errors.Is(err, context.Canceled) {
		t.Fatalf("a waiter whose request ended = %v, want its context's error", err)
	}
	close(release)
	if err := <-leaderErr; err != nil {
		t.Fatalf("the read it left = %v", err)
	}
}

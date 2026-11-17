package oidc_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kodestar/audiosilo-server/internal/oidc"
)

// stubIDP is a provider's discovery document served by a transport with no
// network, so the probe's waits run on synctest's clock: answered after delay,
// never (hang: until the caller's deadline) or refused at once (down).
type stubIDP struct {
	mu    sync.Mutex
	delay time.Duration
	hang  bool
	down  bool
	calls int
}

func (s *stubIDP) set(delay time.Duration, hang, down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay, s.hang, s.down = delay, hang, down
}

func (s *stubIDP) asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubIDP) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	delay, hang, down := s.delay, s.hang, s.down
	s.calls++
	s.mu.Unlock()
	if down {
		return nil, errors.New("connect: connection refused")
	}
	var answer <-chan time.Time
	if !hang {
		t := time.NewTimer(delay)
		defer t.Stop()
		answer = t.C
	}
	select {
	case <-answer:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	const doc = `{"issuer":"https://idp.test","authorization_endpoint":"https://idp.test/authorize",` +
		`"token_endpoint":"https://idp.test/token","jwks_uri":"https://idp.test/jwks"}`
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Request: r,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(doc))}, nil
}

// start is one /start against idp's provider: its error and how long it took.
func start(p *oidc.Provider) (time.Duration, error) {
	began := time.Now()
	_, err := p.AuthURL(context.Background(), flow(), scopes)
	return time.Since(began), err
}

func newStubProvider(t *testing.T, idp *stubIDP) *oidc.Provider {
	t.Helper()
	p, err := oidc.New(oidc.Settings{Issuer: "https://idp.test", ClientID: "audiosilo",
		HTTPClient: &http.Client{Transport: idp}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProbeGivesASlowProviderTheBudget: a provider whose discovery document
// always takes seconds was discovered within the call budget, and is probed
// within it too (allowed): its sign-ins keep starting, each probe due costing
// one slow answer, and the status stays ok.
func TestProbeGivesASlowProviderTheBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const slow = 5 * time.Second
		idp := &stubIDP{delay: slow}
		p := newStubProvider(t, idp)
		for i := 1; i <= 3; i++ {
			if took, err := start(p); err != nil || took != slow {
				t.Fatalf("start %d of a slow provider: %v after %v", i, err, took)
			}
			// Within the interval the answer stands: nothing asked.
			if took, err := start(p); err != nil || took != 0 || idp.asked() != i {
				t.Fatalf("start %d again: %v after %v, asked %d times", i, err, took, idp.asked())
			}
			if st := p.Check(context.Background()); st.State != oidc.StateOK {
				t.Fatalf("status of a slow provider: %+v", st)
			}
			time.Sleep(31 * time.Second)
		}
	})
}

// TestProbeOfADeadProvider: a provider gone since its discovery ends a
// sign-in's start as unavailable (denied), at once when it refuses the
// connection and at the call budget's end when it never answers. A failure
// stands only a few seconds from when it was learned: those behind it aren't
// made to wait for it again, and a provider that is back signs people in
// well within the half minute a good answer stands for (allowed).
func TestProbeOfADeadProvider(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		idp := &stubIDP{}
		p := newStubProvider(t, idp)
		if _, err := start(p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(31 * time.Second)

		idp.set(0, false, true)
		if took, err := start(p); !errors.Is(err, oidc.ErrUnavailable) || took != 0 {
			t.Fatalf("a provider refusing connections: %v after %v", err, took)
		}
		if st := p.Status(); st.State != oidc.StateError || !strings.Contains(st.Error, "refused") {
			t.Fatalf("status of a provider gone: %+v", st)
		}
		asked := idp.asked()
		time.Sleep(2 * time.Second)
		if _, err := start(p); !errors.Is(err, oidc.ErrUnavailable) || idp.asked() != asked {
			t.Fatalf("within the failure's hold: %v, asked %d times, want %d", err, idp.asked(), asked)
		}
		// Back: asked again 6 s after the failure, not 30.
		idp.set(0, false, false)
		time.Sleep(4 * time.Second)
		if _, err := start(p); err != nil {
			t.Fatalf("a provider that is back: %v", err)
		}
		if st := p.Status(); st.State != oidc.StateOK {
			t.Fatalf("status once it is back: %+v", st)
		}
		time.Sleep(31 * time.Second)

		idp.set(0, true, false)
		if took, err := start(p); !errors.Is(err, oidc.ErrUnavailable) || took != oidc.CallBudget {
			t.Fatalf("a provider that never answers: %v after %v, want the budget %v", err, took, oidc.CallBudget)
		}
		asked = idp.asked()
		if took, err := start(p); !errors.Is(err, oidc.ErrUnavailable) || took != 0 || idp.asked() != asked {
			t.Fatalf("the start behind it: %v after %v, asked %d times, want %d", err, took, idp.asked(), asked)
		}
	})
}

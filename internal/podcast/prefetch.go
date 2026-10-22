package podcast

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

type prefetchCurrent struct {
	ref     catalog.Ref
	streams int
	chosen  bool
	busy    bool
	pending *catalog.Caller
}

// Prefetcher owns bounded one-ahead work. Caller is the actual authenticated
// request's caller, retaining its member rule and podcast client gate.
type Prefetcher struct {
	Catalog *catalog.Catalog
	Bridge  *bridge.Service
	Context context.Context
	mu      sync.Mutex
	users   map[int64]*prefetchCurrent
	slots   chan struct{}
	work    sync.WaitGroup
}

func (p *Prefetcher) Start(who catalog.Caller, current catalog.Ref, stream bool) func() {
	p.mu.Lock()
	if p.users == nil {
		p.users = map[int64]*prefetchCurrent{}
		p.slots = make(chan struct{}, 8)
	}
	c := p.users[who.ID]
	if c == nil || c.ref != current {
		if c == nil && len(p.users) >= 4096 {
			p.mu.Unlock()
			return func() {}
		}
		c = &prefetchCurrent{ref: current}
		p.users[who.ID] = c
	}
	if stream {
		c.streams++
	}
	p.mu.Unlock()
	p.reconsider(who, true)
	return func() {
		if stream {
			p.mu.Lock()
			c.streams--
			p.mu.Unlock()
		}
	}
}

// Reconsider is called only after a successful queue mutation, or a play-fetch
// trigger. Admission happens before starting a goroutine and never waits.
func (p *Prefetcher) Reconsider(who catalog.Caller) { p.reconsider(who, false) }
func (p *Prefetcher) reconsider(who catalog.Caller, initial bool) {
	p.mu.Lock()
	c := p.users[who.ID]
	if c == nil || c.chosen {
		p.mu.Unlock()
		return
	}
	if c.busy {
		c.pending = &who
		p.mu.Unlock()
		return
	}
	select {
	case p.slots <- struct{}{}:
	default:
		p.mu.Unlock()
		return
	}
	c.busy = true
	active := initial || c.streams > 0
	p.mu.Unlock()
	p.work.Add(1)
	go func() {
		defer p.work.Done()
		chosen := false
		defer func() {
			p.mu.Lock()
			c.busy = false
			c.chosen = c.chosen || chosen
			pending := c.pending
			c.pending = nil
			p.mu.Unlock()
			<-p.slots
			if pending != nil {
				p.Reconsider(*pending)
			}
		}()
		ctx, cancel := context.WithTimeout(p.Context, StreamWait)
		defer cancel()
		if !active {
			states, err := p.Bridge.PodcastFetchStatuses(ctx, c.ref.LibraryID, []string{c.ref.Path})
			if err != nil {
				return
			}
			st := states[c.ref.Path]
			if st.RequestID == nil || (st.State != "queued" && st.State != "fetching" && st.State != "unavailable") {
				return
			}
		}
		scopes, err := p.Catalog.CallerScopes(ctx, who)
		if err != nil {
			return
		}
		if !catalog.ScopesAllow(scopes, c.ref) {
			return
		}
		queue, err := p.Catalog.Queue(ctx, who.ID, scopes)
		if err != nil {
			return
		}
		libs, err := p.Catalog.ListLibraries(ctx)
		if err != nil {
			return
		}
		podcasts := map[int64]bool{}
		for _, lib := range libs {
			podcasts[lib.ID] = lib.Kind == catalog.KindPodcasts
		}
		next, ok := NextPodcast(queue, c.ref, podcasts)
		if !ok {
			return
		}
		states, err := p.Bridge.PodcastFetchStatuses(ctx, next.LibraryID, []string{next.Path})
		if err != nil {
			return
		}
		st, ok := states[next.Path]
		if !ok || st.State != "absent" {
			return
		}
		// A queue replacement while we were reading must not prepare an episode for
		// a different current item. This check also caps a user/current pair.
		p.mu.Lock()
		current := p.users[who.ID] == c && !c.chosen
		p.mu.Unlock()
		if !current {
			return
		}
		pin, err := p.Catalog.PodcastPin(ctx, next.LibraryID, next.Path)
		if err != nil {
			return
		}
		raw, _ := json.Marshal(bridge.EpisodeParams{ShowID: pin.ShowID, EpisodeID: pin.EpisodeID})
		_, _, err = p.Bridge.EnsurePodcastFetch(ctx, bridge.NewJob{LibraryID: &next.LibraryID, Path: &next.Path, Params: raw, Priority: 10, RequestedBy: who.ID}, bridge.FetchOptions{})
		chosen = err == nil
	}()
}

// NextPodcast chooses from the already scoped queue, skipping book entries.
func NextPodcast(queue []catalog.ListItem, current catalog.Ref, podcasts map[int64]bool) (catalog.Ref, bool) {
	start := 0
	for i, item := range queue {
		if item.Ref == current {
			start = i + 1
			break
		}
	}
	for _, item := range queue[start:] {
		if item.Ref != current && item.Book != nil && podcasts[item.LibraryID] {
			return item.Ref, true
		}
	}
	return catalog.Ref{}, false
}

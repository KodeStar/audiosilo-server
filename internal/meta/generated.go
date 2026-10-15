package meta

import (
	"strings"

	"github.com/kodestar/audiosilo-meta/pkg/model"
)

// Generated content is the characters and recaps the Bridge generated for a book
// on this server (its .audiosilo/ attachments, BRIDGE-PLAN.md Phase 1a). /meta
// serves it beside the community data, per request and never cached: community
// data wins per dimension, generated content fills what the community work
// lacks (or is the whole work of a book with no match), and it never carries the
// community's CC BY-SA attribution.

// Generated is a book's generated content as the catalog stores it: nil for a
// file it doesn't have. GeneratedAt is when it was generated (RFC3339; "" when
// unknown).
type Generated struct {
	Characters  *model.Characters
	Recaps      *model.Recaps
	GeneratedAt string
}

// MetaGenerated says which parts of a work came from the server's generated
// files rather than the community (MetaWork.Generated); omitted when none did.
type MetaGenerated struct {
	Characters  bool   `json:"characters"`
	Recaps      bool   `json:"recaps"`
	GeneratedAt string `json:"generated_at,omitempty"`
	// summary: the recap_summary came from the generated recaps too. Only with
	// Recaps (a community summary is kept beside generated recaps), and never on
	// the wire: the player marks recaps as one dimension.
	summary bool
}

// Unmatched is /meta's answer for a book with no community match: matched false,
// with the generated work when the book has generated content. A shipped player
// reads only matched, so the work is additive.
type Unmatched struct {
	Matched bool      `json:"matched"`
	Work    *MetaWork `json:"work,omitempty"`
}

// characters is g's characters as the envelope carries them (none for none).
func (g *Generated) characters() []MetaCharacter {
	if g == nil || g.Characters == nil || len(g.Characters.Characters) == 0 {
		return nil
	}
	out := make([]MetaCharacter, 0, len(g.Characters.Characters))
	for _, c := range g.Characters.Characters {
		out = append(out, MetaCharacter{
			ID:          c.ID,
			Name:        c.Name,
			Aliases:     c.Aliases,
			Role:        c.Role,
			Reveal:      MetaPosition(c.Reveal),
			Description: c.Description,
		})
	}
	return out
}

// recaps is g's recaps and whole-book summary as the envelope carries them (nil
// for none; a blank summary is none, as toRecapSummary drops one).
func (g *Generated) recaps() ([]MetaRecap, *MetaRecapSummary) {
	if g == nil || g.Recaps == nil {
		return nil, nil
	}
	var out []MetaRecap
	for _, r := range g.Recaps.Recaps {
		out = append(out, MetaRecap{Through: MetaPosition(r.Through), Scope: r.Scope, Text: r.Text})
	}
	var summary *MetaRecapSummary
	if strings.TrimSpace(g.Recaps.InShort) != "" || strings.TrimSpace(g.Recaps.Ending) != "" {
		summary = &MetaRecapSummary{InShort: g.Recaps.InShort, Ending: g.Recaps.Ending}
	}
	return out, summary
}

// fill lays g over w (a copy the caller owns) where w has nothing, per
// dimension: characters when w has none; recaps when w has none, and then the
// summary too when w has none. It records what it took in w.Generated (nil when
// nothing) and keeps w's attribution only while w still holds community content.
func (g *Generated) fill(w *MetaWork) {
	gen := MetaGenerated{}
	if chars := g.characters(); len(w.Characters) == 0 && len(chars) > 0 {
		w.Characters, gen.Characters = chars, true
	}
	if recaps, summary := g.recaps(); len(w.Recaps) == 0 && (len(recaps) > 0 || summary != nil) {
		w.Recaps, gen.Recaps = recaps, len(recaps) > 0
		if w.RecapSummary == nil && summary != nil {
			w.RecapSummary, gen.summary, gen.Recaps = summary, true, true
		}
	}
	w.Generated = nil
	if gen.Characters || gen.Recaps {
		gen.GeneratedAt = g.GeneratedAt
		w.Generated = &gen
	}
	if !carriesCommunityContent(w) {
		w.Attribution = nil
	}
}

// WithGenerated returns env with g's content filling what its work lacks
// (community wins per dimension; MetaWork.Generated names what was filled). env
// is shared with the cache and never modified: the work is copied when anything
// is filled, else env itself is returned.
func WithGenerated(env *Enrichment, g *Generated) *Enrichment {
	if env == nil || env.Work == nil || g == nil {
		return env
	}
	w := *env.Work
	g.fill(&w)
	if w.Generated == nil {
		return env
	}
	out := *env
	out.Work = &w
	return &out
}

// GeneratedWork is the work /meta answers for a book with no community match
// from its generated content alone: no id, the book's title, the generated
// characters, recaps and summary, and no attribution. Nil when g holds none.
func GeneratedWork(title string, g *Generated) *MetaWork {
	if g == nil {
		return nil
	}
	w := &MetaWork{Title: title, Authors: []MetaPersonRef{}}
	g.fill(w)
	if w.Generated == nil {
		return nil
	}
	return w
}

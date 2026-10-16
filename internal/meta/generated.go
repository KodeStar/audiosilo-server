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
// community's CC BY-SA attribution. It is kept apart from the community work
// until the very end (Answer): which dimensions it fills is decided from the
// community work as published, each side is gated for spoilers on its own, and
// only then are they laid together, so nothing that reads the community work
// (attribution, spoiler gating) ever sees generated content.

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
// Recaps covers the recap list and the whole-book summary alike.
type MetaGenerated struct {
	Characters  bool   `json:"characters"`
	Recaps      bool   `json:"recaps"`
	GeneratedAt string `json:"generated_at,omitempty"`
}

// Unmatched is /meta's answer for a book with no community match: matched false,
// with the generated work when the book has generated content. A shipped player
// reads only matched, so the work is additive.
type Unmatched struct {
	Matched bool      `json:"matched"`
	Work    *MetaWork `json:"work,omitempty"`
}

// Place is where a listener is in a book, for ?spoilers=hide (ChapterAt).
type Place struct {
	Chapter  int
	Finished bool
}

// Fillable reports whether generated content could fill anything in e's work
// (parts): it lacks characters or recaps (or e has no match at all); a summary
// is only ever taken with the recaps. When it can't, /meta needn't load the
// book's generated content.
func (e *Enrichment) Fillable() bool {
	return e == nil || e.Work == nil || len(e.Work.Characters) == 0 || len(e.Work.Recaps) == 0
}

// Answer is /meta's per-request answer for a book: env is its matched envelope
// (a per-request copy, rails placed and previous works added; nil when the book
// has no match), title the book's own, g its generated content (nil for none)
// and hide, when set, the listener's place to gate spoilers at. A matched book
// answers env with g filling what its work lacks (MetaWork.Generated names
// what); an unmatched one answers Unmatched, with a work of g's alone (the
// book's title, no id, no attribution) when g holds anything once gated. env is
// never modified.
func Answer(env *Enrichment, title string, g *Generated, hide *Place) any {
	var community *MetaWork
	if env != nil {
		community = env.Work
	}
	gen := g.parts(community)
	if hide != nil {
		env = HideSpoilers(env, hide.Chapter, hide.Finished)
		gen = GateWork(gen, hide.Chapter, hide.Finished)
	}
	if env == nil {
		if gen == nil {
			return Unmatched{}
		}
		w := &MetaWork{Title: title, Authors: []MetaPersonRef{}}
		gen.layInto(w, g.GeneratedAt)
		if w.Generated == nil {
			// The spoiler gate left nothing (every character revealed later, every
			// recap beyond the listener): no work of a title alone.
			return Unmatched{}
		}
		return Unmatched{Work: w}
	}
	if env.Work == nil || gen == nil {
		return env
	}
	w := *env.Work
	gen.layInto(&w, g.GeneratedAt)
	if w.Generated == nil {
		return env
	}
	out := *env
	out.Work = &w
	return &out
}

// parts is the generated content community (the work as published, nil for
// none) takes, as a work of its own holding just those parts: the characters
// when community has none; the recaps when it has none, and then the summary
// too when it has none. Nil when it takes nothing.
func (g *Generated) parts(community *MetaWork) *MetaWork {
	if g == nil {
		return nil
	}
	var c MetaWork
	if community != nil {
		c = *community
	}
	w := &MetaWork{}
	if len(c.Characters) == 0 {
		w.Characters = g.characters()
	}
	if len(c.Recaps) == 0 {
		w.Recaps = g.recaps()
		if c.RecapSummary == nil {
			w.RecapSummary = g.summary()
		}
	}
	if len(w.Characters) == 0 && len(w.Recaps) == 0 && w.RecapSummary == nil {
		return nil
	}
	return w
}

// layInto lays the generated parts gen holds (parts, gated as asked) into w, a
// copy the caller owns, naming them in w.Generated (left nil when gen holds
// nothing any more).
func (gen *MetaWork) layInto(w *MetaWork, generatedAt string) {
	var mark MetaGenerated
	if len(gen.Characters) > 0 {
		w.Characters, mark.Characters = gen.Characters, true
	}
	if len(gen.Recaps) > 0 {
		w.Recaps, mark.Recaps = gen.Recaps, true
	}
	if gen.RecapSummary != nil {
		w.RecapSummary, mark.Recaps = gen.RecapSummary, true
	}
	if mark.Characters || mark.Recaps {
		mark.GeneratedAt = generatedAt
		w.Generated = &mark
	}
}

// characters is g's characters as the envelope carries them (none for none).
func (g *Generated) characters() []MetaCharacter {
	if g.Characters == nil {
		return nil
	}
	var out []MetaCharacter
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

// recaps is g's recaps as the envelope carries them (none for none).
func (g *Generated) recaps() []MetaRecap {
	if g.Recaps == nil {
		return nil
	}
	var out []MetaRecap
	for _, r := range g.Recaps.Recaps {
		out = append(out, MetaRecap{Through: MetaPosition(r.Through), Scope: r.Scope, Text: r.Text})
	}
	return out
}

// summary is g's whole-book summary, nil when blank (as toRecapSummary drops
// one).
func (g *Generated) summary() *MetaRecapSummary {
	if g.Recaps == nil || (strings.TrimSpace(g.Recaps.InShort) == "" && strings.TrimSpace(g.Recaps.Ending) == "") {
		return nil
	}
	return &MetaRecapSummary{InShort: g.Recaps.InShort, Ending: g.Recaps.Ending}
}

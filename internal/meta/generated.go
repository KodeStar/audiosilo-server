package meta

import (
	"strings"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Generated content is the characters, recaps and description the Bridge
// generated for a book on this server (its .audiosilo/ attachments,
// BRIDGE-PLAN.md Phases 1a and 1c). /meta
// serves it beside the community data, per request and never cached: community
// data wins per dimension, generated content fills what the community work
// lacks (or is the whole work of a book with no match), and it never carries the
// community's CC BY-SA attribution. It is kept apart from the community work
// until the very end (Answer): which dimensions it fills is decided from the
// community work as published, each side is gated for spoilers on its own, and
// only then are they laid together, so nothing that reads the community work
// (attribution, spoiler gating) ever sees generated content.

// Generated is a book's generated content as the catalog stores it: nil for a
// file it doesn't have. GeneratedAt is when its characters and recaps were
// generated and DescriptionAt when its description was written (RFC3339; ""
// when unknown): a description-only job keeps the earlier run's time as
// bridge.json's generated_at.
type Generated struct {
	Characters    *model.Characters
	Recaps        *model.Recaps
	Description   *model.Description
	GeneratedAt   string
	DescriptionAt string
}

// MetaGenerated says which parts of a work came from the server's generated
// files rather than the community (MetaWork.Generated); omitted when none did.
// Recaps is set when the recap list or the whole-book summary was generated
// (as before RecapSummary existed, so an older client reads it unchanged);
// RecapSummary, additive, says the summary itself was, so a client can credit a
// community summary kept beside a generated list (a summary-only community
// work, which fillable leaves the list to fill) to the community. Description
// names the work's generated_description. GeneratedAt is when what it names
// was generated: the characters' and recaps' run, or the description's own
// time when the description is all it names.
type MetaGenerated struct {
	Characters   bool   `json:"characters"`
	Recaps       bool   `json:"recaps"`
	RecapSummary bool   `json:"recap_summary,omitempty"`
	Description  bool   `json:"description"`
	GeneratedAt  string `json:"generated_at,omitempty"`
}

// MetaGeneratedDescription is a book's own-words description generated on this
// server (MetaWork.GeneratedDescription). It is never the community's
// community_description: a shipped 2.0 app shows that one first and credits it
// to the community, so a generated text rides in its own field, which those apps
// ignore. Spoiler-free by construction (written from the opening chapters'
// notes only), so ?spoilers=hide keeps it.
type MetaGeneratedDescription struct {
	Text string `json:"text"`
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

// fillable is which generated parts could fill the community work c (the work
// as published; nil for no match, which every part fills): characters when it
// has none, recaps when it has no recap list (a summary is only ever taken with
// the recaps), a description when it has no community description (its core
// description, the publisher's kind, doesn't block it: the player orders
// those). The one statement of the rule: /meta loads by it (Fillable) and lays
// in by it (parts), so a part is never laid in without being loaded.
func fillable(c *MetaWork) metadata.AttachmentParts {
	if c == nil {
		return metadata.AllAttachmentParts
	}
	return metadata.AttachmentParts{
		Characters:  len(c.Characters) == 0,
		Recaps:      len(c.Recaps) == 0,
		Description: c.CommunityDescription == nil,
	}
}

// Fillable is fillable for e's work: the parts of the book's generated content
// /meta loads (none when it is none).
func (e *Enrichment) Fillable() metadata.AttachmentParts {
	if e == nil {
		return fillable(nil)
	}
	return fillable(e.Work)
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
		gen.layInto(w, g)
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
	gen.layInto(&w, g)
	if w.Generated == nil {
		return env
	}
	out := *env
	out.Work = &w
	return &out
}

// parts is the generated content community (the work as published, nil for
// none) takes, as a work of its own holding just those parts: each part
// fillable names, and with the recaps the summary too when community has none.
// Nil when it takes nothing.
func (g *Generated) parts(community *MetaWork) *MetaWork {
	if g == nil {
		return nil
	}
	fill := fillable(community)
	w := &MetaWork{}
	if fill.Characters {
		w.Characters = g.characters()
	}
	if fill.Recaps {
		w.Recaps = g.recaps()
		if community == nil || community.RecapSummary == nil {
			w.RecapSummary = g.summary()
		}
	}
	if fill.Description {
		w.GeneratedDescription = g.description()
	}
	if len(w.Characters) == 0 && len(w.Recaps) == 0 && w.RecapSummary == nil && w.GeneratedDescription == nil {
		return nil
	}
	return w
}

// layInto lays the generated parts gen holds (g's parts, gated as asked) into
// w, a copy the caller owns, naming them in w.Generated (left nil when gen
// holds nothing any more), dated by what it names: g's run, or the
// description's own time when that is all.
func (gen *MetaWork) layInto(w *MetaWork, g *Generated) {
	var mark MetaGenerated
	if len(gen.Characters) > 0 {
		w.Characters, mark.Characters = gen.Characters, true
	}
	if len(gen.Recaps) > 0 {
		w.Recaps, mark.Recaps = gen.Recaps, true
	}
	if gen.RecapSummary != nil {
		w.RecapSummary, mark.Recaps, mark.RecapSummary = gen.RecapSummary, true, true
	}
	if gen.GeneratedDescription != nil {
		w.GeneratedDescription, mark.Description = gen.GeneratedDescription, true
	}
	if mark.Characters || mark.Recaps || mark.Description {
		mark.GeneratedAt = g.GeneratedAt
		if !mark.Characters && !mark.Recaps {
			mark.GeneratedAt = g.DescriptionAt
		}
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

// description is g's description as the envelope carries it, nil when blank.
// metadata.ParseDescription already stores the text trimmed and drops a blank
// one; the trim here is deliberate defence, as summary's is, so a Generated
// built any other way can't send a blank description.
func (g *Generated) description() *MetaGeneratedDescription {
	if g.Description == nil || strings.TrimSpace(g.Description.Text) == "" {
		return nil
	}
	return &MetaGeneratedDescription{Text: g.Description.Text}
}

package matchrun

import (
	"slices"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// ProposalFields are the community values to compare and the default writes.
// Reverting a stale community series restores the book's scanned value.
type ProposalFields struct {
	Values map[string]string `json:"values"`
	Set    map[string]string `json:"set"`
	Revert []string          `json:"revert,omitempty"`
}

type BookProposal struct {
	ProposalFields
	WorkID      string `json:"work_id"`
	RecordingID string `json:"recording_id"`
	CoverURL    string `json:"cover_url"`
	Cover       bool   `json:"cover"`
	Match       bool   `json:"match"`
	// Position and other series if the admin changes the main series checkbox.
	AlternateSeries *ProposalFields `json:"alternate_series,omitempty"`
}

func (p *ProposalFields) offer(field, value string) {
	if value == "" {
		delete(p.Values, field)
	} else {
		p.Values[field] = value
	}
}

// Preview is the match dialog's comparison for its chosen work and recording.
func Preview(st *catalog.MatchState, c *meta.MatchCandidate, rec *meta.MatchRecording) BookProposal {
	p := Propose(c, rec)
	w, series := planBook(ScopeOverwrite, st, p)
	result := BookProposal{ProposalFields: ProposalFields{Values: p.Values, Set: w.Set, Revert: w.Revert},
		WorkID: p.WorkID, RecordingID: p.RecordingID, CoverURL: p.CoverURL, Cover: w.Cover, Match: w.Anchor != nil}
	result.offer(catalog.FieldSeries, series.name)
	result.offer(catalog.FieldSeriesIndex, series.position)
	result.offer(catalog.FieldMoreSeries, series.more)

	// The main series checkbox can either set an offered name or revert a
	// stale community override. Its dependants are planned beside that choice.
	main := w.after(st, catalog.FieldSeries)
	alternate := series.name
	if w.Set[catalog.FieldSeries] != "" || slices.Contains(w.Revert, catalog.FieldSeries) {
		alternate = st.Fields[catalog.FieldSeries].Value
	} else if alternate == "" {
		return result
	}
	if alternate != main {
		other := Writes{Set: map[string]string{}}
		pos, more := planSeriesDetails(ScopeOverwrite, st, series.work, alternate, &other)
		fields := &ProposalFields{Values: map[string]string{}, Set: other.Set, Revert: other.Revert}
		fields.offer(catalog.FieldSeriesIndex, pos)
		fields.offer(catalog.FieldMoreSeries, more)
		result.AlternateSeries = fields
	}
	return result
}

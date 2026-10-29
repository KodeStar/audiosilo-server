package bridge

import (
	"encoding/json"
	"math"
)

// ReadalongCoverage is a finished readalong.align job's text coverage (its
// result's coverage.text: how much of the ebook's read body the map lines up,
// 0 to 1), for the message that says it is ready; ok is false for any other
// job, or a result that doesn't say (or says something out of range).
func (j *Job) ReadalongCoverage() (coverage float64, ok bool) {
	if j.Kind != KindReadalong || j.State != StateDone {
		return 0, false
	}
	var r struct {
		Coverage struct {
			Text *float64 `json:"text"`
		} `json:"coverage"`
	}
	if json.Unmarshal(j.Result, &r) != nil || r.Coverage.Text == nil {
		return 0, false
	}
	c := *r.Coverage.Text
	if math.IsNaN(c) || c < 0 || c > 1 {
		return 0, false
	}
	return c, true
}

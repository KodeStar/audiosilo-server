package metadata

import "testing"

func TestParseSeriesRange(t *testing.T) {
	cases := []struct {
		in         string
		start, end float64
		ok         bool
	}{
		{"1-3", 1, 3, true},
		{" 1 - 3 ", 1, 3, true},
		{"1–3", 1, 3, true}, // en dash
		{"2.5-4", 2.5, 4, true},
		{"1e-3-4", 0.001, 4, true}, // the dash inside a number is not the separator
		{"1-101", 1, 101, true},    // a span of 100
		{"1-102", 0, 0, false},     // a span over MaxSeriesSpan
		{"3", 0, 0, false},         // a number is no range
		{"3-3", 0, 0, false},       // end must be above start
		{"3-1", 0, 0, false},
		{"0-2", 0, 0, false}, // 0 is no position
		{"-1-3", 0, 0, false},
		{"99999-100001", 0, 0, false}, // above MaxSeriesPosition
		{"1-NaN", 0, 0, false},
		{"1-Inf", 0, 0, false},
		{"1-3-5", 0, 0, false},
		{"a-b", 0, 0, false},
		{"", 0, 0, false},
		{"1—3", 0, 0, false}, // an em dash is not a separator
	}
	for _, c := range cases {
		start, end, ok := ParseSeriesRange(c.in)
		if start != c.start || end != c.end || ok != c.ok {
			t.Errorf("ParseSeriesRange(%q) = %v, %v, %v; want %v, %v, %v", c.in, start, end, ok, c.start, c.end, c.ok)
		}
	}
}

func TestParseSeriesPosition(t *testing.T) {
	for in, want := range map[string][3]float64{
		"3": {3, 0, 1}, " 3 ": {0, 0, 0}, "2.5": {2.5, 0, 1}, "1-3": {1, 3, 1}, " 1 - 3 ": {1, 3, 1},
		"": {0, 0, 0}, "one": {0, 0, 0}, "3-1": {0, 0, 0},
	} {
		start, end, ok := ParseSeriesPosition(in)
		if start != want[0] || end != want[1] || ok != (want[2] == 1) {
			t.Errorf("ParseSeriesPosition(%q) = %v, %v, %v; want %v", in, start, end, ok, want)
		}
	}
}

func TestApplyProbeSeriesPartRange(t *testing.T) {
	m := &Metadata{}
	applyProbe(m, &probeResult{Tags: map[string]string{"series": "Omnibus", "series-part": "1-3"}})
	if m.SeriesIndex != 1 || m.SeriesIndexEnd != 3 {
		t.Errorf("series-part 1-3 = %v-%v, want 1-3", m.SeriesIndex, m.SeriesIndexEnd)
	}
	m = &Metadata{}
	applyProbe(m, &probeResult{Tags: map[string]string{"series": "Omnibus", "series-part": "one"}})
	if m.SeriesIndex != 0 || m.SeriesIndexEnd != 0 {
		t.Errorf("series-part one = %v-%v, want none", m.SeriesIndex, m.SeriesIndexEnd)
	}
}

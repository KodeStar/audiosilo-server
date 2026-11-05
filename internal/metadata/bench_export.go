//go:build benchharness

package metadata

// ProbeTagsForBench is the Metadata a scan reads from a file whose probe
// answered tags and seconds (applyProbe): for the matcher's benchmark harness
// (internal/matchrun, build tag benchharness), which replays recorded ffprobe
// tag maps without the files. Never built into the server.
func ProbeTagsForBench(tags map[string]string, seconds float64) *Metadata {
	m := &Metadata{}
	applyProbe(m, &probeResult{Duration: seconds, Tags: normalizeTags(tags)})
	return m
}

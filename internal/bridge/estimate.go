package bridge

import "math"

// SidecarsCost is what a sidecars.generate job is expected to cost, from the
// Bridge's own figures (its last heartbeat's estimates): the confirmation
// dialog's numbers, as GET /admin/bridge/estimate carries them.
type SidecarsCost struct {
	// Source is what the run will read: SourceEbook when an ebook sits beside
	// the audio, else SourceAudio.
	Source string `json:"source"`
	// ASRHours is the ASR wall time in hours (0 for an ebook, which needs none,
	// when the Bridge sent no figures, and when the audio's length is unknown).
	ASRHours float64 `json:"asr_hours"`
	// AgentCostUSD is the agent's expected spend; nil (null on the wire) when the
	// Bridge has no history for this source yet (or sent no figures), or the
	// audio's length is unknown: no estimate, never a free run.
	AgentCostUSD *float64 `json:"agent_cost_usd"`
	// Subscription, AgentReady and CanContribute are the Bridge's: the agent's
	// credential is a subscription login (the cost is notional), the agent is
	// ready to run, and the Bridge can offer the results to the community (a
	// job may ask for contribute "issue").
	Subscription  bool `json:"subscription"`
	AgentReady    bool `json:"agent_ready"`
	CanContribute bool `json:"can_contribute"`
	// Figures: the Bridge has sent its figures (its heartbeat's estimates).
	// Without them every field above is only a default, so a false agent_ready
	// means "not known yet", not "not ready".
	Figures bool `json:"figures"`
}

// EstimateSidecars costs a sidecars.generate run over a book of duration
// seconds, reading its ebook when it has one, from the Bridge's figures (nil:
// it sent none). Hours and dollars are rounded to hundredths.
func EstimateSidecars(duration float64, ebook bool, est *SidecarsEstimates) SidecarsCost {
	c := SidecarsCost{Source: SourceAudio}
	if ebook {
		c.Source = SourceEbook
	}
	if est == nil {
		return c
	}
	c.Figures = true
	c.Subscription, c.AgentReady, c.CanContribute = est.Subscription, est.AgentReady, est.CanContribute
	if ebook {
		if est.AgentUSDPerEbook != nil {
			c.AgentCostUSD = new(cents(*est.AgentUSDPerEbook))
		}
		return c
	}
	if duration <= 0 {
		return c // audio of unknown length (no probe): nothing to cost it by
	}
	hours := duration / 3600
	c.ASRHours = cents(hours * est.ASRSecondsPerAudioHour / 3600)
	if est.AgentUSDPerAudioHour != nil {
		c.AgentCostUSD = new(cents(hours * *est.AgentUSDPerAudioHour))
	}
	return c
}

// cents is v rounded to two decimal places.
func cents(v float64) float64 { return math.Round(v*100) / 100 }

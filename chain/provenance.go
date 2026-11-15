package chain

import (
	"math"

	"github.com/ionalpha/flynn/spine"
)

// The provenance vocabulary of a run driven by an external agent harness (a CLI
// with its own inner agentic loop). A native run records none of it. An external
// run records one ProvenanceDeclared event: its effects passed through the
// governed dispatch waist and are enforced, but the harness's inner reasoning is
// outside the run's tracing and declared unobserved. The record names that gap
// instead of claiming a native run's integrity. These strings are the wire
// contract between producer and this package and must not change.
const (
	// ProvenanceDeclared marks an external-harness run and its tiers.
	ProvenanceDeclared = "provenance.declared"

	// ProvenanceHarnessKey names the harness (for example "codex").
	ProvenanceHarnessKey = "harness"
	// ProvenanceEffectsKey is the tier of the run's tool calls. They reach the
	// workspace only through the loopback bridge, so they are enforced at the
	// same waist as a native loop.
	ProvenanceEffectsKey = "effects"
	// ProvenanceReasoningKey is the tier of the harness's inner reasoning, which
	// is outside the run's tracing.
	ProvenanceReasoningKey = "reasoning"
	// ProvenanceReplayableKey is false for an external harness: replay is only
	// at episode level.
	ProvenanceReplayableKey = "replayable"
	// ProvenanceAttestedKey counts events the harness reported about itself. The
	// run has only its account, so they are attested, not enforced.
	ProvenanceAttestedKey = "attested_events"
	// ProvenanceDriftKey maps each failed conformance probe to the episodes it
	// failed on. The harness's own prompt outranks the run's, so the behavioral
	// contract is a request. Absent or empty means it was honored throughout.
	ProvenanceDriftKey = "probe_failures"
	// ProvenanceNativeRateKey is the share, in [0,1], of tool attempts that used
	// the harness's built-in tools instead of the governed bridged ones. Those
	// effects could not land, but what they read the run never saw, so a high rate
	// means more unobserved activity behind the same signature.
	ProvenanceNativeRateKey = "native_tool_rate"

	// TierEnforced: every effect passed the governed dispatch waist.
	TierEnforced = "enforced"
	// TierUnobserved: outside the run's tracing, named as a gap.
	TierUnobserved = "unobserved"
	// TierAttested: the harness's own account, repeated without vouching.
	TierAttested = "attested"
)

// Provenance is absent for a native run, which is fully enforced and replayable.
type Provenance struct {
	Harness string
	// Effects is TierEnforced for an external run.
	Effects string
	// Reasoning is TierUnobserved for an external run.
	Reasoning string
	// Replayable is false for an external harness.
	Replayable bool
	// AttestedEvents are the harness's account of itself, not vouched for; the
	// enforced effects are recorded separately.
	AttestedEvents int
	// Drift maps each failed probe to its failing episodes; empty means none.
	Drift map[string]int
	// NativeToolRate is in [0,1].
	NativeToolRate float64
}

// ProvenanceOf returns the declaration on a run's stream; a native run has none,
// so ok is false. The events must already be verified and in order, as the
// governance and ground-truth verifiers assume.
func ProvenanceOf(events []spine.Event) (Provenance, bool) {
	for _, e := range events {
		if e.Type != ProvenanceDeclared {
			continue
		}
		p := Provenance{}
		p.Harness, _ = e.Payload[ProvenanceHarnessKey].(string)
		p.Effects, _ = e.Payload[ProvenanceEffectsKey].(string)
		p.Reasoning, _ = e.Payload[ProvenanceReasoningKey].(string)
		p.Replayable, _ = e.Payload[ProvenanceReplayableKey].(bool)
		p.AttestedEvents = payloadInt(e.Payload[ProvenanceAttestedKey])
		p.NativeToolRate = payloadFloat(e.Payload[ProvenanceNativeRateKey])
		p.Drift = payloadCounts(e.Payload[ProvenanceDriftKey])
		return p, true
	}
	return Provenance{}, false
}

// payloadFloat accepts the float64 of a JSON round trip and an int.
func payloadFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}

// payloadCounts accepts both nested-map shapes: JSON yields map[string]any,
// canonical CBOR map[any]any.
func payloadCounts(v any) map[string]int {
	switch m := v.(type) {
	case map[string]any:
		if len(m) == 0 {
			return nil
		}
		out := make(map[string]int, len(m))
		for name, raw := range m {
			out[name] = payloadInt(raw)
		}
		return out
	case map[any]any:
		if len(m) == 0 {
			return nil
		}
		out := make(map[string]int, len(m))
		for name, raw := range m {
			s, ok := name.(string)
			if !ok {
				continue
			}
			out[s] = payloadInt(raw)
		}
		return out
	default:
		return nil
	}
}

// payloadInt accepts int (in memory), float64 (the JSON store) and uint64
// (canonical CBOR); anything else reads as zero.
func payloadInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case uint64:
		// A record may be sealed by another implementation. Clamp rather than wrap
		// to a negative.
		if n > math.MaxInt {
			return math.MaxInt
		}
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

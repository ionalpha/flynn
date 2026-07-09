package chain

import (
	"math"

	"github.com/ionalpha/flynn/spine"
)

// The provenance vocabulary a record carries when a run's loop was driven by an
// external agent harness (a CLI that runs its own inner agentic loop) rather than a
// native model conversation. A native run records none of this. An external run
// records one ProvenanceDeclared event stating which tiers the sealed record can
// vouch for: its effects passed through the governed dispatch waist and are enforced,
// but the harness's inner reasoning (its own model calls, the context it compacts, its
// direct channel to its provider) is structurally outside the run's tracing and is
// declared unobserved. So an external-harness run never claims the integrity of a
// native run: the record is signed and its effects are enforced, but it names the gap
// rather than hiding it. These string values are the wire contract a producer writes
// and this package reads, so they must not change.
const (
	// ProvenanceDeclared marks a run driven by an external agent harness and declares
	// the provenance tiers its record vouches for.
	ProvenanceDeclared = "provenance.declared"

	// ProvenanceHarnessKey names the external harness that drove the run (for example
	// "codex").
	ProvenanceHarnessKey = "harness"
	// ProvenanceEffectsKey is the tier the run's effects (its tool calls) were recorded
	// at. An external harness's effects reach the workspace only through the loopback
	// bridge, so they are enforced at the same waist as a native loop.
	ProvenanceEffectsKey = "effects"
	// ProvenanceReasoningKey is the tier the harness's inner reasoning was recorded at.
	// It is unobserved: an external harness's inner model calls and context are outside
	// the run's tracing.
	ProvenanceReasoningKey = "reasoning"
	// ProvenanceReplayableKey is false for an external-harness run: deterministic replay
	// does not apply to a harness the run does not drive, so replay granularity degrades
	// to the episode level.
	ProvenanceReplayableKey = "replayable"
	// ProvenanceAttestedKey is how many events the external harness reported about its
	// own episode. The run did not observe them at the waist; it has only the harness's
	// account, so they are attested rather than enforced.
	ProvenanceAttestedKey = "attested_events"
	// ProvenanceDriftKey maps each conformance probe the harness failed to how many of the
	// run's episodes it failed on. An external harness's own prompt outranks the run's, so
	// the behavioral contract is a request; this records where the request was ignored.
	// Absent or empty means the harness honored the contract on every episode.
	ProvenanceDriftKey = "probe_failures"
	// ProvenanceNativeRateKey is the share of the harness's tool attempts that reached for
	// its own built-in tools instead of the governed bridged ones, in [0,1]. Those effects
	// could not land, but what the harness read with them the run never saw, so a high rate
	// means a larger unobserved surface behind the same signature.
	ProvenanceNativeRateKey = "native_tool_rate"

	// TierEnforced is the effects tier: every effect passed through the governed
	// dispatch waist, exactly like a native action.
	TierEnforced = "enforced"
	// TierUnobserved is the reasoning tier: the work is structurally outside the run's
	// tracing, named as a gap rather than covered.
	TierUnobserved = "unobserved"
	// TierAttested is the tier of an action the external harness reported about itself.
	// The run has the harness's account of it and nothing more, so the record repeats
	// the claim without vouching for it.
	TierAttested = "attested"
)

// Provenance is the declared provenance of a run: the harness that drove it and the
// tiers its sealed record can vouch for. It is absent for a native run, which is fully
// enforced and replayable and needs no declaration.
type Provenance struct {
	// Harness is the external harness that drove the run (for example "codex").
	Harness string
	// Effects is the tier the run's effects were recorded at (TierEnforced for an
	// external run, since every effect crossed the dispatch waist).
	Effects string
	// Reasoning is the tier the harness's inner reasoning was recorded at
	// (TierUnobserved for an external run).
	Reasoning string
	// Replayable reports whether the run supports deterministic replay (false for an
	// external harness the run does not drive).
	Replayable bool
	// AttestedEvents is how many events the harness reported about its own episode.
	// They are the harness's account of itself, repeated by the record but not vouched
	// for; the enforced effects are recorded separately at the dispatch waist.
	AttestedEvents int
	// Drift maps each conformance probe the harness failed to the number of episodes it
	// failed on. Empty means the harness honored the session contract throughout.
	Drift map[string]int
	// NativeToolRate is the share of the harness's tool attempts that reached for its own
	// tools rather than the governed bridged ones, in [0,1].
	NativeToolRate float64
}

// ProvenanceOf returns the provenance declaration recorded on a run's event stream and
// whether one is present. A native run carries no declaration, so ok is false and the
// caller reports the run as fully enforced. The events must already be authentic and in
// order (verify the record first), the same precondition the governance and
// ground-truth verifiers assume.
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

// payloadFloat reads a rate from an event payload, accepting the float64 a JSON round
// trip produces and the int a producer may have written for a whole number.
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

// payloadCounts reads a name-to-count map from an event payload. The two encodings a
// declaration is read back from disagree on the shape of a nested map as well as on its
// numbers: JSON yields map[string]any, while canonical CBOR yields map[any]any with the
// keys typed as any. Both are accepted, and the values go through payloadInt.
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

// payloadInt reads a count from an event payload. The same declaration is read back from
// two encodings, and each renders a whole number differently: the durable store round
// trips through JSON, where every number is a float64, while a sealed record round trips
// through canonical CBOR, where a non-negative number is a uint64. In memory it is still
// the int the producer wrote. All three are accepted; anything else reads as zero.
func payloadInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case uint64:
		// A record is untrusted input until it verifies, and even a verified one may have
		// been sealed by another implementation. A count that cannot fit is clamped rather
		// than wrapped to a negative, which would read as fewer attested events than none.
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

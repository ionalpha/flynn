package mission

import (
	"context"

	"github.com/ionalpha/flynn/llm"
)

// GenerationEnvelope is the reproducibility identity of a model call: the decoding parameters
// that, together with the model's weights, determine its output. It is recorded for every
// generation so the parameters that make a run reproducible are part of its durable history,
// and a recorded run can later be replayed and checked. The model and weights identity is
// recorded by the serving layer that knows it; this envelope carries the decoding half, and the
// two compose into a full, reproducible description of a generation.
type GenerationEnvelope struct {
	// Pinned is true when the run fixed its sampling, rather than taking the server's defaults.
	// A free-running generation is recorded as not pinned, so its non-reproducibility is itself
	// an honest part of the history.
	Pinned bool
	// Deterministic is true when the decoding is in the guaranteed-reproducible regime (greedy),
	// so a replay against the same weights must produce identical output.
	Deterministic bool
	// Seed, Temperature, and TopP are the pinned decoding parameters, normalized, or zero when
	// the run is free-running.
	Seed        int64
	Temperature float64
	TopP        float64
}

// envelopeOf builds the envelope recorded for a request's sampling. A nil sampling is a
// free-running call and yields the zero envelope (not pinned).
func envelopeOf(s *llm.Sampling) GenerationEnvelope {
	if s == nil {
		return GenerationEnvelope{}
	}
	n := s.Normalized()
	return GenerationEnvelope{
		Pinned:        true,
		Deterministic: n.Deterministic(),
		Seed:          n.Seed,
		Temperature:   n.Temperature,
		TopP:          n.TopP,
	}
}

// GenerationRecorder records the decoding envelope of each model call onto a run's durable
// history, so the parameters that make a run reproducible are not lost. It is a narrow port,
// kept separate from the dispatch waist (which governs an action's metadata, not a model call's
// typed request) so the envelope is a domain event in its own right rather than waist payload.
//
// Flynn ships no implementation but the discarding one, and that is deliberate rather than
// an unfinished wire. Nothing in the binary calls WithSampling, so every model call a Flynn
// run makes is free-running, and the envelope such a run would record is the zero envelope:
// Pinned false, no seed, no temperature, on every single call. Writing that to the sealed
// stream once per model call would put a constant on the record and call it evidence, and
// the fact it encodes ("this run did not pin its sampling") is one fact about the whole run
// rather than one per turn.
//
// The port stays because the question it answers is real and the answer can change. Pin a
// run's sampling and the envelope stops being a constant: it becomes the half of a
// generation's identity the serving layer does not carry, and a replay that cannot say what
// seed and temperature a call ran under is missing something a verifier would want. The
// order matters, though. Deciding Flynn's default sampling posture comes first; a recorder
// wired ahead of it records only the absence of one.
//
// So: a host that pins its own sampling wires this to its event spine and gets a complete
// decoding record. A standalone Flynn run does not, and does not pretend to.
type GenerationRecorder interface {
	RecordGeneration(ctx context.Context, env GenerationEnvelope)
}

// nopGenerationRecorder discards every envelope. It is the standalone default for the reason
// GenerationRecorder gives: with no run pinning its sampling, the envelope carries no
// per-call information, and a recorder writing the same "not pinned" to the record on every
// turn would be volume rather than evidence.
type nopGenerationRecorder struct{}

// RecordGeneration implements GenerationRecorder by doing nothing.
func (nopGenerationRecorder) RecordGeneration(context.Context, GenerationEnvelope) {}

var _ GenerationRecorder = nopGenerationRecorder{}

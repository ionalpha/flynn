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
// typed request) so the envelope is a first-class domain event rather than waist payload. The
// default is a no-op; a host wires it to the event spine.
type GenerationRecorder interface {
	RecordGeneration(ctx context.Context, env GenerationEnvelope)
}

// nopGenerationRecorder discards every envelope; the zero-setup standalone default.
type nopGenerationRecorder struct{}

// RecordGeneration implements GenerationRecorder by doing nothing.
func (nopGenerationRecorder) RecordGeneration(context.Context, GenerationEnvelope) {}

var _ GenerationRecorder = nopGenerationRecorder{}

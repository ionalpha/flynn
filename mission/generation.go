package mission

import (
	"context"

	"github.com/ionalpha/flynn/llm"
)

// GenerationEnvelope is the decoding half of a model call's reproducibility identity; the
// serving side records the weights.
type GenerationEnvelope struct {
	// Pinned is false when the run took the server's sampling defaults.
	Pinned bool
	// Deterministic marks greedy decoding, so a replay on the same weights must match.
	Deterministic bool
	// Seed, Temperature and TopP are normalized, and zero when not pinned.
	Seed        int64
	Temperature float64
	TopP        float64
}

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

// GenerationRecorder records each call's envelope, apart from the dispatch waist, which
// carries no typed request. A host that pins its sampling wires it to its spine, so a
// replay can say what seed and temperature a call ran under.
//
// Flynn ships only a discarding recorder: the binary never pins sampling, and recording the
// same zero envelope every turn adds volume and no evidence.
type GenerationRecorder interface {
	RecordGeneration(ctx context.Context, env GenerationEnvelope)
}

type nopGenerationRecorder struct{}

func (nopGenerationRecorder) RecordGeneration(context.Context, GenerationEnvelope) {}

var _ GenerationRecorder = nopGenerationRecorder{}

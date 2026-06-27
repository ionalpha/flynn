package profilestore

import (
	"context"
	"encoding/json"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/resource"
)

// Source is a harness.ProfileSource backed by profiles in the resource store. It loads a snapshot
// once at construction and answers every lookup from memory, so the dispatch waist consults a
// model's capability without touching the store on the hot path. A model never measured is simply
// absent, which the harness treats as unknown and scaffolds most conservatively.
type Source struct {
	byModel map[string]harness.ModelProfile
}

var _ harness.ProfileSource = Source{}

// NewSource loads every stored profile and indexes it by model id. When a model has more than one
// profile, for instance several quantizations, the indexed profile is the element-wise minimum
// across them: the lookup does not know which quant will be served, so it reports the least
// capable, and the harness never under-scaffolds a model on the strength of a more capable build
// it might not be running. A store read error is returned; an empty store yields an empty source
// that reports every model as unknown.
func NewSource(ctx context.Context, rs resource.Store) (Source, error) {
	all, err := rs.ListAll(ctx, Kind, nil)
	if err != nil {
		return Source{}, err
	}
	byModel := make(map[string]harness.ModelProfile, len(all))
	for _, r := range all {
		var spec Spec
		if len(r.Spec) == 0 {
			continue
		}
		if err := json.Unmarshal(r.Spec, &spec); err != nil || spec.ModelID == "" {
			continue
		}
		p := spec.profile()
		if existing, ok := byModel[spec.ModelID]; ok {
			p = mostConservative(existing, p)
		}
		byModel[spec.ModelID] = p
	}
	return Source{byModel: byModel}, nil
}

// Profile returns the measured profile for a model, or false when none is stored.
func (s Source) Profile(model string) (harness.ModelProfile, bool) {
	p, ok := s.byModel[model]
	return p, ok
}

// mostConservative folds two profiles of one model into the least capable on every dimension, so
// a model with several measured builds is treated as no better than its weakest. A zero (unknown)
// effective context on either side stays unknown, the conservative reading.
func mostConservative(a, b harness.ModelProfile) harness.ModelProfile {
	return harness.ModelProfile{
		ToolCallReliability:  min(a.ToolCallReliability, b.ToolCallReliability),
		StructuredOutput:     min(a.StructuredOutput, b.StructuredOutput),
		InstructionFollowing: min(a.InstructionFollowing, b.InstructionFollowing),
		EffectiveContext:     minContext(a.EffectiveContext, b.EffectiveContext),
	}
}

// minContext is the tighter of two effective-context figures, treating zero (unknown) as the most
// constraining so an unmeasured window is never assumed to be the larger one.
func minContext(a, b int) int {
	switch {
	case a == 0 || b == 0:
		return 0
	default:
		return min(a, b)
	}
}

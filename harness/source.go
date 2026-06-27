package harness

// ProfileSource resolves the measured capability profile for a model, keyed by its selector
// (the provider:model or runtime:model id). A source that has no measurement for a model returns
// ok=false, and the caller treats the model as unknown, which yields the most conservative plan.
// It is the read side of the capability store: an evaluation harness writes profiles in, and the
// assembly reads them out here to decide how much to scaffold a run.
type ProfileSource interface {
	Profile(model string) (ModelProfile, bool)
}

// PlanFor resolves model's profile from src and maps it to a scaffolding plan against the
// advertised context window. A nil source, or a model the source has not measured, is treated as
// unknown: the zero profile, which Adapt turns into the fully scaffolded plan. This makes the
// safe default automatic, so a model is only driven leanly once it has been measured and found
// reliable, never on assumption.
func PlanFor(src ProfileSource, model string, advertisedContext int) Plan {
	var p ModelProfile
	if src != nil {
		if measured, ok := src.Profile(model); ok {
			p = measured
		}
	}
	return Adapt(p, advertisedContext)
}

// StaticProfiles is a ProfileSource backed by a fixed map, for seeding known profiles ahead of a
// live evaluation store. A nil or absent entry reads as unmeasured.
type StaticProfiles map[string]ModelProfile

// Profile returns the profile recorded for model, if any.
func (s StaticProfiles) Profile(model string) (ModelProfile, bool) {
	p, ok := s[model]
	return p, ok
}

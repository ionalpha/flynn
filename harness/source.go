package harness

// ProfileSource resolves a model's measured profile by selector (provider:model or
// runtime:model). ok=false means unmeasured, which yields the most conservative
// plan. An evaluation harness writes profiles; the assembly reads them here.
type ProfileSource interface {
	Profile(model string) (ModelProfile, bool)
}

// PlanFor maps model's profile from src to a plan. A nil source or unmeasured
// model gets the zero profile and the full scaffolding, so a model runs lean only
// once it has been measured as reliable.
func PlanFor(src ProfileSource, model string, advertisedContext int) Plan {
	var p ModelProfile
	if src != nil {
		if measured, ok := src.Profile(model); ok {
			p = measured
		}
	}
	return Adapt(p, advertisedContext)
}

// StaticProfiles is a ProfileSource over a fixed map, for seeding known profiles.
type StaticProfiles map[string]ModelProfile

// Profile returns the profile recorded for model, if any.
func (s StaticProfiles) Profile(model string) (ModelProfile, bool) {
	p, ok := s[model]
	return p, ok
}

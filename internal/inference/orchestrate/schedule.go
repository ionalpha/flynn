// Package orchestrate decides which local models stay resident in limited device memory.
// It is the control-plane policy over the serve manager: given the models that should be
// resident and the models currently served, it computes the launches and evictions that
// converge the two within a memory budget, without ever evicting a pinned or actively
// decoding model. The decision is a pure function, so the policy is exhaustively testable
// without a live runtime; the reconcile-driven controller that applies it is wired
// separately.
package orchestrate

import "sort"

// Desired is a model the controller should keep resident, with the inputs the policy needs
// to choose under memory pressure.
type Desired struct {
	// ModelID is the catalog id, the identity shared with the serve manager.
	ModelID string
	// Footprint is the estimated device memory the model occupies when resident, in bytes.
	// It comes from the model's known size, not a live reading, so a model can be budgeted
	// before it is launched. A negative value is treated as zero.
	Footprint int64
	// Priority orders models under memory pressure: a higher priority is kept, and a lower
	// one is evicted first.
	Priority int
	// Pinned keeps a model resident regardless of priority or budget, for a model that must
	// stay hot (a small default model, or a draft model for speculative decoding).
	Pinned bool
	// Draft, when set, is a small companion model paired with this one for speculative
	// decoding. It must be resident whenever its primary is, so the policy keeps and budgets
	// the two together: the primary is admitted only if both fit, and evicting the primary
	// evicts the draft. A nil Draft means the model serves on its own.
	Draft *Draft
}

// Draft is the companion model paired with a primary for speculative decoding. It is identified
// and budgeted on its own, but it is never scheduled independently: it rides with its primary.
type Draft struct {
	// ModelID is the draft model's catalog id.
	ModelID string
	// Footprint is the device memory it occupies when resident, in bytes. A negative value is
	// treated as zero.
	Footprint int64
}

// Resident is a model the serve manager currently runs.
type Resident struct {
	// ModelID is the catalog id.
	ModelID string
	// Footprint is the device memory it occupies, in bytes. A negative value is treated as
	// zero.
	Footprint int64
	// Pinned marks a model that must not be evicted.
	Pinned bool
	// Active is true when the model is currently decoding a request, so evicting it would
	// drop in-flight work; an active model is kept even when it is no longer desired.
	Active bool
	// LastUsed orders eviction among otherwise-equal candidates, least-recently-used first.
	// It is a logical or wall-clock stamp supplied by the caller.
	LastUsed int64
}

// Plan is the set of actions that converge the resident set toward the desired set. Applying
// it is idempotent: a resident set already equal to the chosen set yields an empty plan.
type Plan struct {
	// Launch lists model ids to start; every entry is a desired model not already resident.
	Launch []string
	// Evict lists model ids to stop; every entry is currently resident and neither pinned
	// nor active.
	Evict []string
	// Unschedulable lists desired models that did not fit the budget, so the caller can
	// surface them rather than dropping them silently.
	Unschedulable []string
}

// Schedule computes the actions to converge resident toward desired within budget bytes of
// device memory. It first keeps every forced model (a pinned desired model, which overrides
// the budget, and every pinned or actively decoding resident model), then admits the
// remaining desired models in priority order (preferring those already resident, and the
// more-recently-used among ties, to avoid churn) for as long as they fit the budget. A
// resident model that is neither kept nor pinned nor active is evicted; a desired model that
// does not fit is reported as unschedulable. A model paired with a draft is admitted only if both
// fit, and a kept model's draft is kept with it, so a speculative-decoding pair is resident
// together or not at all. The result is deterministic and idempotent: the chosen set is a fixed
// point, so applying the plan and scheduling again yields no further launches or evictions.
func Schedule(desired []Desired, resident []Resident, budget int64) Plan {
	s := newScheduler(desired, resident)
	s.keepForced(desired, resident)
	unschedulable := s.admit(s.candidates(desired), budget)
	launch, evict := s.actions(resident)

	sort.Strings(launch)
	sort.Strings(evict)
	sort.Strings(unschedulable)
	return Plan{Launch: launch, Evict: evict, Unschedulable: unschedulable}
}

// scheduler holds the indexed inputs and the running keep/budget decision for one Schedule
// call. Its methods are the phases of that decision; bundling them on a value (rather than
// threading the maps and the mutable used/kept accumulators through free functions) is what
// keeps each phase small and Schedule itself a readable sequence.
type scheduler struct {
	residentByID  map[string]Resident
	desiredByID   map[string]Desired
	draftFootByID map[string]int64

	// kept is the set of model ids the plan keeps resident; used is the budget they consume.
	// Both accumulate across keepForced and admit.
	kept map[string]bool
	used int64
}

func newScheduler(desired []Desired, resident []Resident) *scheduler {
	s := &scheduler{
		residentByID:  make(map[string]Resident, len(resident)),
		desiredByID:   make(map[string]Desired, len(desired)),
		draftFootByID: make(map[string]int64),
		kept:          make(map[string]bool, len(resident)+len(desired)),
	}
	for _, r := range resident {
		s.residentByID[r.ModelID] = r
	}
	for _, d := range desired {
		s.desiredByID[d.ModelID] = d
		if d.Draft != nil {
			s.draftFootByID[d.Draft.ModelID] = footprint(d.Draft.Footprint)
		}
	}
	return s
}

// footOf reports a model's budgeted footprint. A model costs the same to keep whether it is
// already resident or about to be launched, so it is budgeted by a single footprint: a desired
// model at its declared estimate, a paired draft at its declared estimate, and a resident-only
// model at its observed size. Budgeting a desired or draft model at its own declared estimate
// (not the runtime's measurement) is what makes the plan a stable fixed point: a model cannot be
// evicted as too big and then re-launched as small enough.
func (s *scheduler) footOf(id string) int64 {
	if d, ok := s.desiredByID[id]; ok {
		return footprint(d.Footprint)
	}
	if f, ok := s.draftFootByID[id]; ok {
		return f
	}
	return footprint(s.residentByID[id].Footprint)
}

// force keeps a single model and charges its footprint to the budget, once.
func (s *scheduler) force(id string) {
	if !s.kept[id] {
		s.kept[id] = true
		s.used += s.footOf(id)
	}
}

// keepPair keeps a desired model and, if it has one, its paired draft, so the two are always
// resident together and budgeted together.
func (s *scheduler) keepPair(d Desired) {
	s.force(d.ModelID)
	if d.Draft != nil {
		s.force(d.Draft.ModelID)
	}
}

// keepForced keeps every model that must stay resident and consumes budget first: a desired
// model that is pinned (it must always be resident, so it overrides the budget), a resident model
// that is pinned, or a resident model that is actively decoding, since evicting it would drop
// in-flight work.
func (s *scheduler) keepForced(desired []Desired, resident []Resident) {
	for _, d := range desired {
		if d.Pinned {
			s.keepPair(d)
		}
	}
	for _, r := range resident {
		if r.Pinned || r.Active {
			s.force(r.ModelID)
			// A forced resident that is a desired model with a draft keeps its draft too, so an
			// actively-decoding or pinned primary is never left without the draft it pairs with.
			if d, ok := s.desiredByID[r.ModelID]; ok && d.Draft != nil {
				s.force(d.Draft.ModelID)
			}
		}
	}
}

// candidates returns the not-yet-kept desired models in admission order (priority high to low,
// then already-resident and more-recently-used first to avoid churn, then id for determinism). A
// duplicate desired entry is collapsed to its first occurrence, so a malformed input cannot
// double-count a model or list it as unschedulable twice.
func (s *scheduler) candidates(desired []Desired) []Desired {
	seen := make(map[string]bool, len(desired))
	candidates := make([]Desired, 0, len(desired))
	for _, d := range desired {
		if s.kept[d.ModelID] || seen[d.ModelID] {
			continue
		}
		seen[d.ModelID] = true
		candidates = append(candidates, d)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return s.higherPriority(candidates[i], candidates[j])
	})
	return candidates
}

// higherPriority orders admission candidates: higher priority first, then a model already
// resident over one not (and the more-recently-used among resident ties) to avoid churn, then
// model id so the order is total and deterministic.
func (s *scheduler) higherPriority(a, b Desired) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	ra, aok := s.residentByID[a.ModelID]
	rb, bok := s.residentByID[b.ModelID]
	if aok != bok {
		return aok // prefer keeping a model that is already resident
	}
	if aok && bok && ra.LastUsed != rb.LastUsed {
		return ra.LastUsed > rb.LastUsed // among resident ties, keep the more recently used
	}
	return a.ModelID < b.ModelID
}

// admit keeps candidates in order for as long as they fit the budget, returning those that did
// not fit. A model and its draft are admitted together: the cost is the primary plus the draft,
// counting the draft only if it is not already kept, so a primary that cannot fit both is
// reported unschedulable rather than launched without the draft it needs.
func (s *scheduler) admit(candidates []Desired, budget int64) []string {
	var unschedulable []string
	for _, d := range candidates {
		cost := s.footOf(d.ModelID)
		if d.Draft != nil && !s.kept[d.Draft.ModelID] {
			cost += s.footOf(d.Draft.ModelID)
		}
		if s.used+cost <= budget {
			s.keepPair(d)
		} else {
			unschedulable = append(unschedulable, d.ModelID)
		}
	}
	return unschedulable
}

// actions turns the kept set into concrete moves: launch every kept model that is not yet
// resident; evict every resident model that is not kept (which, since pinned and active models
// are always kept, is evictable).
func (s *scheduler) actions(resident []Resident) (launch, evict []string) {
	for id := range s.kept {
		if _, isResident := s.residentByID[id]; !isResident {
			launch = append(launch, id)
		}
	}
	for _, r := range resident {
		if !s.kept[r.ModelID] {
			evict = append(evict, r.ModelID)
		}
	}
	return launch, evict
}

// footprint reads a byte count as non-negative, so a malformed negative input cannot make
// the budget accounting drift.
func footprint(b int64) int64 {
	if b < 0 {
		return 0
	}
	return b
}

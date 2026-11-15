// Package orchestrate decides which local models stay resident in limited device
// memory. Given the desired and the served models, it computes the launches and
// evictions that converge the two within a memory budget, and never evicts a pinned
// or actively decoding model. The decision is a pure function, so the policy is
// testable without a live runtime.
package orchestrate

import "sort"

// Desired is a model the controller should keep resident, with the inputs the policy
// needs under memory pressure.
type Desired struct {
	// ModelID is the catalog id, the identity shared with the serve manager.
	ModelID string
	// Footprint is the estimated resident device memory in bytes, from the model's known
	// size, so a model can be budgeted before it is launched. Negative reads as zero.
	Footprint int64
	// Priority orders models under memory pressure; the lower one is evicted first.
	Priority int
	// Pinned keeps a model resident regardless of priority or budget, such as a small
	// default model.
	Pinned bool
	// Draft is a small companion for speculative decoding that must be resident whenever
	// its primary is. The primary is admitted only if both fit, and evicting the primary
	// evicts the draft. Nil means the model serves alone.
	Draft *Draft
}

// Draft is the companion model paired with a primary for speculative decoding. It is
// budgeted on its own but never scheduled independently.
type Draft struct {
	// ModelID is the draft model's catalog id.
	ModelID string
	// Footprint is its resident device memory in bytes. Negative reads as zero.
	Footprint int64
}

// Resident is a model the serve manager currently runs.
type Resident struct {
	// ModelID is the catalog id.
	ModelID string
	// Footprint is its device memory in bytes. Negative reads as zero.
	Footprint int64
	// Pinned marks a model that must not be evicted.
	Pinned bool
	// Active is true while the model is decoding a request. An active model is kept even
	// when it is no longer desired, so in-flight work is never dropped.
	Active bool
	// LastUsed orders eviction among equal candidates, least recently used first. It is a
	// logical or wall-clock stamp from the caller.
	LastUsed int64
}

// Plan is the set of actions that converge the resident set toward the desired set. A
// resident set already equal to the chosen set yields an empty plan.
type Plan struct {
	// Launch lists desired model ids not yet resident.
	Launch []string
	// Evict lists resident model ids that are neither pinned nor active.
	Evict []string
	// Unschedulable lists desired models that did not fit the budget, so the caller can
	// report them.
	Unschedulable []string
}

// Schedule computes the actions that converge resident toward desired within budget
// bytes of device memory. It first keeps every forced model (pinned desired models,
// which override the budget, and pinned or active residents), then admits the rest in
// priority order while they fit, preferring resident and recently used models to avoid
// churn. A draft pair is resident together or not at all. The chosen set is a fixed
// point: applying the plan and scheduling again yields no further moves.
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

// scheduler holds the indexed inputs and the running keep and budget decision for one
// Schedule call, so each phase is a small method instead of a function threading the
// accumulators.
type scheduler struct {
	residentByID  map[string]Resident
	desiredByID   map[string]Desired
	draftFootByID map[string]int64

	// kept is the set of model ids the plan keeps resident; used is the budget they
	// consume. Both accumulate across keepForced and admit.
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

// footOf reports a model's budgeted footprint: a desired or draft model at its declared
// estimate, and a resident-only model at its observed size. Using the declared estimate
// instead of the runtime's measurement makes the plan a fixed point, so a model cannot
// be evicted as too big and then relaunched as small enough.
func (s *scheduler) footOf(id string) int64 {
	if d, ok := s.desiredByID[id]; ok {
		return footprint(d.Footprint)
	}
	if f, ok := s.draftFootByID[id]; ok {
		return f
	}
	return footprint(s.residentByID[id].Footprint)
}

// force keeps a single model and charges its footprint to the budget once.
func (s *scheduler) force(id string) {
	if !s.kept[id] {
		s.kept[id] = true
		s.used += s.footOf(id)
	}
}

// keepPair keeps a desired model and its draft, if any, so the two are budgeted
// together.
func (s *scheduler) keepPair(d Desired) {
	s.force(d.ModelID)
	if d.Draft != nil {
		s.force(d.Draft.ModelID)
	}
}

// keepForced keeps and charges first every pinned desired model, every pinned resident,
// and every resident that is actively decoding.
func (s *scheduler) keepForced(desired []Desired, resident []Resident) {
	for _, d := range desired {
		if d.Pinned {
			s.keepPair(d)
		}
	}
	for _, r := range resident {
		if r.Pinned || r.Active {
			s.force(r.ModelID)
			// A forced primary keeps its draft too, so it is never left without it.
			if d, ok := s.desiredByID[r.ModelID]; ok && d.Draft != nil {
				s.force(d.Draft.ModelID)
			}
		}
	}
}

// candidates returns the not-yet-kept desired models in admission order. A duplicate
// entry collapses to its first occurrence, so a malformed input cannot double-count a
// model or report it unschedulable twice.
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

// higherPriority orders admission candidates: higher priority first, then an already
// resident model (the more recently used among ties) to avoid churn, then model id so
// the order is total.
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

// admit keeps candidates in order while they fit the budget and returns those that did
// not. A primary costs itself plus its draft, if the draft is not already kept, so a
// primary that cannot fit both is reported unschedulable instead of launched alone.
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

// actions launches every kept model not yet resident and evicts every resident model
// not kept, which is never a pinned or active one since those are always kept.
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

// footprint reads a byte count as non-negative, so a malformed negative input cannot
// make the budget accounting drift.
func footprint(b int64) int64 {
	if b < 0 {
		return 0
	}
	return b
}

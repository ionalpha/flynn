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
	residentByID := make(map[string]Resident, len(resident))
	for _, r := range resident {
		residentByID[r.ModelID] = r
	}
	desiredByID := make(map[string]Desired, len(desired))
	draftFootByID := make(map[string]int64)
	for _, d := range desired {
		desiredByID[d.ModelID] = d
		if d.Draft != nil {
			draftFootByID[d.Draft.ModelID] = footprint(d.Draft.Footprint)
		}
	}

	// A model costs the same to keep whether it is already resident or about to be launched,
	// so it is budgeted by a single footprint: a desired model at its declared estimate, a
	// paired draft at its declared estimate, and a resident-only model at its observed size.
	// Budgeting a desired or draft model at its own declared estimate (not the runtime's
	// measurement) is what makes the plan a stable fixed point: a model cannot be evicted as
	// too big and then re-launched as small enough.
	footOf := func(id string) int64 {
		if d, ok := desiredByID[id]; ok {
			return footprint(d.Footprint)
		}
		if f, ok := draftFootByID[id]; ok {
			return f
		}
		return footprint(residentByID[id].Footprint)
	}

	// Forced models are kept no matter what and consume budget first: a desired model that
	// is pinned (it must always be resident, so it overrides the budget), a resident model
	// that is pinned, or a resident model that is actively decoding, since evicting it would
	// drop in-flight work.
	kept := make(map[string]bool, len(resident)+len(desired))
	var used int64
	force := func(id string) {
		if !kept[id] {
			kept[id] = true
			used += footOf(id)
		}
	}
	// keepPair keeps a desired model and, if it has one, its paired draft, so the two are always
	// resident together and budgeted together.
	keepPair := func(d Desired) {
		force(d.ModelID)
		if d.Draft != nil {
			force(d.Draft.ModelID)
		}
	}
	for _, d := range desired {
		if d.Pinned {
			keepPair(d)
		}
	}
	for _, r := range resident {
		if r.Pinned || r.Active {
			force(r.ModelID)
			// A forced resident that is a desired model with a draft keeps its draft too, so an
			// actively-decoding or pinned primary is never left without the draft it pairs with.
			if d, ok := desiredByID[r.ModelID]; ok && d.Draft != nil {
				force(d.Draft.ModelID)
			}
		}
	}

	// Admit the remaining desired models in priority order, budget permitting. A duplicate
	// desired entry is collapsed to its first occurrence, so a malformed input cannot
	// double-count a model or list it as unschedulable twice.
	seen := make(map[string]bool, len(desired))
	candidates := make([]Desired, 0, len(desired))
	for _, d := range desired {
		if kept[d.ModelID] || seen[d.ModelID] {
			continue
		}
		seen[d.ModelID] = true
		candidates = append(candidates, d)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		ra, aok := residentByID[a.ModelID]
		rb, bok := residentByID[b.ModelID]
		if aok != bok {
			return aok // prefer keeping a model that is already resident
		}
		if aok && bok && ra.LastUsed != rb.LastUsed {
			return ra.LastUsed > rb.LastUsed // among resident ties, keep the more recently used
		}
		return a.ModelID < b.ModelID
	})

	var unschedulable []string
	for _, d := range candidates {
		// A model and its draft are admitted together: the cost is the primary plus the draft,
		// counting the draft only if it is not already kept, so a primary that cannot fit both is
		// reported unschedulable rather than launched without the draft it needs.
		cost := footOf(d.ModelID)
		if d.Draft != nil && !kept[d.Draft.ModelID] {
			cost += footOf(d.Draft.ModelID)
		}
		if used+cost <= budget {
			keepPair(d)
		} else {
			unschedulable = append(unschedulable, d.ModelID)
		}
	}

	// Launch every kept model that is not yet resident; evict every resident model that is
	// not kept (which, since pinned and active models are always kept, is evictable).
	var launch, evict []string
	for id := range kept {
		if _, isResident := residentByID[id]; !isResident {
			launch = append(launch, id)
		}
	}
	for _, r := range resident {
		if !kept[r.ModelID] {
			evict = append(evict, r.ModelID)
		}
	}

	sort.Strings(launch)
	sort.Strings(evict)
	sort.Strings(unschedulable)
	return Plan{Launch: launch, Evict: evict, Unschedulable: unschedulable}
}

// footprint reads a byte count as non-negative, so a malformed negative input cannot make
// the budget accounting drift.
func footprint(b int64) int64 {
	if b < 0 {
		return 0
	}
	return b
}

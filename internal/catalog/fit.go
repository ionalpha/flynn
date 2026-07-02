package catalog

// Fit is how a model sits against a memory budget: can the box run it, run it with
// little headroom, or not at all. It is the answer to "which of these can I actually
// run" once the machine's memory is known.
type Fit string

const (
	// FitFeasible means the weights and a working runtime headroom fit comfortably.
	FitFeasible Fit = "feasible"
	// FitTight means the weights fit but the headroom for the cache and activations is
	// thin, so a long context or a second process may not.
	FitTight Fit = "tight"
	// FitOverBudget means the weights alone do not fit; the model is out of spec.
	FitOverBudget Fit = "over-budget"
	// FitUnknown means there was not enough information to judge (no size, or no
	// detected/!given memory budget).
	FitUnknown Fit = "unknown"
)

// headroomNumer/headroomDenom express the runtime headroom reserved over the raw
// weights for the key-value cache and activations, as a fraction (here one fifth, so
// 20%). It is a deliberately simple, conservative estimate; a context-aware estimate
// that scales the cache term with the target window is a later refinement.
const (
	headroomNumer = 1
	headroomDenom = 5
)

// Feasibility classifies whether a quantization of sizeBytes runs within availBytes of
// memory, reserving a headroom over the weights so a model judged feasible has room to
// actually run, not just to load. A non-positive size or budget is unknown rather than
// a false verdict.
func Feasibility(availBytes, sizeBytes int64) Fit {
	if availBytes <= 0 || sizeBytes <= 0 {
		return FitUnknown
	}
	needed := sizeBytes + sizeBytes*headroomNumer/headroomDenom
	switch {
	case availBytes >= needed:
		return FitFeasible
	case availBytes >= sizeBytes:
		return FitTight
	default:
		return FitOverBudget
	}
}

// FitFor reports how a model fits a memory budget, judged by its smallest
// quantization (the cheapest way to run it). An API model downloads nothing and is
// always feasible; a local model with no size is unknown.
func (m ModelSpec) FitFor(availBytes int64) Fit {
	if !m.Local() {
		return FitFeasible
	}
	q, ok := m.SmallestQuant()
	if !ok {
		return FitUnknown
	}
	return Feasibility(availBytes, q.SizeBytes)
}

// Runnable reports whether a fit verdict means the model can run at all (feasible or
// tight), so a "what can I run" view can keep the runnable ones and a recommendation
// can ignore the rest.
func (f Fit) Runnable() bool { return f == FitFeasible || f == FitTight }

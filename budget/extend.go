package budget

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// The errors Extend returns. Each carries a stable code a client branches on, and
// every one means nothing was raised except where its doc comment says otherwise.
var (
	// ErrNoExtensionCap refuses an extension on a ledger that was given no cap
	// (see [Ledger.WithExtensionCap]). A ceiling a person can raise without limit
	// is not a ceiling, so an unset cap is a refusal and not "unlimited".
	ErrNoExtensionCap = fault.New(fault.Terminal, "extension_cap_unset", "no extension cap is configured, so the ceiling cannot be raised")
	// ErrExtensionCap refuses an extension that would take the total raised past
	// the cap on some axis.
	ErrExtensionCap = fault.New(fault.Terminal, "extension_cap_exceeded", "the extension would exceed the cap on total extensions")
	// ErrNotHuman refuses an extension that does not come from a person's request:
	// no principal named, or a context that belongs to a governed action.
	ErrNotHuman = fault.New(fault.Forbidden, "not_human", "only a person can extend a budget")
	// ErrNotRaised refuses a request that names no ceiling to raise.
	ErrNotRaised = fault.New(fault.Terminal, "not_raised", "the extension names no ceiling to raise")
	// ErrLowered refuses a request for a ceiling below the current one.
	ErrLowered = fault.New(fault.Terminal, "ceiling_lowered", "an extension can only raise a ceiling")
	// ErrNoCeiling refuses a request on an axis that has no ceiling: that axis is
	// already unlimited, and a finite ceiling would lower it.
	ErrNoCeiling = fault.New(fault.Terminal, "no_ceiling", "the axis has no ceiling to raise")
	// ErrNotBudgetStop refuses an extension for a run that is not stopped on its
	// budget: a run still working, or one that stopped for another reason.
	ErrNotBudgetStop = fault.New(fault.Terminal, "not_budget_stop", "the run is not stopped on its budget")
	// ErrKilled refuses an extension for a run an operator killed. A kill is final.
	ErrKilled = fault.New(fault.Terminal, "killed", "the run was killed and cannot be reopened")
	// ErrExtendUnwired refuses an extension that names no recorder or no goal to
	// reopen. Either missing would leave the ceilings disagreeing or the raise off
	// the record.
	ErrExtendUnwired = fault.New(fault.Terminal, "extend_unwired", "an extension needs a recorder and a goal to reopen")
)

// Recorder writes an extension onto the run's record (the spine) as an event
// attributed to a person.
type Recorder interface {
	RecordExtended(ctx context.Context, e Extended) error
}

// Reopener is the goal side of an extension: the second ceiling, and the stop the
// first one caused. [Ledger.Extend] drives it so the two cannot be moved by
// separate calls.
type Reopener interface {
	// Admit reports whether the run may be extended, before anything changes. It
	// returns [ErrKilled] for a killed run and [ErrNotBudgetStop] for one that is
	// running or stopped for another reason.
	Admit(ctx context.Context) error
	// Reopen raises the run's own ceiling to ceiling and releases the budget stop.
	// It checks admission again against the record it writes, so a run killed
	// between Admit and Reopen stays killed.
	Reopen(ctx context.Context, ceiling Limits) error
}

// Extension is a request to raise a run's ceiling.
type Extension struct {
	// To is the ceiling asked for, per axis. An axis left at zero is not asked
	// about; a set axis must not be below the ledger's current ceiling. It is
	// absolute rather than a delta so that repeating a request that stopped between
	// its writes finishes it and raises nothing twice.
	To Limits
	// Principal names the person asking, as the entry point authenticated them.
	Principal string
	// Reason is the person's own words and nothing branches on it.
	Reason string
	// Record and Goal are both required.
	Record Recorder
	Goal   Reopener
}

// Extended is what an extension changed, as it is recorded.
type Extended struct {
	Run       string
	Principal string
	Reason    string
	// From and To are the ledger's ceiling before and after.
	From, To Limits
	// Added is what this call raised, Total what every extension on this run has
	// raised, and Cap the most that may be raised in all.
	Added, Total, Cap Limits
}

// WithExtensionCap returns a Ledger over the same store that may raise a run's
// ceiling by cap in total, per axis. A zero axis allows no extension on that axis,
// so the zero Limits, which is what an unconfigured host passes, allows none.
func (l *Ledger) WithExtensionCap(limit Limits) *Ledger {
	cp := *l
	cp.extendCap = limit
	return &cp
}

// Extend raises the ceiling of run id, and the goal's with it, at a person's request.
// The order is the safety argument. The ledger is raised first, the extension is
// recorded second and the goal is reopened last, so a crash leaves the ceiling
// raised and the run still stopped, never a run working on a ceiling nobody
// recorded. Repeating the same request completes whatever the crash left undone.
//
// The cap is checked inside the write that raises the ceiling, against the total
// that write reads, so concurrent extensions cannot jointly pass it.
//
// Only an entry point that has authenticated a person may call Extend: a context
// bound to a run's budget (the one every governed action carries) is refused, and
// an architecture test fails on any other caller. A raise already made is not
// undone if recording or reopening then fails.
func (l *Ledger) Extend(ctx context.Context, id string, scope resource.Scope, ext Extension) (Extended, error) {
	if l.extendCap.zero() {
		return Extended{}, ErrNoExtensionCap
	}
	if _, inAction := FromContext(ctx); inAction || strings.TrimSpace(ext.Principal) == "" {
		return Extended{}, ErrNotHuman
	}
	if ext.Record == nil || ext.Goal == nil {
		return Extended{}, ErrExtendUnwired
	}
	if err := ext.To.validate(); err != nil {
		return Extended{}, err
	}
	if err := ext.Goal.Admit(ctx); err != nil {
		return Extended{}, err
	}

	out := Extended{Run: id, Principal: ext.Principal, Reason: ext.Reason, Cap: l.extendCap}
	_, err := resource.Update(ctx, l.store, Kind, scope, id, func(r *resource.Resource) error {
		spec, err := DecodeSpec(*r)
		if err != nil {
			return err
		}
		status, err := DecodeStatus(*r)
		if err != nil {
			return err
		}
		var total Limits
		if status.Extended != nil {
			total = *status.Extended
		}
		next, added, err := spec.Limits.raised(ext.To)
		if err != nil {
			return err
		}
		total = total.plus(added)
		if total.over(l.extendCap) {
			return ErrExtensionCap
		}
		out.From, out.To, out.Added, out.Total = spec.Limits, next, added, total
		if added.zero() {
			return resource.ErrSkipUpdate
		}
		spec.Limits = next
		status.Extended = &total
		rawSpec, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		rawStatus, err := status.Encode()
		if err != nil {
			return err
		}
		r.Spec, r.Status = rawSpec, rawStatus
		return nil
	})
	if err != nil {
		return Extended{}, extendErr(err)
	}
	if err := ext.Record.RecordExtended(ctx, out); err != nil {
		return out, err
	}
	if err := ext.Goal.Reopen(ctx, out.To); err != nil {
		return out, err
	}
	return out, nil
}

func extendErr(err error) error {
	if errors.Is(err, resource.ErrConflict) {
		return fault.New(fault.Transient, "budget_update_contention", "budget extension gave up after repeated write conflicts")
	}
	return err
}

// zero reports whether no axis is set.
func (l Limits) zero() bool { return l.Tokens == 0 && l.Cost == 0 }

func (l Limits) plus(o Limits) Limits {
	return Limits{Tokens: l.Tokens + o.Tokens, Cost: l.Cost + o.Cost}
}

// over reports whether l exceeds limit on any axis, an unset axis of limit
// allowing nothing.
func (l Limits) over(limit Limits) bool { return l.Tokens > limit.Tokens || l.Cost > limit.Cost }

// validate refuses a request that is empty, negative or not a number.
func (l Limits) validate() error {
	if l.Tokens < 0 || !(l.Cost >= 0) || math.IsInf(l.Cost, 0) {
		return ErrLowered
	}
	if l.zero() {
		return ErrNotRaised
	}
	return nil
}

// raised returns the ceiling after asking for to, and how much that added. An
// axis is refused when it has no ceiling or when to is below it.
func (l Limits) raised(to Limits) (next, added Limits, err error) {
	next = l
	if to.Tokens > 0 {
		switch {
		case l.Tokens == 0:
			return l, Limits{}, ErrNoCeiling
		case to.Tokens < l.Tokens:
			return l, Limits{}, ErrLowered
		}
		next.Tokens, added.Tokens = to.Tokens, to.Tokens-l.Tokens
	}
	if to.Cost > 0 {
		switch {
		case l.Cost == 0:
			return l, Limits{}, ErrNoCeiling
		case to.Cost < l.Cost:
			return l, Limits{}, ErrLowered
		}
		next.Cost, added.Cost = to.Cost, to.Cost-l.Cost
	}
	return next, added, nil
}

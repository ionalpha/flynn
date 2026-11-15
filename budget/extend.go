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

// The errors [Ledger.Extend] returns, each with a stable code. None of them raised
// anything.
var (
	// ErrNoExtensionCap refuses on a ledger with no [Ledger.WithExtensionCap], because
	// a ceiling raisable without limit is no ceiling.
	ErrNoExtensionCap = fault.New(fault.Terminal, "extension_cap_unset", "no extension cap is configured, so the ceiling cannot be raised")
	// ErrExtensionCap refuses a raise that takes the total past the cap on an axis.
	ErrExtensionCap = fault.New(fault.Terminal, "extension_cap_exceeded", "the extension would exceed the cap on total extensions")
	// ErrNotHuman refuses an empty principal or a context bound to a run's budget.
	ErrNotHuman = fault.New(fault.Forbidden, "not_human", "only a person can extend a budget")
	// ErrNotRaised refuses a request that names no axis.
	ErrNotRaised = fault.New(fault.Terminal, "not_raised", "the extension names no ceiling to raise")
	// ErrLowered refuses a ceiling below the current one, or a negative or non-finite one.
	ErrLowered = fault.New(fault.Terminal, "ceiling_lowered", "an extension can only raise a ceiling")
	// ErrNoCeiling refuses an axis with no ceiling, because a finite one would lower it.
	ErrNoCeiling = fault.New(fault.Terminal, "no_ceiling", "the axis has no ceiling to raise")
	// ErrNotBudgetStop refuses a run still working or stopped for another reason.
	ErrNotBudgetStop = fault.New(fault.Terminal, "not_budget_stop", "the run is not stopped on its budget")
	// ErrKilled refuses a run an operator killed, because a kill is final.
	ErrKilled = fault.New(fault.Terminal, "killed", "the run was killed and cannot be reopened")
	// ErrExtendUnwired refuses a missing recorder or goal, either of which would leave
	// the raise off the record or the two ceilings disagreeing.
	ErrExtendUnwired = fault.New(fault.Terminal, "extend_unwired", "an extension needs a recorder and a goal to reopen")
)

// Recorder writes an extension onto the run's record as an event attributed to a person.
type Recorder interface {
	RecordExtended(ctx context.Context, e Extended) error
}

// Reopener is the goal's ceiling and budget stop. [Ledger.Extend] drives it so the two
// ceilings cannot be moved by separate calls.
type Reopener interface {
	// Admit returns [ErrKilled] or [ErrNotBudgetStop] before anything changes.
	Admit(ctx context.Context) error
	// Reopen raises the goal's ceiling and releases the stop. It re-checks admission on
	// the record it writes, so a run killed after Admit stays killed.
	Reopen(ctx context.Context, ceiling Limits) error
}

// Extension is a request to raise a run's ceiling.
type Extension struct {
	// To is the absolute ceiling per axis, zero meaning unchanged. It is absolute so a
	// repeated request finishes an interrupted one and raises nothing twice.
	To Limits
	// Principal is the person the entry point authenticated.
	Principal string
	// Reason is free text; nothing branches on it.
	Reason string
	// Record and Goal are required.
	Record Recorder
	Goal   Reopener
}

// Extended is what an extension changed, as it is recorded.
type Extended struct {
	Run       string
	Principal string
	Reason    string
	From, To  Limits
	// Added is this call's raise, Total every raise on the run, Cap the most allowed.
	Added, Total, Cap Limits
}

// WithExtensionCap returns a copy that may raise a run's ceiling by limit in total, per
// axis. A zero axis allows none, so an unconfigured host allows no extension.
func (l *Ledger) WithExtensionCap(limit Limits) *Ledger {
	cp := *l
	cp.extendCap = limit
	return &cp
}

// Extend raises run id's ceiling, and the goal's, at a person's request. It raises the
// ledger, then records, then reopens the goal, so a crash leaves the run stopped on a
// recorded ceiling; repeating the request finishes the rest. A raise is not undone if a
// later step fails.
//
// The cap is checked inside the write that raises the ceiling, so concurrent
// extensions cannot jointly pass it. A context bound to a run's budget is refused, and
// an architecture test fails on any caller other than an authenticated entry point.
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

func (l Limits) zero() bool { return l.Tokens == 0 && l.Cost == 0 }

func (l Limits) plus(o Limits) Limits {
	return Limits{Tokens: l.Tokens + o.Tokens, Cost: l.Cost + o.Cost}
}

// over treats an unset axis of limit as allowing nothing.
func (l Limits) over(limit Limits) bool { return l.Tokens > limit.Tokens || l.Cost > limit.Cost }

// validate's !(Cost >= 0) also refuses NaN.
func (l Limits) validate() error {
	if l.Tokens < 0 || !(l.Cost >= 0) || math.IsInf(l.Cost, 0) {
		return ErrLowered
	}
	if l.zero() {
		return ErrNotRaised
	}
	return nil
}

// raised returns the new ceiling and what it added.
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

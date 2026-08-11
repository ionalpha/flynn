package evidence

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// AuditInvariantAction is the dispatch action name a term's check runs under, so an
// audit is admitted, traced and recorded under a stable name alongside the tools the
// agent invokes. It is deliberately distinct from VerifyItemAction: the two run the same
// kind of thing for opposite purposes, and a grant should be able to say yes to one
// without saying yes to the other.
const AuditInvariantAction = "goal.audit.invariant"

// CommandAuditor is the goal.InvariantAuditor that rules on a term by running the check
// the term declares, inside the run's own sandbox and through the dispatch waist. A
// zero exit says the term still holds; any other exit is a breach carrying the check's
// output as what was observed.
//
// Running the check is the whole point. The failure invariants exist for is a run that
// looks locally compliant at every step, and asking that run whether it kept to its
// terms is asking the wrong witness. A check that exits non-zero is not an opinion.
//
// Everything an audit cannot answer is an error rather than a clean verdict. A missing
// sandbox, a check that could not be started, an admission that refused it, a term with no
// check and nobody to rule on it: none of them is evidence a term holds, and reporting no
// breach would hand the run a pass it never earned. The reconciler stops the goal on the
// error, which is the fail-closed direction. This is the opposite of CommandVerifier's
// rule for a ledger item, and deliberately so: an item that cannot be checked is unproven
// and the run carries on trying, while a term that cannot be checked means the run is no
// longer governed and there is nothing to carry on to.
//
// A term that declares no check is not this auditor's to rule on, and it hands those to
// the prose auditor it was built with (ModelAuditor in production). The split is by what
// the term declares rather than by what it says, so a term with a check is always settled
// by running it: a model asked to judge from the record whether the check would have
// passed is guessing at something one command away.
type CommandAuditor struct {
	sb         sandbox.Sandbox
	log        spine.Log
	dispatcher *dispatch.Dispatcher
	prose      goal.InvariantAuditor
}

// NewCommandAuditor builds an auditor running each term's check in sb, recording every
// audit on log, governed by a dispatcher built from opts. Pass the same admitter, event
// sink and observability the rest of the run uses, so an audit shares its governance and
// its spine.
//
// prose is where terms that declare no check are sent, and it is an explicit argument
// rather than an option because every host has to decide what happens to them. A nil prose
// auditor is a legitimate answer, and it means the strict one: a goal may only state terms
// that reduce to a command, and one that states anything else stops rather than running
// against a term nobody rules on.
func NewCommandAuditor(sb sandbox.Sandbox, log spine.Log, prose goal.InvariantAuditor, opts ...dispatch.Option) *CommandAuditor {
	return &CommandAuditor{sb: sb, log: log, prose: prose, dispatcher: dispatch.New(opts...)}
}

var _ goal.InvariantAuditor = (*CommandAuditor)(nil)

// Audit runs every term's check and returns a breach for each that failed.
//
// It audits on the reconcile path, which carries no run's authority of its own, so the
// goal's grant is bound onto the context here the way the unit fan-out binds it for a
// plan-driven spawn. An audit is therefore admitted against the authority of the run
// being audited: a goal that cannot run its own checks cannot be audited by running
// them, and it stops rather than being waved through.
func (a *CommandAuditor) Audit(ctx context.Context, r resource.Resource, spec goal.Spec, status goal.Status, terms []goal.Invariant) ([]goal.Breach, error) {
	declared, prose := splitByCheck(terms)
	if len(declared) > 0 && a.sb == nil {
		return nil, fault.Wrap(fault.Terminal, "audit_no_sandbox", ErrNoSandbox)
	}
	// A goal with no grant is unconstrained, exactly as it is everywhere else, so an
	// empty one is left unbound rather than bound as a grant that allows nothing.
	if len(spec.Grant) > 0 {
		ctx = capability.Into(ctx, capability.NewGrant(spec.Grant...))
	}
	var breaches []goal.Breach
	for _, term := range declared {
		held, detail, err := a.auditOne(ctx, r, term)
		if err != nil {
			return nil, err
		}
		if !held {
			breaches = append(breaches, goal.Breach{ID: term.ID, Detail: detail})
		}
	}
	if len(prose) == 0 {
		return breaches, nil
	}
	if a.prose == nil {
		return nil, fault.New(fault.Terminal, "audit_no_check",
			"audit: invariant "+prose[0].ID+" declares no check to run and no prose auditor is wired, "+
				"so nothing can rule on it")
	}
	// The prose terms go in one call so the auditor reads the record once and judges them
	// all against the same reading. They are handed the status this pass built, the same
	// as the ones audited here, since that is the run as it stands after the step.
	found, err := a.prose.Audit(ctx, r, spec, status, prose)
	if err != nil {
		return nil, err
	}
	return append(breaches, found...), nil
}

// splitByCheck separates the terms that declare a check from the terms that do not.
func splitByCheck(terms []goal.Invariant) (declared, prose []goal.Invariant) {
	for _, term := range terms {
		if strings.TrimSpace(term.Check) == "" {
			prose = append(prose, term)
			continue
		}
		declared = append(declared, term)
	}
	return declared, prose
}

// auditOne runs one term's check and records the audit on the run's stream. The record
// is written for a term that held as well as one that did not: the point of auditing on
// the spine rather than only on the status is to be able to show afterwards that the
// terms were checked, and a log that only ever mentions the breaches cannot show that.
func (a *CommandAuditor) auditOne(ctx context.Context, r resource.Resource, term goal.Invariant) (held bool, detail string, err error) {
	check := strings.TrimSpace(term.Check)
	var res sandbox.ExecResult
	gerr := a.dispatcher.Govern(ctx, dispatch.Action{
		Name:  AuditInvariantAction,
		Scope: state.Scope(r.Scope),
		Trust: sandbox.TrustSemi,
		Goal:  r.Name,
	}, func(ctx context.Context) (dispatch.Metering, error) {
		out, execErr := a.sb.Exec(ctx, sandbox.Command{Line: check})
		res = out
		return dispatch.Metering{}, execErr
	})
	if gerr != nil {
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		// Refused or unable to start: the term is unaudited, which is not the same as
		// broken and is certainly not the same as held. A momentary failure keeps its
		// class and retries; anything else is reported Terminal, so the goal settles
		// saying the check could not run.
		//
		// Terminal rather than the refusal's own class, because a refusal is where this
		// went wrong before. A gate that will never admit this check (a host whose
		// sandbox cannot contain semi-trusted work is the one that happens) refuses
		// Forbidden, and the goal reconciler settles a goal on a Terminal fault only:
		// every other class is handed back, the controller declines to spin on it, and
		// the run is left running with a step in flight, no verdict on its terms, and
		// nobody told. A guard that cannot be applied has to stop the run, and stopping
		// it is worth nothing if the run never hears about it.
		if class := fault.Classify(gerr); class == fault.Transient {
			return false, "", fault.Wrap(class, "audit_check_unrun",
				fmt.Errorf("audit: invariant %s: the check could not run: %w", term.ID, gerr))
		}
		return false, "", fault.Wrap(fault.Terminal, "audit_check_unrun",
			fmt.Errorf("audit: invariant %s: the check could not run: %w", term.ID, gerr))
	}

	held = res.ExitCode == 0
	if !held {
		detail = fmt.Sprintf("`%s` exited %d\n%s", check, res.ExitCode, clip(res.Output, maxDetail))
	}
	if err := a.record(ctx, r, term, held, detail, res); err != nil {
		return false, "", err
	}
	return held, detail, nil
}

// record appends the audit to the goal's stream. A failure to write it fails the audit:
// an audit nobody can show happened is the thing this exists to prevent, so it is not
// quietly dropped in favour of the verdict it produced.
func (a *CommandAuditor) record(ctx context.Context, r resource.Resource, term goal.Invariant, held bool, detail string, res sandbox.ExecResult) error {
	if a.log == nil {
		return fault.New(fault.Terminal, "audit_no_log",
			"audit: no spine log wired to record the audit on")
	}
	payload := map[string]any{
		chain.InvariantKey:      term.ID,
		chain.InvariantHeldKey:  held,
		chain.ItemProvenanceKey: chain.ProvenanceExecuted,
		chain.ItemExitKey:       res.ExitCode,
		chain.ItemOutputKey:     outputHash(res.Output),
	}
	if detail != "" {
		payload[chain.InvariantDetailKey] = detail
	}
	// ActorSystem: the runtime ran the check and observed the exit code. Nothing a model
	// wrote reaches this event's provenance, which is what the executed marker certifies.
	if _, err := a.log.Append(ctx, spine.AppendInput{
		Stream:  r.Name,
		Type:    chain.InvariantAudited,
		Actor:   spine.ActorSystem,
		Payload: payload,
	}); err != nil {
		return fault.Wrap(fault.Transient, "audit_append", err)
	}
	return nil
}

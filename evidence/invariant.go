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

// AuditInvariantAction is the dispatch action a term's check runs under. It differs from
// [VerifyItemAction] so a grant can allow one without the other.
const AuditInvariantAction = "goal.audit.invariant"

// CommandAuditor is the [goal.InvariantAuditor] that runs each term's check in the run's
// sandbox through the dispatch waist. Exit 0 holds; any other exit is a breach. The run
// is not asked, because a run that looks compliant at each step is the wrong witness.
//
// An audit that cannot answer is an error and the goal stops. [CommandVerifier] does the
// opposite, because an unchecked item is unproven while an unchecked term leaves the run
// ungoverned. A term with no check goes to the prose auditor ([ModelAuditor]).
type CommandAuditor struct {
	sb         sandbox.Sandbox
	log        spine.Log
	dispatcher *dispatch.Dispatcher
	prose      goal.InvariantAuditor
}

// NewCommandAuditor runs checks in sb and records audits on log, under a dispatcher built
// from opts. prose is an argument so every host decides where unchecked terms go. A nil
// prose is the strict answer: a goal stating a term with no check stops.
func NewCommandAuditor(sb sandbox.Sandbox, log spine.Log, prose goal.InvariantAuditor, opts ...dispatch.Option) *CommandAuditor {
	return &CommandAuditor{sb: sb, log: log, prose: prose, dispatcher: dispatch.New(opts...)}
}

var _ goal.InvariantAuditor = (*CommandAuditor)(nil)

// Audit returns a breach for each term that failed. The reconcile path carries no run's
// authority, so the goal's grant is bound here, and a goal that cannot run its own
// checks stops rather than being waved through.
func (a *CommandAuditor) Audit(ctx context.Context, r resource.Resource, spec goal.Spec, status goal.Status, terms []goal.Invariant) ([]goal.Breach, error) {
	declared, prose := splitByCheck(terms)
	if len(declared) > 0 && a.sb == nil {
		return nil, fault.Wrap(fault.Terminal, "audit_no_sandbox", ErrNoSandbox)
	}
	// An empty grant means unconstrained, so binding it would wrongly allow nothing.
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
	// One call, so every prose term is judged against one reading of the record.
	found, err := a.prose.Audit(ctx, r, spec, status, prose)
	if err != nil {
		return nil, err
	}
	return append(breaches, found...), nil
}

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

// auditOne records held terms as well as breaches, so the spine shows afterwards that
// every term was checked.
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
		// An unrun term is neither held nor broken. A Transient failure retries; any other
		// becomes Terminal, because the goal reconciler settles only on Terminal. A
		// Forbidden refusal (a sandbox that cannot contain semi-trusted work) would leave
		// the run going with no verdict on its terms and nobody told.
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

// record fails the audit when the write fails, because an audit nobody can show
// happened is what this guards against.
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
	// The runtime observed the exit code, so no model text reaches the provenance.
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

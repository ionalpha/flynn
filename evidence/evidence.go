// Package evidence is the shipped producer behind the goal package's evidence gate: it
// runs a ledger item's declared check and writes the verdict onto the run's spine, where
// the gate reads it.
//
// It exists as its own package for the reason progress does. The goal package owns the
// rule (what counts as proof) and must stay free of a log implementation, a sandbox, and
// the dispatch waist; this owns the mechanism, depends on all three, and nothing depends
// on it but the composition that wires it.
//
// The two halves are deliberately separate types. CommandVerifier decides what happened;
// SpineEvidence records it. Neither marks anything proven (the reconciler does, by
// folding the record back through the gate), so there is no path from "a check ran" to
// "an item is done" that skips the rule.
package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// VerifyItemAction is the dispatch action name an item's check runs under, so a
// verification is admitted, traced, and recorded on the spine under a stable, greppable
// name alongside the tools the agent invokes. Running a model-authored command is an
// action with consequences whoever asked for it, so it goes through the same chokepoint
// as the rest of the run rather than being a side channel that bypasses governance.
const VerifyItemAction = "goal.verify.item"

// maxDetail bounds the check output spliced into a verdict's Detail. The full output is
// hashed onto the spine, not carried; this is the part handed back to the model as
// feedback, and an unbounded tail of it would crowd out the turn it is meant to inform.
const maxDetail = 2000

// SpineEvidence is the goal.Evidence implementation over a run's own event stream: a
// verification is appended as a chain.ItemVerified event on the goal's stream, and read
// back from the same stream by sequence.
//
// The event's sequence is its identity, which is what makes consumption work: the gate
// spends a verification by ref, and no two appends can share one. The stream is the goal's
// name, the same identity the session, the progress probe and the conversation cache key
// on, so a run's verifications interleave with its other events in one ordered log rather
// than living in a record of their own.
type SpineEvidence struct {
	log spine.Log
}

// NewSpineEvidence builds an evidence record over log.
func NewSpineEvidence(log spine.Log) *SpineEvidence { return &SpineEvidence{log: log} }

var _ goal.Evidence = (*SpineEvidence)(nil)

// Record appends one item verification to the goal's stream and returns it as the gate
// will read it.
//
// The provenance written is the verdict's own Executed field and nothing else. This is the
// single place the executed marker can enter the record, it is set from what the verifier
// observed about its own execution, and no payload from a model reaches it, which is the
// invariant the whole provenance axis rests on. A model that could claim execution would
// make the field certify nothing.
//
// The event is recorded as ActorSystem: the runtime produced it, not the agent. An
// executed verdict also carries the exit code and a hash of the output, so a later reader
// can tell a check that passed from one merely said to have passed, without the record
// growing by the size of every check's output.
func (s *SpineEvidence) Record(ctx context.Context, r resource.Resource, item string, v goal.ItemVerdict) (goal.Verification, error) {
	payload := map[string]any{
		chain.ItemKey:           item,
		chain.ItemPassedKey:     v.Passed,
		chain.ItemProvenanceKey: chain.ProvenanceAsserted,
	}
	if v.Executed {
		payload[chain.ItemProvenanceKey] = chain.ProvenanceExecuted
		payload[chain.ItemExitKey] = v.ExitCode
		payload[chain.ItemOutputKey] = outputHash(v.Output)
	}
	e, err := s.log.Append(ctx, spine.AppendInput{
		Stream:  r.Name,
		Type:    chain.ItemVerified,
		Actor:   spine.ActorSystem,
		Payload: payload,
	})
	if err != nil {
		return goal.Verification{}, fault.Wrap(fault.Transient, "evidence_append", err)
	}
	// Read the appended event back through the same decoder the gate uses, rather than
	// building the Verification here. The two would otherwise be independent derivations
	// of one wire contract, free to drift on the ref format or the provenance rule, and a
	// drift there is a verification the producer thinks it wrote and the gate never sees.
	vs := goal.VerificationsFrom([]spine.Event{e})
	if len(vs) != 1 {
		return goal.Verification{}, fault.New(fault.Terminal, "evidence_unreadable",
			"evidence: the recorded verification did not read back as one verification")
	}
	return vs[0], nil
}

// Recorded returns every item verification on the goal's stream, in the order recorded. A
// read failure is transient: a record that cannot be reached for a moment is not a run
// with no evidence, and treating it as one would stall a healthy goal.
func (s *SpineEvidence) Recorded(ctx context.Context, r resource.Resource) ([]goal.Verification, error) {
	events, err := s.log.Read(ctx, spine.Query{Stream: r.Name})
	if err != nil {
		return nil, fault.Wrap(fault.Transient, "evidence_read", err)
	}
	return goal.VerificationsFrom(events), nil
}

// outputHash is the content address of a check's output, recorded instead of the output
// itself so the evidence names exactly what was observed without the record growing by the
// size of every test run's log.
func outputHash(out string) string {
	sum := sha256.Sum256([]byte(out))
	return hex.EncodeToString(sum[:])
}

// CommandVerifier runs a ledger item's verify clause as a shell command inside the run's
// sandbox and treats a zero exit code as proof, routing the run through the dispatch waist
// so it is admitted, traced and recorded like any tool the agent invokes.
//
// It is learn.SandboxVerifier's shape aimed at a ledger item rather than a skill, and the
// resemblance is the point: that path was built once, is in production, and already
// separates "the check failed" from "the check could not run". Rebuilding it differently
// here would give the ledger a weaker version of a distinction the codebase already makes.
type CommandVerifier struct {
	sb         sandbox.Sandbox
	dispatcher *dispatch.Dispatcher
}

// NewCommandVerifier builds a verifier running each item's check in sb, governed by a
// dispatcher built from opts. Pass the same admitter, event sink and observability the
// rest of the run uses, so a verification shares its governance and its spine; with no
// options the dispatcher applies standalone defaults and the check is recorded but
// ungoverned.
func NewCommandVerifier(sb sandbox.Sandbox, opts ...dispatch.Option) *CommandVerifier {
	return &CommandVerifier{sb: sb, dispatcher: dispatch.New(opts...)}
}

var _ goal.ItemVerifier = (*CommandVerifier)(nil)

// VerifyItem runs the item's declared check and reports the verdict.
//
// Every way of not running the check lands on the same honest outcome: a verdict that did
// not pass, marked unexecuted, saying why. An item with no clause, a sandbox that could
// not start it, an admission that refused it: none of these is evidence the item is done,
// and none is a failure that should stall the goal, because a clause no mechanism can run
// is a real and common outcome and the gate is the right place to rule on it. Only a
// cancelled context is a hard error.
//
// The command is run as the model wrote it, which is why it runs inside the sandbox and
// under the waist. It is classified semi-trusted, the same level the shell tool declares:
// the text is model-authored, but it executes inside the agent's own confined sandbox
// rather than as arbitrary foreign code. Classifying it any stronger would be a refusal
// dressed as caution. The containment gate would decline every check on every host that
// runs the shell tool happily, and every item would read as unproven for a reason that has
// nothing to do with the items.
func (v *CommandVerifier) VerifyItem(ctx context.Context, r resource.Resource, item goal.LedgerItem) (goal.ItemVerdict, error) {
	check := strings.TrimSpace(item.Verify)
	if check == "" {
		return goal.ItemVerdict{Detail: "the item declared no check to run"}, nil
	}
	if v.sb == nil {
		return goal.ItemVerdict{Detail: ErrNoSandbox.Error()}, nil
	}

	var verdict goal.ItemVerdict
	err := v.dispatcher.Govern(ctx, dispatch.Action{
		Name:  VerifyItemAction,
		Scope: state.Scope(r.Scope),
		Trust: sandbox.TrustSemi,
		Goal:  r.Name,
	}, func(ctx context.Context) (dispatch.Metering, error) {
		res, execErr := v.sb.Exec(ctx, sandbox.Command{Line: check})
		if execErr != nil {
			return dispatch.Metering{}, execErr
		}
		verdict = goal.ItemVerdict{
			Passed:   res.ExitCode == 0,
			Executed: true,
			ExitCode: res.ExitCode,
			Output:   res.Output,
			Detail:   fmt.Sprintf("`%s` exited %d\n%s", check, res.ExitCode, clip(res.Output, maxDetail)),
		}
		return dispatch.Metering{}, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return goal.ItemVerdict{}, ctx.Err()
		}
		return goal.ItemVerdict{Detail: "the check could not run: " + err.Error()}, nil
	}
	return verdict, nil
}

// clip bounds a check's output for the detail handed back to the agent, keeping the tail
// rather than the head: a failing command's diagnosis is almost always its last lines.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// ErrNoSandbox reports a verifier asked to run a check with no sandbox to run it in. It is
// a composition error, not a runtime one: a run reaching this had its verifier wired
// wrong, and every item would otherwise read as unexecuted for a reason that has nothing
// to do with the items.
var ErrNoSandbox = errors.New("evidence: command verifier has no sandbox")

// Package evidence runs a ledger item's declared check and writes the verdict onto the
// run's spine, where the goal package's evidence gate reads it. The goal package owns
// the rule and stays free of a log, a sandbox and the dispatch waist; this package owns
// the mechanism and depends on all three.
//
// [CommandVerifier] decides what happened and [SpineEvidence] records it. Neither marks
// an item proven; the reconciler does, through the gate, so no path from "a check ran"
// to "an item is done" skips the rule.
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

// VerifyItemAction is the dispatch action an item's check runs under. A model-authored
// command has consequences, so it is admitted and traced like any tool call.
const VerifyItemAction = "goal.verify.item"

// maxDetail bounds the output handed back to the model, so it cannot crowd out the turn.
// The spine records only a hash of the full output.
const maxDetail = 2000

// SpineEvidence is the [goal.Evidence] that appends each verification as a
// chain.ItemVerified event on the goal's stream. The event's sequence is its ref, and no
// two appends share one, so the gate can spend a verification exactly once.
type SpineEvidence struct {
	log spine.Log
}

// NewSpineEvidence returns a SpineEvidence over log.
func NewSpineEvidence(log spine.Log) *SpineEvidence { return &SpineEvidence{log: log} }

var _ goal.Evidence = (*SpineEvidence)(nil)

// Record appends one item verification as ActorSystem and returns it as the gate reads it.
//
// This is the only place the executed marker enters the record, and it comes from the
// verdict's Executed field, never from a model payload. A model that could claim
// execution would make the marker certify nothing. An executed verdict also carries the
// exit code and an output hash.
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
	} else if detail := strings.TrimSpace(v.Detail); detail != "" {
		// The reason tells an unrunnable clause from a sandbox that failed to start, which
		// otherwise read the same on a stopped goal.
		payload[chain.ItemReasonKey] = clip(detail, maxDetail)
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
	// Decode with the gate's own decoder, so producer and gate cannot drift on the ref
	// format or the provenance rule.
	vs := goal.VerificationsFrom([]spine.Event{e})
	if len(vs) != 1 {
		return goal.Verification{}, fault.New(fault.Terminal, "evidence_unreadable",
			"evidence: the recorded verification did not read back as one verification")
	}
	return vs[0], nil
}

// RecordAdmission appends one admission verdict as ActorSystem. Refusals are recorded too,
// so a reader sees what the planner was sent back for. The evidence gate does not read it.
func (s *SpineEvidence) RecordAdmission(ctx context.Context, r resource.Resource, v goal.AdmissionVerdict) error {
	payload := map[string]any{
		chain.ItemKey:         v.Item,
		chain.ItemAdmittedKey: v.Admitted,
		chain.ItemDryRunKey:   v.DryRun,
	}
	if v.Refusal != "" {
		payload[chain.ItemRefusalKey] = string(v.Refusal)
	}
	if reason := strings.TrimSpace(v.Reason); reason != "" {
		payload[chain.ItemReasonKey] = clip(reason, maxDetail)
	}
	if _, err := s.log.Append(ctx, spine.AppendInput{
		Stream:  r.Name,
		Type:    chain.ItemAdmitted,
		Actor:   spine.ActorSystem,
		Payload: payload,
	}); err != nil {
		return fault.Wrap(fault.Transient, "admission_append", err)
	}
	return nil
}

var _ goal.AdmissionRecorder = (*SpineEvidence)(nil)

// Recorded returns the goal's verifications in order. A read failure is Transient,
// because treating it as no evidence would stall a healthy goal.
func (s *SpineEvidence) Recorded(ctx context.Context, r resource.Resource) ([]goal.Verification, error) {
	events, err := s.log.Read(ctx, spine.Query{Stream: r.Name})
	if err != nil {
		return nil, fault.Wrap(fault.Transient, "evidence_read", err)
	}
	return goal.VerificationsFrom(events), nil
}

// outputHash names the observed output exactly without storing it.
func outputHash(out string) string {
	sum := sha256.Sum256([]byte(out))
	return hex.EncodeToString(sum[:])
}

// CommandVerifier runs an item's verify clause in the run's sandbox through the dispatch
// waist and treats exit 0 as proof. It follows learn.SandboxVerifier, which separates
// "the check failed" from "the check could not run".
type CommandVerifier struct {
	sb         sandbox.Sandbox
	dispatcher *dispatch.Dispatcher
}

// NewCommandVerifier runs checks in sb under a dispatcher built from opts. Pass the run's
// own admitter, sink and observability; with none the check is recorded but ungoverned.
func NewCommandVerifier(sb sandbox.Sandbox, opts ...dispatch.Option) *CommandVerifier {
	return &CommandVerifier{sb: sb, dispatcher: dispatch.New(opts...)}
}

var _ goal.ItemVerifier = (*CommandVerifier)(nil)

// VerifyItem runs the item's check. A check that cannot run (no clause, no sandbox, a
// refused admission) yields an unexecuted, unpassed verdict saying why, so the gate rules
// on it and the goal does not stall. Only a cancelled context is an error.
//
// The command runs as TrustSemi, the shell tool's level. A stronger class would make the
// containment gate refuse every check on hosts that run the shell tool.
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

// clip keeps the tail, because a failing command's diagnosis is almost always its last
// lines.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// ErrNoSandbox reports a verifier or auditor wired with no sandbox, a composition error.
var ErrNoSandbox = errors.New("evidence: command verifier has no sandbox")

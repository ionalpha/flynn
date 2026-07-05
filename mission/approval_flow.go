package mission

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/state"
)

// ApprovalRequest describes a privileged action the waist paused for a human decision:
// the action's name, the scope it would run in, and the host the run is on. Grace is how
// long the requester will wait before the request auto-declines, so an interactive
// prompter can show a countdown. It carries no action arguments, matching what the
// dispatch waist authorizes by (action identity, not payload).
type ApprovalRequest struct {
	Action string
	Scope  state.Scope
	Host   string
	Grace  time.Duration
}

// ApprovalDecision is a human's answer to an ApprovalRequest. Allow admits the action;
// Scope, when set, narrows the grant to an exact glob or target (bound into the minted
// approval, so the authorization does not over-grant); Feedback is a short reason fed
// back to the run on a denial so the model can adapt.
type ApprovalDecision struct {
	Allow    bool
	Scope    string
	Feedback string
}

// ApprovalPrompter resolves an approval request interactively (a TUI prompt) or by
// policy (a headless auto-decider). It is called on the run goroutine and blocks until
// the decision is made, so an implementation must observe ctx: a context deadline is the
// grace-period expiry, and the requester treats a context error or a returned error as a
// fail-closed decline.
type ApprovalPrompter interface {
	Prompt(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)
}

// defaultApprovalGrace bounds how long the waist waits on a human before a paused action
// auto-declines, so a run is never blocked forever on an unattended prompt. It is a
// fail-closed backstop; a prompter with its own shorter countdown resolves first.
const defaultApprovalGrace = 2 * time.Minute

// WithApprovalPrompter enables interactive resolution of a paused privileged action:
// when the waist returns NeedsApproval, the executor asks prompter for a decision and,
// on allow, mints a single-use approval signed by signer for host, presents it, and
// retries the action once. Without this option (or with a nil prompter or signer) a
// NeedsApproval rejection surfaces to the model unchanged, so the standalone agent is
// zero-config. It composes with WithApproval, which installs the gate that pauses the
// action in the first place.
func WithApprovalPrompter(prompter ApprovalPrompter, signer approval.Signer, host string) Option {
	return func(e *Executor) {
		if prompter == nil || signer == nil {
			return
		}
		e.prompter = prompter
		e.approvalSigner = signer
		e.approvalHost = host
		if e.approvalGrace == 0 {
			e.approvalGrace = defaultApprovalGrace
		}
		if e.approvalClock == nil {
			e.approvalClock = clock.System{}
		}
	}
}

// WithApprovalGrace overrides how long the waist waits on a human before a paused action
// auto-declines (default defaultApprovalGrace). A non-positive duration is ignored.
func WithApprovalGrace(d time.Duration) Option {
	return func(e *Executor) {
		if d > 0 {
			e.approvalGrace = d
		}
	}
}

// WithApprovalClock sets the clock the executor stamps a minted approval's validity
// window from, so a deterministic run (and a test) controls the window. The default is
// clock.System. It should match the verifier's clock.
func WithApprovalClock(c clock.Clock) Option {
	return func(e *Executor) {
		if c != nil {
			e.approvalClock = c
		}
	}
}

// governWithApproval governs a through the waist and, when the waist pauses the action
// for approval (NeedsApproval) and a prompter is configured, asks the prompter for a
// decision. On allow it mints a single-use approval bound to exactly this action, scope,
// principal, and (optional) target, presents it on the retry context, and governs once
// more; the gate rebuilds the same binding and admits the action against the presented
// approval. On deny it returns the feedback to the run so the model adapts, and on a
// grace-period timeout (or a prompter error) it declines fail-closed. Without a prompter,
// or for any outcome other than NeedsApproval, it is a bare Govern.
func (e *Executor) governWithApproval(ctx context.Context, a dispatch.Action, work func(context.Context) (dispatch.Metering, error)) error {
	err := e.dispatcher.Govern(ctx, a, work)
	if e.prompter == nil || fault.Classify(err) != fault.NeedsApproval {
		return err
	}

	req := ApprovalRequest{Action: a.Name, Scope: a.Scope, Host: e.approvalHost, Grace: e.approvalGrace}
	promptCtx := ctx
	if e.approvalGrace > 0 {
		var cancel context.CancelFunc
		promptCtx, cancel = context.WithTimeout(ctx, e.approvalGrace)
		defer cancel()
	}
	dec, derr := e.prompter.Prompt(promptCtx, req)
	if derr != nil {
		// A prompter error or a grace-period timeout is a fail-closed auto-decline: the
		// action stays refused, never admitted on a missing decision.
		return fault.New(fault.Forbidden, "approval_declined",
			"action "+a.Name+" was not approved within the grace period")
	}
	if !dec.Allow {
		msg := "action " + a.Name + " was not approved"
		if dec.Feedback != "" {
			msg += ": " + dec.Feedback
		}
		return fault.New(fault.Forbidden, "approval_denied", msg)
	}

	// Bind the minted approval to exactly what the operator authorized. A scope narrows
	// the grant to a target (Envelope.Detail), so the authorization cannot be widened to
	// another target; the gate rebuilds the same binding from this context.
	apprCtx := ctx
	if dec.Scope != "" {
		apprCtx = approval.WithDetail(ctx, dec.Scope)
	}
	env := approval.Binding(apprCtx, a, e.approvalHost)
	now := e.approvalClock.Now().UnixNano()
	env.Nonce = ids.New()
	env.Expiry = now + e.approvalGrace.Nanoseconds()
	appr, serr := e.approvalSigner.Sign(env)
	if serr != nil {
		return serr
	}
	return e.dispatcher.Govern(approval.Into(apprCtx, appr), a, work)
}

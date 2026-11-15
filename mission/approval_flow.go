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

// ApprovalRequest is a privileged action paused for a human. It carries no arguments
// because the waist authorizes by action identity. Grace is the time left before an
// auto-decline, so a prompter can show a countdown.
type ApprovalRequest struct {
	Action string
	Scope  state.Scope
	Host   string
	Grace  time.Duration
}

// ApprovalDecision answers an [ApprovalRequest]. Scope narrows the minted approval to one
// glob or target so it cannot over-grant; Feedback goes to the model on a denial.
type ApprovalDecision struct {
	Allow    bool
	Scope    string
	Feedback string
}

// ApprovalPrompter decides an approval interactively or by policy. It blocks the run
// goroutine, so it must observe ctx, whose deadline is the grace expiry; any error is a
// decline.
type ApprovalPrompter interface {
	Prompt(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)
}

// defaultApprovalGrace declines a paused action so an unattended prompt never blocks a run
// forever.
const defaultApprovalGrace = 2 * time.Minute

// WithApprovalPrompter asks prompter on NeedsApproval and, on allow, retries once with a
// single-use approval signed by signer for host. With a nil prompter or signer the refusal
// reaches the model unchanged. WithApproval installs the gate that pauses the action.
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

// WithApprovalGrace overrides [defaultApprovalGrace]. A non-positive duration is ignored.
func WithApprovalGrace(d time.Duration) Option {
	return func(e *Executor) {
		if d > 0 {
			e.approvalGrace = d
		}
	}
}

// WithApprovalClock sets the clock for a minted approval's validity window. It should match
// the verifier's clock.
func WithApprovalClock(c clock.Clock) Option {
	return func(e *Executor) {
		if c != nil {
			e.approvalClock = c
		}
	}
}

// governWithApproval mints an approval bound to exactly this action, scope, principal and
// target, so the gate rebuilds the same binding on the retry and nothing wider is admitted.
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
		// A missing decision never admits the action.
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

	// The scope goes into Envelope.Detail, so the approval cannot cover another target.
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

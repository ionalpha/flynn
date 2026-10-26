package governed

import (
	"context"
	"errors"

	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/spine"
)

// Kit is the governed ingredients of a run without a goal loop: the sandbox, the toolset,
// the grant and a dispatcher carrying the containment gate, the rate breaker and the
// governance sink. It is for a host that serves tool calls from a client (an MCP server)
// instead of driving a model, and it gets the same grant and the same record as a run
// assembled by [Assemble], so the two cannot drift apart.
type Kit struct {
	p   *parts
	brk *brakes.Hook
}

// NewKit builds a kit recording onto log. It accepts the options that describe the
// working tree ([WithWorkDir], [WithSandbox], [WithShellPolicy], [WithSkills],
// [WithRunID]) and [Waive] of [GuardBrakes]; an option that only a goal loop can use is
// an error rather than ignored.
func NewKit(ctx context.Context, log spine.Log, opts ...Option) (*Kit, error) {
	if log == nil {
		return nil, errors.New("governed: NewKit needs a log")
	}
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	if c.err != nil {
		return nil, c.err
	}
	if err := c.checkKitOptions(); err != nil {
		return nil, err
	}
	p, err := newParts(ctx, c, log, false)
	if err != nil {
		return nil, err
	}
	if err := recordWaivers(ctx, log, p.sess.ID(), c.waiveList); err != nil {
		_ = p.close()
		return nil, err
	}
	k := &Kit{p: p}
	if _, waived := c.waivers[GuardBrakes]; !waived {
		k.brk = DefaultBrakes()
	}
	return k, nil
}

// checkKitOptions refuses an option only a goal loop can use, and a waiver of a guard a
// kit does not have, so neither is accepted and then ignored.
func (c *config) checkKitOptions() error {
	if c.hasToolset || c.external != nil || c.fanout != nil || c.bridge != nil || c.hasCaps ||
		c.driver != "" || c.brake != nil || c.budget != nil || len(c.approve) > 0 || len(c.outside) > 0 ||
		c.prompter != nil || c.reporter != nil || c.planning {
		return errors.New("governed: NewKit takes only the working-tree options and Waive(GuardBrakes)")
	}
	for g := range c.waivers {
		if g != GuardBrakes {
			return errors.New("governed: NewKit can waive only GuardBrakes; " + string(g) + " has no meaning without a goal loop")
		}
	}
	return nil
}

// Toolset is the tools the kit offers.
func (k *Kit) Toolset() []mission.Tool { return append([]mission.Tool(nil), k.p.toolset...) }

// Grant is the full authority over the kit's toolset. A host that serves less (a
// read-only client) narrows it with [capability.NewGrant] and passes the result to
// [Kit.Bind]; the dispatcher admits only what the bound grant lists.
func (k *Kit) Grant() capability.Grant { return k.p.grant }

// SessionID is the spine stream the kit records onto.
func (k *Kit) SessionID() string { return k.p.sess.ID() }

// boundAdmitter is the capability admitter for a call that must carry a grant. The
// capability package admits everything on a context with none, which suits a standalone
// agent; a served tool call that skipped [Kit.Bind] would then run with no policy at all,
// and a refusal that names the missing step is what tells its author.
type boundAdmitter struct{ capability.Admitter }

func (a boundAdmitter) Admit(ctx context.Context, act dispatch.Action) error {
	if _, ok := capability.FromContext(ctx); !ok {
		return fault.New(fault.Forbidden, "capability_unbound",
			"action "+act.Name+" has no grant on its context; bind one with Kit.Bind before dispatching")
	}
	return a.Admitter.Admit(ctx, act)
}

// Dispatcher is the waist a served call crosses: the capability admitter, the governance
// sink, the containment gate over the kit's sandbox and the rate breaker. It is the same
// waist a goal loop's actions cross. A call whose context was not bound with [Kit.Bind] is
// refused, not admitted.
func (k *Kit) Dispatcher() *dispatch.Dispatcher {
	opts := []dispatch.Option{
		dispatch.WithAdmitter(boundAdmitter{}),
		dispatch.WithEventSink(k.p.sink),
		dispatch.WithHook(capability.NewContainmentGate(k.p.sb)),
	}
	if k.brk != nil {
		opts = append(opts, dispatch.WithHook(k.brk))
	}
	return dispatch.New(opts...)
}

// Bind returns ctx carrying grant and the kit's run identity, the bindings a goal loop's
// executor sets before it dispatches. The dispatcher holds no grant of its own.
func (k *Kit) Bind(ctx context.Context, grant capability.Grant) context.Context {
	return brakes.Into(capability.Into(ctx, grant), k.SessionID())
}

// Close releases a sandbox the kit built.
func (k *Kit) Close() error { return k.p.close() }

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

// Kit is a governed run without a goal loop, for a host that serves tool calls (an MCP
// server). It shares [Assemble]'s grant and record, so the two cannot drift apart.
type Kit struct {
	p   *parts
	brk *brakes.Hook
}

// NewKit accepts only [WithWorkDir], [WithSandbox], [WithShellPolicy], [WithSkills],
// [WithRunID] and [Waive] of [GuardBrakes]; any other option is an error, so none is
// accepted and then ignored.
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

// Toolset returns a copy of the kit's tools.
func (k *Kit) Toolset() []mission.Tool { return append([]mission.Tool(nil), k.p.toolset...) }

// Grant is full authority over the toolset. A host serving less narrows it with
// [capability.NewGrant] before [Kit.Bind].
func (k *Kit) Grant() capability.Grant { return k.p.grant }

// SessionID is the spine stream the kit records onto.
func (k *Kit) SessionID() string { return k.p.sess.ID() }

// boundAdmitter refuses a context with no grant, because the capability package admits
// everything then, and a served call that skipped [Kit.Bind] would run with no policy.
type boundAdmitter struct{ capability.Admitter }

func (a boundAdmitter) Admit(ctx context.Context, act dispatch.Action) error {
	if _, ok := capability.FromContext(ctx); !ok {
		return fault.New(fault.Forbidden, "capability_unbound",
			"action "+act.Name+" has no grant on its context; bind one with Kit.Bind before dispatching")
	}
	return a.Admitter.Admit(ctx, act)
}

// Dispatcher carries the same gates a goal loop's actions cross. It refuses a call whose
// context was not bound with [Kit.Bind].
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

// Bind returns ctx carrying grant and the kit's run identity; the dispatcher holds no
// grant of its own.
func (k *Kit) Bind(ctx context.Context, grant capability.Grant) context.Context {
	return brakes.Into(capability.Into(ctx, grant), k.SessionID())
}

// Close releases a sandbox the kit built.
func (k *Kit) Close() error { return k.p.close() }

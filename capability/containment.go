package capability

import (
	"context"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/sandbox"
)

// ContainmentGate is the dispatch hook that refuses an action whose trust level
// needs stronger isolation than the run's sandbox provides. Trusted work runs at
// any tier, semi-trusted work (a model-authored command) needs the kernel-confined
// tier, and untrusted work needs the hardware boundary. The action is refused, never
// downgraded, so a model-authored command cannot run on a host that cannot isolate
// it.
type ContainmentGate struct {
	sb sandbox.Sandbox
}

// NewContainmentGate builds a gate that checks each action's trust against sb.
func NewContainmentGate(sb sandbox.Sandbox) ContainmentGate {
	return ContainmentGate{sb: sb}
}

var _ dispatch.Hook = ContainmentGate{}

// Before refuses the action when the sandbox cannot contain its trust level. A nil
// sandbox means no containment is wired, so the gate admits, like the rest of the
// waist before it is configured.
func (g ContainmentGate) Before(_ context.Context, a dispatch.Action) error {
	if g.sb == nil {
		return nil
	}
	return sandbox.Admit(g.sb, a.Trust)
}

// After is a no-op: the gate holds nothing to release.
func (g ContainmentGate) After(context.Context, dispatch.Action, dispatch.Metering, error) {}

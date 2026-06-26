package capability

import (
	"context"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/sandbox"
)

// ContainmentGate is the dispatch hook that refuses an action whose trust level needs
// stronger isolation than the run's sandbox provides. It is the containment half of
// admission: where the Admitter checks that a grant permits the action by name, this
// checks that the host can actually contain the work. Trusted work (the agent's own
// tools) runs at any tier; semi-trusted work (a model-authored command) needs the
// kernel-confined tier; untrusted work needs the hardware boundary. On a host that
// cannot meet the requirement the action is refused before it runs, never downgraded,
// so a model-authored command cannot execute on a host that cannot isolate it.
type ContainmentGate struct {
	sb sandbox.Sandbox
}

// NewContainmentGate builds a gate that measures each action's trust against sb's
// containment.
func NewContainmentGate(sb sandbox.Sandbox) ContainmentGate {
	return ContainmentGate{sb: sb}
}

var _ dispatch.Hook = ContainmentGate{}

// Before refuses the action when the sandbox cannot contain its trust level. A nil
// sandbox means no containment context is wired, so the gate is permissive rather than
// blocking every action, matching the rest of the waist's default-open-until-configured
// posture.
func (g ContainmentGate) Before(_ context.Context, a dispatch.Action) error {
	if g.sb == nil {
		return nil
	}
	return sandbox.Admit(g.sb, a.Trust)
}

// After is a no-op: the gate only decides admission, it holds no resource to release.
func (g ContainmentGate) After(context.Context, dispatch.Action, dispatch.Metering, error) {}

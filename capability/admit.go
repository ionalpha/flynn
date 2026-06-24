package capability

import (
	"context"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// Admitter is the dispatch.Admitter that enforces the capability grant bound to an
// action's context. It is the governance gate at the waist: with a grant bound it
// admits only the actions the grant permits and denies the rest with a Forbidden
// fault, before the handler runs and so before any side effect. With no grant
// bound it admits everything, so the standalone agent runs unconstrained until a
// host binds a policy. It is stateless and safe to share.
type Admitter struct{}

var _ dispatch.Admitter = Admitter{}

// Admit denies an action that the run's grant does not permit. A run with no grant
// bound on its context is unconstrained.
func (Admitter) Admit(ctx context.Context, a dispatch.Action) error {
	g, ok := FromContext(ctx)
	if !ok {
		return nil // no policy bound: permissive, the zero-config default
	}
	if g.Allows(a.Name) {
		return nil
	}
	return fault.New(fault.Forbidden, "capability_denied",
		"action "+a.Name+" is not in the run's capability grant")
}

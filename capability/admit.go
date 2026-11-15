package capability

import (
	"context"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// Admitter is the dispatch.Admitter that enforces the grant bound to an action's
// context. It denies an action the grant omits with a Forbidden fault before the
// handler runs, so no side effect happens. With no grant bound it admits
// everything. It is stateless and safe to share.
type Admitter struct{}

var _ dispatch.Admitter = Admitter{}

// Admit denies an action the run's grant does not permit.
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

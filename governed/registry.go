package governed

import (
	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/internal/archetype"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/dependency"
	"github.com/ionalpha/flynn/internal/playbook"
	"github.com/ionalpha/flynn/internal/profilestore"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/resource"
)

// Registry builds the resource registry a durable store admits against: the core kinds,
// the Goal kind the runtime drives, the budget a run is charged to, and every kind the
// binary persists. A host opening its own store must use this registry, because a run
// spawns children and opens budgets that a store without those kinds would refuse.
func Registry() (*resource.Registry, error) {
	reg := resource.NewRegistry()
	for _, register := range []func(*resource.Registry) error{
		resource.RegisterCoreKinds,
		goal.RegisterKind,
		budgetpkg.RegisterKind,
		inbox.RegisterKind,
		profilestore.RegisterKind,
		archetype.RegisterKind,
		instance.RegisterKind,
		credential.RegisterKind,
		extension.RegisterKind,
		service.RegisterKind,
		dependency.RegisterKind,
		playbook.RegisterKind,
	} {
		if err := register(reg); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

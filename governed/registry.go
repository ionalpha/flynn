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

// Registry lists every kind the binary persists. A host opening its own store must use it,
// because a run spawns goals and opens budgets a narrower store would refuse.
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

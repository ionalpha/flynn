package sqlite_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/skill"
	"github.com/ionalpha/flynn/skill/skilltest"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// TestSkillFacadeConformance proves the typed skill facade behaves identically over
// the durable SQLite resource backend as over the in-memory one: the same SkillStore
// contract, now persisted, schema-admitted, and event-sourced on the shared spine.
func TestSkillFacadeConformance(t *testing.T) {
	skilltest.RunSuite(t, func() state.SkillStore {
		reg := resource.NewRegistry()
		if err := resource.RegisterCoreKinds(reg); err != nil {
			t.Fatalf("register core kinds: %v", err)
		}
		if err := skill.RegisterKind(reg); err != nil {
			t.Fatalf("register skill kind: %v", err)
		}
		p, err := sqlite.Open(context.Background(), ":memory:")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return skill.NewStore(p.Resources(reg))
	})
}

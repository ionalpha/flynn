package testkit

import (
	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// Generators for the core types. Defined once here and reused by every
// package's property tests — one generator replaces dozens of hand-written
// cases, and rapid shrinks any failure to a minimal reproducer.

// ScopeGen generates a state.Scope on the instance/project/workspace axis.
func ScopeGen() *rapid.Generator[state.Scope] {
	return rapid.Custom(func(t *rapid.T) state.Scope {
		return state.Scope{
			Instance:  rapid.StringMatching(`[a-z]{0,8}`).Draw(t, "instance"),
			Project:   rapid.StringMatching(`[a-z]{0,8}`).Draw(t, "project"),
			Workspace: rapid.StringMatching(`[a-z]{0,8}`).Draw(t, "workspace"),
		}
	})
}

// SkillGen generates a state.Skill with a non-empty slug.
func SkillGen() *rapid.Generator[state.Skill] {
	return rapid.Custom(func(t *rapid.T) state.Skill {
		return state.Skill{
			Slug:  rapid.StringMatching(`[a-z][a-z0-9-]{0,15}`).Draw(t, "slug"),
			Name:  rapid.String().Draw(t, "name"),
			Body:  rapid.String().Draw(t, "body"),
			Tags:  rapid.SliceOfN(rapid.StringMatching(`[a-z]{1,6}`), 0, 4).Draw(t, "tags"),
			Scope: ScopeGen().Draw(t, "scope"),
		}
	})
}

// MemoryItemGen generates a state.MemoryItem.
func MemoryItemGen() *rapid.Generator[state.MemoryItem] {
	return rapid.Custom(func(t *rapid.T) state.MemoryItem {
		return state.MemoryItem{
			Kind:    rapid.SampledFrom([]string{"fact", "preference", "observation"}).Draw(t, "kind"),
			Content: rapid.String().Draw(t, "content"),
			Scope:   ScopeGen().Draw(t, "scope"),
		}
	})
}

// ActionGen generates a dispatch.Action.
func ActionGen() *rapid.Generator[dispatch.Action] {
	return rapid.Custom(func(t *rapid.T) dispatch.Action {
		return dispatch.Action{
			Name:  rapid.StringMatching(`[a-z][a-z0-9_.]{0,15}`).Draw(t, "name"),
			Scope: ScopeGen().Draw(t, "scope"),
		}
	})
}

// AppendInputGen generates a spine.AppendInput for a fixed stream.
func AppendInputGen(stream string) *rapid.Generator[spine.AppendInput] {
	return rapid.Custom(func(t *rapid.T) spine.AppendInput {
		return spine.AppendInput{
			Stream:        stream,
			Type:          rapid.StringMatching(`[a-z][a-z0-9_.]{0,15}`).Draw(t, "type"),
			Actor:         rapid.SampledFrom([]spine.ActorType{spine.ActorAgent, spine.ActorHuman, spine.ActorSystem}).Draw(t, "actor"),
			SchemaVersion: rapid.IntRange(0, 3).Draw(t, "schema_version"),
		}
	})
}

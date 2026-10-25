// Package archetypes is the public home of the Agent description a host builds and
// hands to the agent. The Agent kind's engine (storage, schema, inheritance
// resolution) stays private; this package names the one value a host must be able
// to spell.
//
// The shipped archetypes live in the subpackages (see package review). They import
// this package rather than package agent, which imports them in turn.
package archetypes

import "github.com/ionalpha/flynn/internal/archetype"

// Spec is an Agent's desired shape: its system prompt, the exact capabilities a run
// as it may take, its model and driver, and the skill and memory scope it sees. It
// is an alias, so a host that names [Spec] and the engine that stores and resolves
// it are talking about one type, and a field added to the engine's type is a field
// here with no second declaration to keep in step.
type Spec = archetype.Spec

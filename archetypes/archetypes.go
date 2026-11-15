// Package archetypes exports the Agent description a host builds and hands to the
// agent; the engine that stores and resolves it stays private. The shipped
// archetypes are its subpackages, which import this package because package agent
// imports them.
package archetypes

import "github.com/ionalpha/flynn/internal/archetype"

// Spec is an Agent's desired shape: system prompt, capabilities, model, driver, and
// skill and memory scope. It is an alias so a field added to the engine's type needs
// no second declaration here.
type Spec = archetype.Spec

package governed

import (
	"context"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/sandbox"
)

// SkillVerifier builds the check a captured skill must pass before it is kept: a sandbox
// verifier that runs each check confined at dir, dispatched through the waist. Routing it
// through dispatch admits a verification against the grant and traces it like a tool call,
// where a side channel would execute a model-proposed command with no governance at all.
//
// Nothing binds a grant to this dispatcher, so its admitter is permissive; the confinement
// of the sandbox it runs in is what limits a check.
func SkillVerifier(dir string) learn.Verifier {
	inner := learn.NewSandboxVerifier(func(context.Context) (sandbox.Sandbox, error) {
		return sandbox.NewLocal(dir, sandbox.WithDefaultConfinement())
	})
	return learn.NewGovernedVerifier(inner, dispatch.WithAdmitter(capability.Admitter{}))
}

// Distiller wraps the model distiller so its model call runs through the dispatch waist,
// like the run's own model calls and [SkillVerifier]'s checks. Nothing binds a grant to it,
// so its admitter is permissive and it distills with the model's own limits.
func Distiller(model llm.Model) learn.Distiller {
	return learn.NewGovernedDistiller(learn.NewModelDistiller(model), dispatch.WithAdmitter(capability.Admitter{}))
}

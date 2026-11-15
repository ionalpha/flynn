package governed

import (
	"context"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/sandbox"
)

// SkillVerifier runs a captured skill's checks confined at dir, through the dispatch waist,
// so a model-proposed command is traced like a tool call. No grant is bound, so the admitter
// is permissive and the sandbox's confinement limits a check.
func SkillVerifier(dir string) learn.Verifier {
	inner := learn.NewSandboxVerifier(func(context.Context) (sandbox.Sandbox, error) {
		return sandbox.NewLocal(dir, sandbox.WithDefaultConfinement())
	})
	return learn.NewGovernedVerifier(inner, dispatch.WithAdmitter(capability.Admitter{}))
}

// Distiller routes the distilling model call through the dispatch waist. As with
// [SkillVerifier], no grant is bound, so the admitter is permissive.
func Distiller(model llm.Model) learn.Distiller {
	return learn.NewGovernedDistiller(learn.NewModelDistiller(model), dispatch.WithAdmitter(capability.Admitter{}))
}

package learn

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// Verdict is the result of a skill's check. Ran false means no check or a sandbox
// that could not start it, so the skill is unverified; Ran without Verified means
// broken. Detail is a short reason for a person.
type Verdict struct {
	Verified bool
	Ran      bool
	Detail   string
}

// Verifier runs a skill lesson's check.
type Verifier interface {
	// Verify runs l's check. scope attributes the check to the run that proposed it
	// when [GovernedVerifier] dispatches it.
	Verify(ctx context.Context, l Lesson, scope state.Scope) (Verdict, error)
}

// SandboxFactory creates one sandbox per check, so checks stay independent and are
// confined like the agent's tools.
type SandboxFactory func(ctx context.Context) (sandbox.Sandbox, error)

// SandboxVerifier runs a check as a shell command in a sandbox; exit 0 verifies.
// The sandbox is what makes running a model-proposed command safe.
type SandboxVerifier struct {
	newSandbox SandboxFactory
}

// NewSandboxVerifier runs each check in a sandbox from f.
func NewSandboxVerifier(f SandboxFactory) *SandboxVerifier {
	return &SandboxVerifier{newSandbox: f}
}

var _ Verifier = (*SandboxVerifier)(nil)

// Verify returns Ran false for a lesson with no check or a sandbox that fails, so a
// sandbox fault keeps the skill unverified. Only a cancelled context is an error.
func (v *SandboxVerifier) Verify(ctx context.Context, l Lesson, _ state.Scope) (Verdict, error) {
	check := strings.TrimSpace(l.Check)
	if l.Kind != LessonSkill || check == "" {
		return Verdict{Detail: "no check"}, nil
	}
	sb, err := v.newSandbox(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return Verdict{}, ctx.Err()
		}
		return Verdict{Detail: "sandbox unavailable: " + err.Error()}, nil
	}
	defer func() { _ = sb.Close() }()

	res, err := sb.Exec(ctx, sandbox.Command{Line: check})
	if err != nil {
		if ctx.Err() != nil {
			return Verdict{}, ctx.Err()
		}
		return Verdict{Detail: "check could not run: " + err.Error()}, nil
	}
	return Verdict{
		Verified: res.ExitCode == 0,
		Ran:      true,
		Detail:   fmt.Sprintf("exit %d", res.ExitCode),
	}, nil
}

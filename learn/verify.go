package learn

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/sandbox"
)

// Verdict is the outcome of verifying a skill's check. Ran reports whether the
// check actually executed: a check that ran and did not verify is proven broken,
// while one that could not run (none was supplied, or the sandbox failed to start
// it) is simply unproven. Detail is a short human-readable reason.
type Verdict struct {
	Verified bool
	Ran      bool
	Detail   string
}

// Verifier decides whether a skill lesson is sound by running its check. It is the
// gate that makes a captured procedure trustworthy in proportion to evidence: the
// Curator crystallizes a verified skill, drops a proven-broken one, and keeps an
// unproven one tagged unverified.
type Verifier interface {
	Verify(ctx context.Context, l Lesson) (Verdict, error)
}

// SandboxFactory creates the sandbox a single verification runs in. A fresh
// sandbox per check keeps verifications independent and confines a check the same
// way the agent's own tools are confined.
type SandboxFactory func(ctx context.Context) (sandbox.Sandbox, error)

// SandboxVerifier runs a skill's check as a shell command in a sandbox and treats a
// zero exit code as verification. Running the candidate before keeping it is what
// separates a procedure that works from one that merely sounds plausible; doing it
// inside the sandbox is what makes executing model-proposed commands safe.
type SandboxVerifier struct {
	newSandbox SandboxFactory
}

// NewSandboxVerifier builds a verifier that runs each check in a sandbox from f.
func NewSandboxVerifier(f SandboxFactory) *SandboxVerifier {
	return &SandboxVerifier{newSandbox: f}
}

var _ Verifier = (*SandboxVerifier)(nil)

// Verify runs l's check in a fresh sandbox. A skill with no check is unproven
// (Ran=false), not broken. A sandbox that cannot start the check is unproven too,
// not an error, so a verification-environment hiccup keeps the skill (tagged
// unverified) rather than discarding it; only a cancelled context is a hard error.
func (v *SandboxVerifier) Verify(ctx context.Context, l Lesson) (Verdict, error) {
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

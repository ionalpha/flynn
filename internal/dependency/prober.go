package dependency

import (
	"context"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/sandbox"
)

// SandboxProber runs a program's version command through the sandbox, so detection
// never spawns a process directly. The command is the program name plus the spec's
// literal version arguments, with no model-authored input.
type SandboxProber struct{ sb sandbox.Sandbox }

// NewSandboxProber wraps a sandbox as a version prober.
func NewSandboxProber(sb sandbox.Sandbox) SandboxProber { return SandboxProber{sb: sb} }

// Probe runs "name args..." through the sandbox and returns its output. A run error
// or nonzero exit is an error, which the manager reads as not usable.
func (p SandboxProber) Probe(ctx context.Context, name string, args []string) (string, error) {
	if p.sb == nil {
		return "", fault.New(fault.Terminal, "dependency_no_sandbox", "dependency: no sandbox configured for the version probe")
	}
	line := name
	if len(args) > 0 {
		line += " " + strings.Join(args, " ")
	}
	res, err := p.sb.Exec(ctx, sandbox.Command{Line: line})
	if err != nil {
		return "", fault.Wrap(fault.Transient, "dependency_probe", err)
	}
	if res.ExitCode != 0 {
		return "", fault.New(fault.Transient, "dependency_probe_exit", "dependency: "+name+" version probe exited non-zero")
	}
	return strings.TrimSpace(res.Output), nil
}

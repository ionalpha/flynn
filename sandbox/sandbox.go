// Package sandbox is the port through which every run's shell commands and file
// operations reach the host. A directory jail (Local), a container, a microVM and
// a remote sandbox implement the same port, so tools do not change when the
// confinement does. The dispatch Admitter decides whether an action may run; the
// Sandbox confines it.
package sandbox

import (
	"context"

	"github.com/ionalpha/flynn/fault"
)

// Command is a shell command to run inside a sandbox.
type Command struct {
	// Line is interpreted by the sandbox's shell.
	Line string
	// Stdin is written to standard input and then closed. Pass a secret here,
	// because argv is visible in the process table and Line reaches logs.
	Stdin []byte
}

// ExecResult carries a non-zero ExitCode as a result; an error from Exec means
// the command did not run.
type ExecResult struct {
	Output   string // combined stdout and stderr
	ExitCode int
}

// Sandbox resolves paths against its root and rejects any that escape. It must be
// safe for concurrent use. A command must see only a credential-free baseline plus
// explicit grants, never the agent's environment, so no secret the agent holds
// reaches a command by default.
type Sandbox interface {
	// Exec errors only when the command could not run.
	Exec(ctx context.Context, cmd Command) (ExecResult, error)
	// ReadFile rejects a path outside the root.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile creates parent directories.
	WriteFile(ctx context.Context, path string, data []byte) error
	// Glob returns root-relative paths.
	Glob(ctx context.Context, pattern string) ([]string, error)
	// Walk returns regular files only, relative to the sandbox root.
	Walk(ctx context.Context, root string) ([]string, error)
	// Close releases the sandbox's resources, such as its scratch directory.
	Close() error
}

// ErrDenied covers a path escaping the root and, in stronger tiers, a denied
// syscall or egress.
var ErrDenied = fault.New(fault.Terminal, "sandbox_denied", "operation denied by the sandbox boundary")

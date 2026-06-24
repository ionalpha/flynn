// Package sandbox is the agent's execution boundary: the port every run's work -
// shell commands and file operations today, computer-use and plugin code later -
// goes through rather than touching the host directly. It is the execution analog
// of state.Provider. Swapping the implementation changes where and under what
// confinement work runs without changing the tools that sit above it: an
// in-process directory jail (Local), a hardened container, a microVM, or a remote
// cloud sandbox are all the same port.
//
// Isolation is a seam the run model goes through from the first execution path,
// not bolted on later. The dispatch waist's Admitter decides whether an action may
// run (the capability chokepoint); the Sandbox is where that grant becomes real
// confinement. The two compose: governance above, isolation below.
package sandbox

import (
	"context"

	"github.com/ionalpha/flynn/fault"
)

// Command is a shell command to run inside a sandbox.
type Command struct {
	// Line is the command line, interpreted by the sandbox's shell.
	Line string
}

// ExecResult is the outcome of a Command. A non-zero ExitCode is a normal result,
// not an error: the command ran and the caller reads its output and code. An error
// from Exec means the command could not be run at all.
type ExecResult struct {
	Output   string // combined stdout and stderr
	ExitCode int
}

// Sandbox is the execution boundary. All paths are relative to the sandbox root,
// and an implementation must reject any operation that would reach outside its
// boundary (default-deny). Implementations must be safe for concurrent use.
type Sandbox interface {
	// Exec runs a command inside the sandbox.
	Exec(ctx context.Context, cmd Command) (ExecResult, error)
	// ReadFile reads a file within the sandbox.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile writes a file within the sandbox, creating parent directories.
	WriteFile(ctx context.Context, path string, data []byte) error
	// Glob lists paths matching a shell pattern within the sandbox, relative to the
	// root.
	Glob(ctx context.Context, pattern string) ([]string, error)
	// Walk returns the regular-file paths under root within the sandbox, relative
	// to the sandbox root.
	Walk(ctx context.Context, root string) ([]string, error)
	// Close releases the sandbox's resources.
	Close() error
}

// ErrDenied is returned when an operation falls outside the sandbox's grant: a
// path that escapes the root, or (in stronger tiers) a denied syscall or egress.
// The boundary is default-deny.
var ErrDenied = fault.New(fault.Terminal, "sandbox_denied", "operation denied by the sandbox boundary")

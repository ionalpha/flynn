//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os/exec"

	"golang.org/x/sys/windows"
)

// captureRun confines with Exec's policy, applied at process creation, which an
// exec.Cmd cannot carry, so it bypasses captureExec.
func (l *Local) captureRun(ctx context.Context, spec CaptureSpec, confined bool) (ExecResult, error) {
	if !confined {
		return l.captureExec(ctx, spec, false)
	}
	if l.hostReadable {
		return l.captureWriteRestricted(ctx, spec)
	}
	return l.captureAppContainer(ctx, spec)
}

// captureWriteRestricted invokes the program as captureAppContainer does.
func (l *Local) captureWriteRestricted(ctx context.Context, spec CaptureSpec) (ExecResult, error) {
	appPath, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: capture: command not found: %s: %w", spec.Argv[0], err)
	}
	if err := l.grantRestrictedDir(ctx, l.root); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: grant working directory: %w", err)
	}
	if err := l.grantRestrictedWritableDirs(ctx); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: %w", err)
	}
	cmdline := windows.ComposeCommandLine(spec.Argv)
	env := l.appContainerEnvBlock(l.streamEnv(spec.Env))
	return launchWriteRestricted(ctx, appPath, cmdline, l.root, l.root, env, spec.Stdin, l.resLimits)
}

// captureAppContainer runs argv without cmd.exe and drains it through
// launchAppContainer, which terminates the process if ctx is cancelled.
func (l *Local) captureAppContainer(ctx context.Context, spec CaptureSpec) (ExecResult, error) {
	// Resolved here so a missing program gives a clear error.
	appPath, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: capture: command not found: %s: %w", spec.Argv[0], err)
	}
	cmdline := windows.ComposeCommandLine(spec.Argv)

	sid, err := createOrDeriveACSID(appContainerMoniker(l.root))
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: appcontainer profile: %w", err)
	}
	defer func() { _ = windows.FreeSid(sid) }()

	if err := l.grantDir(ctx, l.root, sid); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: grant working directory: %w", err)
	}
	if err := l.grantReadableDirs(ctx, sid); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: %w", err)
	}
	if err := l.grantWritableDirs(ctx, sid); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: %w", err)
	}

	var caps []*windows.SID
	if !l.denyNetwork {
		netCap, err := capabilitySID("internetClient")
		if err != nil {
			return ExecResult{}, fmt.Errorf("sandbox: network capability: %w", err)
		}
		caps = append(caps, netCap)
	}

	env := l.appContainerEnvBlock(l.streamEnv(spec.Env))
	return launchAppContainer(ctx, appPath, cmdline, l.root, env, sid, caps, spec.Stdin, l.resLimits)
}

//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os/exec"

	"golang.org/x/sys/windows"
)

// captureRun runs a program to completion. Unconfined, it runs through the standard
// library exactly like the other platforms. Confined, it runs the program directly (no
// shell) inside the working directory's AppContainer with the same policy Exec and Stream
// use: the working directory the one writable location, the network granted only when it
// was not denied, and the process hardened with the mitigation policies. The AppContainer
// is applied through process creation, which an exec.Cmd cannot carry, so the confined
// path does not go through captureExec.
func (l *Local) captureRun(ctx context.Context, spec CaptureSpec, confined bool) (ExecResult, error) {
	if !confined {
		return l.captureExec(ctx, spec, false)
	}
	if l.hostReadable {
		return l.captureWriteRestricted(ctx, spec)
	}
	return l.captureAppContainer(ctx, spec)
}

// captureWriteRestricted runs spec.Argv to completion under the write-restricted tier and
// returns its combined output and exit code. It resolves and invokes the program exactly
// as the container path does; only the confinement mechanism differs.
func (l *Local) captureWriteRestricted(ctx context.Context, spec CaptureSpec) (ExecResult, error) {
	appPath, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: capture: command not found: %s: %w", spec.Argv[0], err)
	}
	if err := grantRestrictedDir(l.root, l.root); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: grant working directory: %w", err)
	}
	cmdline := windows.ComposeCommandLine(spec.Argv)
	env := l.appContainerEnvBlock(l.streamEnv(spec.Env))
	return launchWriteRestricted(ctx, appPath, cmdline, l.root, env, spec.Stdin, l.resLimits)
}

// captureAppContainer runs spec.Argv to completion inside the working directory's
// AppContainer and returns its combined output and exit code. It sets up the same argv
// invocation as the streaming path (the program resolved on PATH and run directly, never
// through cmd.exe) and then drains it through launchAppContainer, which reads the combined
// output, waits for exit, and terminates the process if ctx is cancelled.
func (l *Local) captureAppContainer(ctx context.Context, spec CaptureSpec) (ExecResult, error) {
	// The program is found on the sandbox's PATH and run directly. Resolving it here (not
	// leaving it to the loader) keeps the failure legible and matches how the other
	// platforms exec the argv without a shell.
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

	if err := grantDir(l.root, sid); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: grant working directory: %w", err)
	}
	if err := l.grantReadableDirs(sid); err != nil {
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

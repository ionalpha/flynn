//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"golang.org/x/sys/windows"
)

// startStream confines with Exec's AppContainer policy, applied at process
// creation, which an exec.Cmd cannot carry, so it bypasses startStreamExec.
func (l *Local) startStream(ctx context.Context, spec StreamSpec, confined bool) (*StreamProcess, error) {
	if !confined {
		return l.startStreamExec(ctx, spec, false)
	}
	return l.startStreamAppContainer(ctx, spec)
}

// startStreamAppContainer shares spawnAppContainer with Exec but runs argv
// without cmd.exe and returns the process running. Cancelling ctx terminates it.
func (l *Local) startStreamAppContainer(ctx context.Context, spec StreamSpec) (*StreamProcess, error) {
	// Resolved here so a missing program gives a clear error.
	appPath, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		return nil, fmt.Errorf("sandbox: stream: command not found: %s: %w", spec.Argv[0], err)
	}
	cmdline := windows.ComposeCommandLine(spec.Argv)

	env := l.appContainerEnvBlock(l.streamEnv(spec.Env))

	var p *acProcess
	if l.hostReadable {
		// The write-restricted tier needs only the workspace write grant.
		if err := l.grantRestrictedDir(ctx, l.root); err != nil {
			return nil, fmt.Errorf("sandbox: grant working directory: %w", err)
		}
		if err := l.grantRestrictedWritableDirs(ctx); err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
		p, err = spawnWriteRestricted(appPath, cmdline, l.root, l.root, env, confinedIO{stdinOnce: spec.Stdin}, l.resLimits)
		if err != nil {
			return nil, err
		}
	} else {
		sid, err := createOrDeriveACSID(appContainerMoniker(l.root))
		if err != nil {
			return nil, fmt.Errorf("sandbox: appcontainer profile: %w", err)
		}
		defer func() { _ = windows.FreeSid(sid) }() // the child holds its own token once created

		if err := l.grantDir(ctx, l.root, sid); err != nil {
			return nil, fmt.Errorf("sandbox: grant working directory: %w", err)
		}
		if err := l.grantReadableDirs(ctx, sid); err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
		if err := l.grantWritableDirs(ctx, sid); err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}

		var caps []*windows.SID
		if !l.denyNetwork {
			netCap, err := capabilitySID("internetClient")
			if err != nil {
				return nil, fmt.Errorf("sandbox: network capability: %w", err)
			}
			caps = append(caps, netCap)
		}

		p, err = spawnAppContainer(appPath, cmdline, l.root, env, sid, caps, confinedIO{stdinOnce: spec.Stdin}, l.resLimits)
		if err != nil {
			return nil, err
		}
	}

	// Terminating on cancel closes the pipe, so a blocked read returns.
	exited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = windows.TerminateProcess(p.pi.Process, 1)
		case <-exited:
		}
	}()

	// Closing stdout in wait closes p.read, so closeProcess must not.
	stdout := os.NewFile(uintptr(p.read), "sandbox-appcontainer-stdout")

	var once sync.Once
	var waitErr error
	wait := func() error {
		once.Do(func() {
			_, _ = windows.WaitForSingleObject(p.pi.Process, windows.INFINITE)
			close(exited) // stop the ctx watcher
			var code uint32
			_ = windows.GetExitCodeProcess(p.pi.Process, &code)
			_ = stdout.Close() // closes p.read
			p.closeProcess()
			if code != 0 {
				waitErr = fmt.Errorf("sandbox: stream: exit status %d", code)
			}
		})
		return waitErr
	}
	return &StreamProcess{stdout: stdout, wait: wait}, nil
}

//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"golang.org/x/sys/windows"
)

// duplexConfinementExpressible is true because both Windows tiers carry a
// session's three inherited pipes. Governed egress is refused earlier, at
// guardEgress; network is all or nothing through internetClient.
func duplexConfinementExpressible() bool { return true }

// startSession confines with the policy Exec and Stream use.
func (l *Local) startSession(ctx context.Context, spec SessionSpec, confined bool) (*Session, error) {
	if !confined {
		return l.startSessionExec(ctx, spec, false)
	}
	return l.startSessionAppContainer(ctx, spec)
}

// startSessionAppContainer matches startStreamAppContainer's setup but wires three
// separate pipes. Cancelling ctx terminates the process, closing its pipes.
func (l *Local) startSessionAppContainer(ctx context.Context, spec SessionSpec) (*Session, error) {
	appPath, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		return nil, fmt.Errorf("sandbox: session: command not found: %s: %w", spec.Argv[0], err)
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
		p, err = spawnWriteRestricted(appPath, cmdline, l.root, l.root, env, confinedIO{duplex: true}, l.resLimits)
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

		p, err = spawnAppContainer(appPath, cmdline, l.root, env, sid, caps, confinedIO{duplex: true}, l.resLimits)
		if err != nil {
			return nil, err
		}
	}

	// Stop cancels sctx, which terminates the process.
	sctx, cancel := context.WithCancel(ctx)
	exited := make(chan struct{})
	go func() {
		select {
		case <-sctx.Done():
			_ = windows.TerminateProcess(p.pi.Process, 1)
		case <-exited:
		}
	}()

	stdin := os.NewFile(uintptr(p.writeIn), "sandbox-appcontainer-stdin")
	stdout := os.NewFile(uintptr(p.read), "sandbox-appcontainer-stdout")
	stderrFile := os.NewFile(uintptr(p.errRead), "sandbox-appcontainer-stderr")

	// Drained into a bounded tail so a chatty child cannot block on a full pipe.
	tail := newTailBuffer(tailBufferCap)
	go func() { _, _ = io.Copy(tail, stderrFile) }()

	var once sync.Once
	var waitErr error
	wait := func() error {
		once.Do(func() {
			_, _ = windows.WaitForSingleObject(p.pi.Process, windows.INFINITE)
			close(exited) // stop the ctx watcher
			var code uint32
			_ = windows.GetExitCodeProcess(p.pi.Process, &code)
			_ = stdin.Close()
			_ = stdout.Close()
			_ = stderrFile.Close()
			p.closeProcess()
			if code != 0 {
				waitErr = fmt.Errorf("sandbox: session: exit status %d", code)
			}
		})
		return waitErr
	}
	return &Session{stdin: stdin, stdout: stdout, tail: tail, cancel: cancel, wait: wait}, nil
}

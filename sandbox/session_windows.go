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

// duplexConfinementExpressible is true on Windows: an AppContainer (or the write-restricted
// token tier) can carry a duplex session's three inherited pipe handles (a live stdin and
// separate stdout and stderr) exactly as it carries the streaming launch's, so a confined
// session runs under the container rather than being refused. Governed per-host egress still
// has no Windows enforcement leg and is refused earlier, at guardEgress; network is denied or
// allowed wholesale through the container's internetClient capability.
func duplexConfinementExpressible() bool { return true }

// startSession launches a duplex session. Unconfined, it runs through the standard library
// like the other platforms. Confined, it runs inside the AppContainer (or the workspace's
// write-restricted token) with the same policy Exec and Stream use, handing the process's
// live stdin, stdout, and stderr pipes back to the caller.
func (l *Local) startSession(ctx context.Context, spec SessionSpec, confined bool) (*Session, error) {
	if !confined {
		return l.startSessionExec(ctx, spec, false)
	}
	return l.startSessionAppContainer(ctx, spec)
}

// startSessionAppContainer starts spec.Argv confined and returns a Session over its own
// stdio pipes, bound to ctx: cancelling ctx (a Stop or a shutdown) terminates the process,
// which closes its pipes so a blocked read returns and Wait completes. It mirrors
// startStreamAppContainer's container setup (the same identity, grants, capabilities, and
// mitigation policy) and differs only in wiring three separate pipes for the interactive
// conversation instead of one combined output stream.
func (l *Local) startSessionAppContainer(ctx context.Context, spec SessionSpec) (*Session, error) {
	appPath, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		return nil, fmt.Errorf("sandbox: session: command not found: %s: %w", spec.Argv[0], err)
	}
	cmdline := windows.ComposeCommandLine(spec.Argv)
	env := l.appContainerEnvBlock(l.streamEnv(spec.Env))

	var p *acProcess
	if l.hostReadable {
		// The write-restricted tier: no container identity, only the workspace write grant.
		if err := grantRestrictedDir(l.root, l.root); err != nil {
			return nil, fmt.Errorf("sandbox: grant working directory: %w", err)
		}
		if err := l.grantRestrictedWritableDirs(); err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
		p, err = spawnWriteRestricted(appPath, cmdline, l.root, env, confinedIO{duplex: true}, l.resLimits)
		if err != nil {
			return nil, err
		}
	} else {
		sid, err := createOrDeriveACSID(appContainerMoniker(l.root))
		if err != nil {
			return nil, fmt.Errorf("sandbox: appcontainer profile: %w", err)
		}
		defer func() { _ = windows.FreeSid(sid) }() // the child holds its own token once created

		if err := grantDir(l.root, sid); err != nil {
			return nil, fmt.Errorf("sandbox: grant working directory: %w", err)
		}
		if err := l.grantReadableDirs(sid); err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
		if err := l.grantWritableDirs(sid); err != nil {
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

	// Bind the process to an internal context cancelled by Stop, so a Stop or a shutdown
	// terminates it, which closes its pipes so a blocked read returns and Wait completes.
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

	// Drain the child's stderr into a bounded tail so a chatty child cannot block on a full
	// stderr pipe and so its diagnostics are retained without unbounded buffering.
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

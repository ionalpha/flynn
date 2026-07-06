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

// startStream launches a streamed subprocess. Unconfined, it runs through the standard
// library exactly like the other platforms. Confined, it runs the program directly (no
// shell) inside an AppContainer with the same policy Exec uses: the working directory the
// one writable location, the network granted only when it was not denied, and the process
// hardened with the mitigation policies. The AppContainer is applied through process
// creation, which an exec.Cmd cannot carry, so the confined path does not go through
// startStreamExec.
func (l *Local) startStream(ctx context.Context, spec StreamSpec, confined bool) (*StreamProcess, error) {
	if !confined {
		return l.startStreamExec(ctx, spec, false)
	}
	return l.startStreamAppContainer(ctx, spec)
}

// startStreamAppContainer starts spec.Argv inside the working directory's AppContainer
// and returns a handle to its live stdout, bound to ctx: a cancellation terminates the
// process. It reuses spawnAppContainer (shared with the one-shot Exec path); the only
// differences are that the program is run directly from its argv rather than through
// cmd.exe, and the process is handed back running rather than drained to completion.
func (l *Local) startStreamAppContainer(ctx context.Context, spec StreamSpec) (*StreamProcess, error) {
	// The program is found on the sandbox's PATH and run directly. Resolving it here (not
	// leaving it to the loader) keeps the failure legible and matches how the other
	// platforms exec the argv without a shell.
	appPath, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		return nil, fmt.Errorf("sandbox: stream: command not found: %s: %w", spec.Argv[0], err)
	}
	cmdline := windows.ComposeCommandLine(spec.Argv)

	sid, err := createOrDeriveACSID(appContainerMoniker(l.root))
	if err != nil {
		return nil, fmt.Errorf("sandbox: appcontainer profile: %w", err)
	}
	defer func() { _ = windows.FreeSid(sid) }() // the child holds its own token once created

	if err := grantDir(l.root, sid); err != nil {
		return nil, fmt.Errorf("sandbox: grant working directory: %w", err)
	}

	var caps []*windows.SID
	if !l.denyNetwork {
		netCap, err := capabilitySID("internetClient")
		if err != nil {
			return nil, fmt.Errorf("sandbox: network capability: %w", err)
		}
		caps = append(caps, netCap)
	}

	env := l.appContainerEnvBlock(l.streamEnv(spec.Env))
	p, err := spawnAppContainer(appPath, cmdline, l.root, env, sid, caps, spec.Stdin)
	if err != nil {
		return nil, err
	}

	// Bind the process to ctx: a cancellation (a halt or shutdown) terminates it, which
	// closes its output pipe so a blocked read returns and Wait then completes. The watcher
	// stops when the process exits on its own, signalled by the wait closing exited.
	exited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = windows.TerminateProcess(p.pi.Process, 1)
		case <-exited:
		}
	}()

	// The parent's read end of the combined-output pipe, as a live stream. Closing the
	// file (in wait) closes the handle, so closeProcess must not also close read.
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

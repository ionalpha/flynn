//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ionalpha/flynn/internal/procexec"
)

// checkTimeout bounds one pass so a wedged child cannot hang a launch. A timeout
// refuses.
const checkTimeout = 15 * time.Second

// checkHost runs the ladder for n under [checkTimeout].
func checkHost(ctx context.Context, n needs) Readiness {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	return check(ctx, n, probe{run: runStage, proc: readProc, arch: runtime.GOARCH})
}

// readProc reads a /proc file or a container marker file for the classifier.
func readProc(path string) (string, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // path is one of the fixed files the classifier names, never caller input
	if err != nil {
		return "", false
	}
	return string(b), true
}

// runStage runs one rung as a confined no-op child.
func runStage(ctx context.Context, s stage, n needs) *failure {
	switch s {
	case stageLauncher:
		return runLauncherStage(ctx, n)
	case stageCanary:
		return runCanaryStage(ctx, n)
	case stageUser, stageNet, stageMount:
		// The namespace rungs share one runner.
	}
	return runNamespaceStage(ctx, s)
}

// runNamespaceStage uses the clone flags and identity mapping [Local.confine] sets.
func runNamespaceStage(ctx context.Context, s stage) *failure {
	name, args := shell("exit 0")
	//nolint:gosec // the command is the fixed no-op from shell("exit 0"); starting it in the namespaces under test is the check
	c := procexec.CommandContext(ctx, name, args...)
	c.Dir = "/"
	c.Env = []string{}
	sp := &syscall.SysProcAttr{}
	enterUserNamespace(sp)
	switch s {
	case stageUser, stageLauncher, stageCanary:
		// The user namespace is entered above; the launcher stage has its own runner.
	case stageNet:
		sp.Cloneflags |= syscall.CLONE_NEWNET
	case stageMount:
		sp.Cloneflags |= mountNamespaceFlags()
	}
	c.SysProcAttr = sp
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	return failureOf(s, c.Run(), out.String())
}

// checkLocal skips the gate, because the gate runs this check. sigsys is set when a
// command dies of SIGSYS.
func checkLocal(dir string, n needs, sigsys *bool) (*Local, error) {
	opts := []LocalOption{withoutGate(), func(l *Local) {
		l.onExit = func(e *exec.ExitError) { *sigsys = diedOfSIGSYS(e) }
	}}
	if n.mount {
		opts = append(opts, WithReadOnlyFS())
	}
	if n.seccomp {
		opts = append(opts, WithSeccomp())
	}
	if n.net {
		opts = append(opts, WithNetworkDenied())
	}
	return NewLocal(dir, opts...)
}

// runLauncherStage treats a non-zero exit as a failure, because the launcher
// reports its failures as an exit code and a message.
func runLauncherStage(ctx context.Context, n needs) *failure {
	dir, err := os.MkdirTemp("", "flynn-sandbox-check-")
	if err != nil {
		return &failure{stage: stageLauncher, raw: "create check directory: " + err.Error()}
	}
	defer func() { _ = os.RemoveAll(dir) }()
	var sigsys bool
	l, err := checkLocal(dir, n, &sigsys)
	if err != nil {
		return failureOf(stageLauncher, err, "")
	}
	defer func() { _ = l.Close() }()
	res, err := l.Exec(ctx, Command{Line: "exit 0"})
	if err != nil {
		return failureOf(stageLauncher, err, "")
	}
	if res.ExitCode != 0 {
		return &failure{stage: stageLauncher, raw: fmt.Sprintf("exit status %d", res.ExitCode), output: res.Output, signal: sigsys}
	}
	return nil
}

// A canary's exit status is its verdict, so a missing tool or unreadable file is
// told apart from a stop by the confinement.
const (
	canaryEscaped   = 0 // the command did what the confinement is there to prevent
	canaryContained = 1 // the confinement stopped it
)

// canaries returns commands, keyed by what they try, that succeed unconfined.
// outside must stay read-only.
func canaries(n needs, outside string) map[string]string {
	out := map[string]string{}
	if n.mount {
		// The redirect runs in a subshell: a failed redirect on a special builtin ends a
		// POSIX shell with status 2, which would read as inconclusive.
		out["write outside the working directory"] = "if (: > " + shellQuote(filepath.Join(outside, "escaped")) + ") 2>/dev/null; then exit 0; fi; exit 1"
	}
	if n.net {
		out["see a network interface of the host"] = `[ -r /proc/net/dev ] || exit 3; ` +
			`while read -r name rest; do case "$name" in lo:*|Inter-*|face) ;; *:*) exit 0;; esac; done < /proc/net/dev; exit 1`
	}
	return out
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// runCanaryStage refuses the host when a negative canary ends any way but
// "contained", because clean exits are not evidence of confinement.
func runCanaryStage(ctx context.Context, n needs) *failure {
	base, err := os.MkdirTemp("", "flynn-sandbox-canary-")
	if err != nil {
		return &failure{stage: stageCanary, raw: "create check directory: " + err.Error()}
	}
	defer func() { _ = os.RemoveAll(base) }()
	work, outside := filepath.Join(base, "work"), filepath.Join(base, "outside")
	for _, d := range []string{work, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return &failure{stage: stageCanary, raw: "create check directory: " + err.Error()}
		}
	}
	var sigsys bool
	l, err := checkLocal(work, n, &sigsys)
	if err != nil {
		return failureOf(stageCanary, err, "")
	}
	defer func() { _ = l.Close() }()
	return judgeCanaries(ctx, l, n, outside, &sigsys)
}

// judgeCanaries reports the first canary that is not as confinement requires.
func judgeCanaries(ctx context.Context, l *Local, n needs, outside string, sigsys *bool) *failure {
	if f := positiveCanary(ctx, l, sigsys); f != nil {
		return f
	}
	for what, line := range canaries(n, outside) {
		res, err := l.Exec(ctx, Command{Line: line})
		switch {
		case err != nil:
			return failureOf(stageCanary, err, res.Output)
		case res.ExitCode == canaryEscaped:
			return &failure{stage: stageCanary, escaped: true, raw: "a command could " + what}
		case res.ExitCode != canaryContained:
			return &failure{stage: stageCanary, raw: fmt.Sprintf("the canary %q ended inconclusively (exit %d)", what, res.ExitCode), output: res.Output, signal: *sigsys}
		}
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) > 0 {
		return &failure{stage: stageCanary, escaped: true, raw: "a file appeared outside the working directory"}
	}
	return nil
}

// positiveCanary writes in the working directory, so a sandbox that refuses
// everything does not pass.
func positiveCanary(ctx context.Context, l *Local, sigsys *bool) *failure {
	res, err := l.Exec(ctx, Command{Line: ": > ok && test -f ok"})
	if err != nil {
		return failureOf(stageCanary, err, res.Output)
	}
	if res.ExitCode != 0 {
		return &failure{stage: stageCanary, raw: fmt.Sprintf("the positive canary exited %d", res.ExitCode), output: res.Output, signal: *sigsys}
	}
	return nil
}

// failureOf keeps a start error's errno and a SIGSYS kill. Any other error
// classifies as unknown, which refuses.
func failureOf(s stage, err error, output string) *failure {
	if err == nil {
		return nil
	}
	f := &failure{stage: s, raw: err.Error(), output: output}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		f.errno = errno
	}
	f.signal = diedOfSIGSYS(err)
	return f
}

// diedOfSIGSYS reads the wait status, because Local.Exec reports a launcher killed
// by a seccomp filter (such as SystemCallFilter=) only as exit code -1.
func diedOfSIGSYS(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	ws, ok := exit.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGSYS
}

// withoutGate stops the check's own launches recursing into the gate. It is
// unexported so no outside caller can skip the gate.
func withoutGate() LocalOption { return func(l *Local) { l.selfTest = true } }

// hostNeeds maps governed egress to the network namespace only, because the ladder
// cannot exercise the proxy the launcher wires.
func (l *Local) hostNeeds() needs {
	return needs{
		net:     l.denyNetwork || l.egressActive(),
		mount:   l.readonlyFS,
		seccomp: l.seccomp,
	}
}

// gateTTL expires a pass, because an AppArmor reload (apparmor_parser -r), a sysctl
// or a unit edit changes the host under a running process.
const gateTTL = time.Minute

var (
	gateMu     sync.Mutex
	gatePassed = map[needs]time.Time{}
	// gateNow is replaced by a test to age a pass.
	gateNow = time.Now //nolint:forbidigo // a pass ages in wall-clock time; the gate is a live host probe, never replayed
	// gateOverride lets a test drive a refusal; it is nil outside tests. It is a
	// hook because an initialised variable would form a cycle: the check launches a
	// child that passes back through the gate.
	gateOverride func(context.Context, needs) Readiness
)

// requireConfinement returns nil only after a pass within [gateTTL], else a
// [*RefusedError]. Refusals are not cached, so a repaired host is seen at the
// next launch. A launch failing after a pass calls [Local.forgetConfinementPass].
func requireConfinement(n needs) error {
	gateMu.Lock()
	at, ok := gatePassed[n]
	gateMu.Unlock()
	if ok && gateNow().Sub(at) < gateTTL {
		return nil
	}
	run := checkHost
	if gateOverride != nil {
		run = gateOverride
	}
	r := run(context.Background(), n)
	if err := r.Err(); err != nil {
		forgetPass(n)
		return err
	}
	gateMu.Lock()
	gatePassed[n] = gateNow()
	gateMu.Unlock()
	return nil
}

// forgetPass makes the next confined launch for n run the check again.
func forgetPass(n needs) {
	gateMu.Lock()
	delete(gatePassed, n)
	gateMu.Unlock()
}

// forgetConfinementPass drops the pass for what this Local uses.
func (l *Local) forgetConfinementPass() { forgetPass(l.hostNeeds()) }

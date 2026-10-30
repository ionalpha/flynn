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

// checkTimeout bounds one pass of the ladder, so a wedged child cannot hang a launch.
// A timeout is a failure like any other: it refuses.
const checkTimeout = 15 * time.Second

// checkHost climbs the ladder for the given needs against this host, under
// [checkTimeout].
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

// runStage runs one rung of the ladder as a real confined no-op child.
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

// runNamespaceStage starts the shell's no-op in the namespaces one rung adds, using the
// same clone flags and identity mapping [Local.confine] sets, with an empty
// environment so nothing of the host's reaches it.
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
		sp.Cloneflags |= syscall.CLONE_NEWNS
	}
	c.SysProcAttr = sp
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	return failureOf(s, c.Run(), out.String())
}

// checkLocal builds the Local the launcher and canary rungs run through, configured for
// the needs under test and skipping the gate (the gate runs this check, so it cannot gate
// it). sigsys is set when a command it runs is killed by SIGSYS.
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

// runLauncherStage runs the no-op through the re-exec launcher with the read-only host
// view and the syscall filter the needs ask for. The launcher reports its failures as an
// exit code and a message rather than an error, so a non-zero exit is a failure carrying
// that message.
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

// A canary reports its own verdict through its exit status, so a command that failed for
// any other reason (a missing tool, an unreadable file) is told apart from one the
// confinement stopped.
const (
	canaryEscaped   = 0 // the command did what the confinement is there to prevent
	canaryContained = 1 // the confinement stopped it
)

// canaries are the commands, keyed by what they try, that would succeed on a host with no
// confinement and that the confined view must stop. outside is a directory the check owns
// and the confined view must keep read-only.
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

// runCanaryStage runs the production sandbox command twice over: a positive canary that
// must succeed, so a sandbox that stops every command is not mistaken for a confined one,
// and the negative canaries, each of which must be stopped. A negative canary that
// succeeds, or ends in any way but its own "contained" verdict, refuses the host:
// directives and clean exits are not evidence that the confinement is in force.
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

// judgeCanaries runs the positive canary and then each negative one through l, and
// reports the first that is not as the confinement requires.
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

// positiveCanary runs a command the confinement must let through: it writes in the
// working directory. A sandbox that refuses everything proves nothing about what it
// stops.
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

// failureOf turns a child's error into a failure, or nil for a clean run. It keeps the
// errno of a start error and notes a SIGSYS kill; every other error is kept as text and
// will classify as unknown, which refuses.
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

// diedOfSIGSYS reports whether err is a process that the kernel killed with SIGSYS, the
// signal a seccomp filter sends for a system call it forbids. The launcher is the process
// that makes the calls a unit's SystemCallFilter= can forbid, and Local.Exec reports its
// death only as exit code -1, so the signal is read from the wait status instead.
func diedOfSIGSYS(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	ws, ok := exit.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGSYS
}

// withoutGate marks a Local as the one running the confinement check itself, so its
// launches do not recurse into the gate. It is unexported: no caller outside this
// package can build a Local that skips the gate.
func withoutGate() LocalOption { return func(l *Local) { l.selfTest = true } }

// hostNeeds is the part of the ladder this Local's configuration depends on. Governed
// egress needs the network namespace and nothing the ladder can exercise beyond it (the
// loopback endpoint is built by the launcher, with a proxy this check does not start).
func (l *Local) hostNeeds() needs {
	return needs{
		net:     l.denyNetwork || l.egressActive(),
		mount:   l.readonlyFS,
		seccomp: l.seccomp,
	}
}

// gateTTL is how long a pass stands. A pass is a fact about the host at one moment: an
// AppArmor profile reloaded with apparmor_parser -r, a sysctl changed or a unit edited
// changes what the next launch meets without restarting this process, so the gate asks
// again rather than trusting the first answer for the life of the process.
const gateTTL = time.Minute

var (
	gateMu     sync.Mutex
	gatePassed = map[needs]time.Time{}
	// gateNow is the clock the pass lifetime reads; a test replaces it to age a pass.
	gateNow = time.Now //nolint:forbidigo // a pass ages in wall-clock time; the gate is a live host probe, never replayed
	// gateOverride replaces the check the gate runs, so a test can drive a refusal
	// without a host that refuses. It is nil outside tests, and nothing at run time sets
	// it. It is a hook rather than an initialised variable because the check launches a
	// confined child, which passes back through the gate.
	gateOverride func(context.Context, needs) Readiness
)

// requireConfinement is the gate in front of every confined launch. It returns nil only
// after a full pass of the ladder for these needs on this host within [gateTTL], and a
// [*RefusedError] otherwise. Refusals are not remembered: a host repaired while the
// process runs is picked up by the next launch, and a transient failure does not poison
// later ones. A launch that fails after a pass drops it through [Local.forgetConfinementPass],
// so the next launch runs the check again.
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

// forgetPass drops the remembered pass for these needs, so the next confined launch runs
// the readiness check again.
func forgetPass(n needs) {
	gateMu.Lock()
	delete(gatePassed, n)
	gateMu.Unlock()
}

// forgetConfinementPass drops the pass for what this Local uses.
func (l *Local) forgetConfinementPass() { forgetPass(l.hostNeeds()) }

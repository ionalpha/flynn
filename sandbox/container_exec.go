package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// This file is the concrete OCI backend for the container tier: it drives the host's docker
// or podman CLI to run the command buildContainerArgv composes, and adopts the running
// container as a Serving handle. The tier's security posture is already fixed by that pure
// builder and the spec validation; this backend only executes the command and observes the
// container's lifecycle, so it cannot weaken the boundary.
//
// The engine is driven through an injected Runner, so the argv composition, the
// container-id handling, and the lifecycle observation are all unit-testable without a
// container engine present. The real runner is a thin exec wrapper over the CLI. A live
// end-to-end check against a real engine is the build-tagged oci_integration leg.

// containerLoopback is the host address a published container port is bound to and the host
// the adopted Serving reports. It matches the bind in buildContainerArgv: a container server
// is reachable only over loopback, never off the host.
const containerLoopback = "127.0.0.1"

// engineDetectTimeout bounds the availability probe so a hung or misconfigured engine
// reports unavailable rather than blocking selection.
const engineDetectTimeout = 8 * time.Second

// engineStopTimeout bounds a teardown so an engine that ignores the stop cannot hang the
// caller forever.
const engineStopTimeout = 20 * time.Second

// engineLogsTimeout bounds a diagnostic log read.
const engineLogsTimeout = 5 * time.Second

// Runner runs an OCI engine command to completion and returns its stdout. argv[0] is the
// engine binary, the rest its arguments. It is injected so the driver's logic is testable
// with a scripted engine; the default runner execs the CLI.
type Runner func(ctx context.Context, argv []string) (stdout string, err error)

// execRunner runs the engine CLI as a host process, returning only its stdout so a value
// the caller parses (a container id) is never corrupted by a warning on stderr; on failure
// the stderr is folded into the error. The CLI is a trusted host command (it is how Flynn
// invokes the isolation, not untrusted code); the container it starts is the boundary,
// exactly as the microVM backend execs the host's VM runtime directly.
func execRunner(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fault.New(fault.Terminal, "container_no_argv", "container: empty engine command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv[0] is a fixed engine name (docker/podman); the container, not this exec, is the boundary
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fault.Wrap(fault.Terminal, "container_engine",
			fmt.Errorf("container: %s %s failed: %w: %s", argv[0], firstArg(argv), err, oneLine(stderr.String())))
	}
	return stdout.String(), nil
}

// ociDriver is a ContainerDriver backed by a docker- or podman-compatible CLI. It only
// executes the already-composed run command and observes the container, so it cannot relax
// the posture buildContainerArgv fixed.
type ociDriver struct {
	engine OCIEngine
	run    Runner
}

// NewContainerDriver builds a driver for engine. A nil runner uses the default CLI exec
// runner; a test supplies a scripted one.
func NewContainerDriver(engine OCIEngine, runner Runner) ContainerDriver {
	if runner == nil {
		runner = execRunner
	}
	return &ociDriver{engine: engine, run: runner}
}

// init registers the docker and podman backends in preference order, so the binary can run
// a container without the host wiring a driver by hand. Detection gates which (if either)
// is actually usable; an engine whose daemon is not running reports unavailable rather than
// being selected, the no-silent-downgrade rule.
func init() {
	RegisterContainerDriver(NewContainerDriver(EngineDocker, nil))
	RegisterContainerDriver(NewContainerDriver(EnginePodman, nil))
}

// Name identifies the engine for logs, errors, and the spine.
func (d *ociDriver) Name() string { return string(d.engine) }

// Detect reports whether this engine can run a container right now. `<engine> version` with
// a server-version template succeeds only when the CLI is installed and its daemon is
// reachable, so a CLI present with no running daemon reports unavailable rather than
// appearing usable.
func (d *ociDriver) Detect() Availability {
	ctx, cancel := context.WithTimeout(context.Background(), engineDetectTimeout)
	defer cancel()
	out, err := d.run(ctx, []string{string(d.engine), "version", "--format", "{{.Server.Version}}"})
	if err != nil {
		return Availability{OK: false, Detail: string(d.engine) + " is not usable: " + oneLine(err.Error())}
	}
	v := strings.TrimSpace(out)
	if v == "" {
		return Availability{OK: false, Detail: string(d.engine) + " reported no server version (is the daemon running?)"}
	}
	return Availability{OK: true, Detail: string(d.engine) + " server " + v}
}

// Run starts the container for spec and adopts it as a Serving. It runs the composed,
// detached command, takes the container id the engine prints, and returns a handle to the
// container's loopback endpoint. An error means no container was started. It assumes spec
// already passed validate (the tier entry points validate before calling a driver), so it
// only executes; it does not re-decide policy.
func (d *ociDriver) Run(ctx context.Context, spec ContainerSpec) (Serving, error) {
	argv := buildContainerArgv(d.engine, spec)
	out, err := d.run(ctx, argv)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(lastLine(out))
	if !plausibleContainerID(id) {
		return nil, fault.New(fault.Terminal, "container_no_id",
			"container: "+string(d.engine)+" did not return a container id")
	}
	addr := ""
	if spec.publishes() {
		addr = fmt.Sprintf("%s:%d", containerLoopback, spec.HostPort)
	}
	return newContainerServing(d.engine, id, addr, d.run), nil
}

// containerServing is the handle to a running container adopted as a server. It observes the
// container through the same engine CLI: a background wait that closes done when the
// container exits, an inspect-free Running derived from that, a logs tail for diagnostics,
// and a bounded stop for teardown.
type containerServing struct {
	engine OCIEngine
	id     string
	addr   string
	run    Runner
	done   chan struct{}
	stop   sync.Once
}

var _ Serving = (*containerServing)(nil)

// newContainerServing adopts a started container and begins reaping it.
func newContainerServing(engine OCIEngine, id, addr string, run Runner) *containerServing {
	s := &containerServing{engine: engine, id: id, addr: addr, run: run, done: make(chan struct{})}
	go s.reap()
	return s
}

// reap blocks on `engine wait <id>`, which returns when the container exits, and closes done
// so callers waiting on the lifecycle are released. It carries no deadline: a server
// container runs until it exits or is stopped. A `--rm` container is removed once wait
// returns, which is also the point past which logs and inspect no longer resolve.
func (s *containerServing) reap() {
	_, _ = s.run(context.Background(), []string{string(s.engine), "wait", s.id})
	close(s.done)
}

// Addr is the host loopback address the container's server answers on, or "" for a
// non-publishing container.
func (s *containerServing) Addr() string { return s.addr }

// Running reports whether the container has not yet exited, read from the reaper rather than
// a fresh inspect so a torn-down `--rm` container reads as stopped without an error.
func (s *containerServing) Running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Output returns the retained tail of the container's logs, for diagnostics. It is
// best-effort: once a `--rm` container is gone the logs no longer resolve and this is empty.
func (s *containerServing) Output() string {
	ctx, cancel := context.WithTimeout(context.Background(), engineLogsTimeout)
	defer cancel()
	out, err := s.run(ctx, []string{string(s.engine), "logs", "--tail", "200", s.id})
	if err != nil {
		return ""
	}
	return out
}

// Done is closed when the container exits.
func (s *containerServing) Done() <-chan struct{} { return s.done }

// Stop ends the container and waits, bounded, for the exit to be observed. It is idempotent
// and best-effort: a container that has already exited is a no-op, and the stop command's
// own error is not surfaced because the goal is teardown, not a report.
func (s *containerServing) Stop() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	s.stop.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), engineStopTimeout)
		defer cancel()
		_, _ = s.run(ctx, []string{string(s.engine), "stop", s.id})
	})
	select {
	case <-s.done:
	case <-time.After(engineStopTimeout):
	}
	return nil
}

// plausibleContainerID reports whether s looks like a container id the engine returns: a
// non-empty run of hex (the engine prints the full 64-character id, or a 12-character short
// id). It is a shape check so an empty or error line is not mistaken for a started container.
func plausibleContainerID(s string) bool {
	if len(s) < 12 {
		return false
	}
	return strings.TrimLeft(s, "0123456789abcdefABCDEF") == ""
}

// lastLine returns the last non-empty line of s, the line the container id is on (the engine
// may print a pull progress line before it).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// oneLine collapses whitespace runs to single spaces so a multi-line engine error reads as
// one diagnostic line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// firstArg returns the engine subcommand for an error message, or "" when there is none.
func firstArg(argv []string) string {
	if len(argv) > 1 {
		return argv[1]
	}
	return ""
}

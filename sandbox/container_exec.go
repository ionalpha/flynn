package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/procexec"
)

// The OCI backend only runs the argv buildContainerArgv composed, so it cannot
// weaken the posture. The oci_integration build tag runs it against a real engine.

// containerLoopback must match the port bind in buildContainerArgv.
const containerLoopback = "127.0.0.1"

// engineDetectTimeout makes a hung engine report unavailable instead of blocking
// selection.
const engineDetectTimeout = 8 * time.Second

// engineStopTimeout bounds teardown against an engine that ignores the stop.
const engineStopTimeout = 20 * time.Second

// engineLogsTimeout bounds a diagnostic log read.
const engineLogsTimeout = 5 * time.Second

// Runner runs an engine command and returns its stdout. Tests inject a scripted
// engine.
type Runner func(ctx context.Context, argv []string) (stdout string, err error)

// execRunner returns stdout alone so a stderr warning cannot corrupt a parsed
// container id; stderr goes into the error. The CLI is trusted; the container is
// the boundary.
func execRunner(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fault.New(fault.Terminal, "container_no_argv", "container: empty engine command")
	}
	cmd := procexec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv[0] is a fixed engine name (docker/podman); the container, not this exec, is the boundary
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// runCounted counts a child only after a confirmed start.
	err := runCounted(cmd)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fault.Wrap(fault.Terminal, "container_engine",
			fmt.Errorf("container: %s %s failed: %w: %s", argv[0], firstArg(argv), err, oneLine(stderr.String())))
	}
	return stdout.String(), nil
}

// ociDriver drives a docker- or podman-compatible CLI.
type ociDriver struct {
	engine OCIEngine
	run    Runner
}

// NewContainerDriver uses the CLI runner when runner is nil.
func NewContainerDriver(engine OCIEngine, runner Runner) ContainerDriver {
	if runner == nil {
		runner = execRunner
	}
	return &ociDriver{engine: engine, run: runner}
}

// init registers docker, then podman. Detection decides which is usable.
func init() {
	RegisterContainerDriver(NewContainerDriver(EngineDocker, nil))
	RegisterContainerDriver(NewContainerDriver(EnginePodman, nil))
}

func (d *ociDriver) Name() string { return string(d.engine) }

// Detect asks for the server version, which fails when the daemon is down even
// though the CLI is installed.
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

// Run runs the detached command and adopts the printed container id. It expects
// a validated spec. An error means nothing started.
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

// containerServing observes the container through the engine CLI: a background
// wait closes done on exit.
type containerServing struct {
	engine OCIEngine
	id     string
	addr   string
	run    Runner
	done   chan struct{}
	stop   sync.Once
	clk    clock.Timing
}

var _ Serving = (*containerServing)(nil)

func newContainerServing(engine OCIEngine, id, addr string, run Runner) *containerServing {
	s := &containerServing{engine: engine, id: id, addr: addr, run: run, done: make(chan struct{}), clk: clock.System{}}
	go s.reap()
	return s
}

// ContainerID lets a later invocation stop the container, which has no host pid.
func (s *containerServing) ContainerID() string { return s.id }

// EngineName tells a later process which engine to drive to stop it.
func (s *containerServing) EngineName() string { return string(s.engine) }

// reap has no deadline because a server runs until stopped. After `wait` returns
// a `--rm` container is gone, and logs and inspect fail.
func (s *containerServing) reap() {
	_, _ = s.run(context.Background(), []string{string(s.engine), "wait", s.id})
	close(s.done)
}

// Addr is "" for a non-publishing container.
func (s *containerServing) Addr() string { return s.addr }

// Running reads the reaper instead of inspect, so a removed `--rm` container
// reads as stopped without an error.
func (s *containerServing) Running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Output is empty once a `--rm` container is gone.
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

// Stop is idempotent and best effort; the stop command's error is dropped.
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
	case <-s.clk.After(engineStopTimeout):
	}
	return nil
}

// plausibleContainerID accepts non-empty hex (64 or 12 characters from the
// engine), so an error line is never taken for a started container.
func plausibleContainerID(s string) bool {
	if len(s) < 12 {
		return false
	}
	return strings.TrimLeft(s, "0123456789abcdefABCDEF") == ""
}

// lastLine skips pull progress the engine may print before the container id.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func firstArg(argv []string) string {
	if len(argv) > 1 {
		return argv[1]
	}
	return ""
}

// StopContainer stops a container that a record names and no handle holds, as in
// a later `models stop`.
func StopContainer(ctx context.Context, engine OCIEngine, id string) error {
	_, err := execRunner(ctx, []string{string(engine), "stop", id})
	return err
}

// PullImage pulls by digest, and the engine verifies the bytes against it, so a
// moved tag cannot substitute content.
func PullImage(ctx context.Context, engine OCIEngine, ref, digest string) error {
	target := digest
	if ref != "" {
		target = ref + "@" + digest
	}
	_, err := execRunner(ctx, []string{string(engine), "pull", target})
	return err
}

// EnsureContainerNetwork creates a named network if missing, so a container
// avoids the default bridge. The network does not block egress: an internal one
// would also break port forwarding, so the runtime runs offline and a proxy on
// this network governs what remains.
func EnsureContainerNetwork(ctx context.Context, engine OCIEngine, name string) error {
	if name == "" {
		return fault.New(fault.Terminal, "container_no_network_name", "container: a serving network needs a name")
	}
	// `network inspect` fails when the network does not exist.
	if _, err := execRunner(ctx, []string{string(engine), "network", "inspect", name}); err == nil {
		return nil
	}
	_, err := execRunner(ctx, []string{string(engine), "network", "create", name})
	return err
}

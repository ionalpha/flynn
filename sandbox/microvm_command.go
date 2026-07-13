package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/procs"
)

// commandMachine drives a host-provided microVM runtime over a small, product-neutral
// protocol, so Flynn stays a thin boundary over whatever hardware virtualization the host
// already trusts rather than embedding a hypervisor. Flynn hands the runtime a manifest
// (the guest image, the resource caps, the read-only mounts, the egress posture, and the
// command to run) and reads back the guest's result. The runtime is the host's to provide;
// the Flynn side, the manifest it builds and the result it reads, is fully implemented and
// tested here. File operations on the guest working area reuse a path-confined Local, since
// the working directory is a host directory the runtime mounts writable into the guest.
type commandMachine struct {
	runtime string // absolute path to the host's microVM runtime binary
	spec    Spec   // the boot request: image, guarantees, root
	files   *Local // path-confined file access to the guest working area (the host Root)
	control string // host directory for manifests and result files, outside the guest-visible Root

	mu      sync.Mutex
	serving []*cmdServing // background servers to tear down on Close
	closed  bool
}

// newCommandMachine builds a machine that drives runtime for spec. The working area
// (spec.Root) must exist; a private control directory is created beside it for the
// manifests and results the guest never sees.
func newCommandMachine(runtime string, spec Spec) (*commandMachine, error) {
	files, err := NewLocal(spec.Root)
	if err != nil {
		return nil, fmt.Errorf("microvm: working area: %w", err)
	}
	control, err := os.MkdirTemp("", "flynn-microvm-")
	if err != nil {
		return nil, fmt.Errorf("microvm: control dir: %w", err)
	}
	return &commandMachine{runtime: runtime, spec: spec, files: files, control: control}, nil
}

var _ Machine = (*commandMachine)(nil)

// manifest is the boot request Flynn hands the runtime, as JSON. It is product-neutral:
// it names the image, the caps, the mounts, the egress posture, and the command, and the
// runtime adapter translates it to its own hypervisor. Egress is always denied and every
// mount is read-only, set here regardless of the caller, so the manifest cannot carry a
// weakened posture even if a caller assembled the spec by hand.
//
// The manifest carries the guest-side posture; by accepting it the runtime also commits to
// the host-side half of the contract (see the Driver doc): run the monitor jailed and
// least-privilege, attach no network interface while Egress is false, give each guest a
// unique uid/gid, and bound the monitor process by the resource caps. Egress=false means
// no network device, not a blocked one, so an inference process has no interface to reach.
type manifest struct {
	Kernel   string            `json:"kernel"`
	RootFS   string            `json:"rootfs"`
	WorkDir  string            `json:"workdir"`
	VCPUs    int               `json:"vcpus"`
	MemMiB   int               `json:"mem_mib"`
	PIDs     int               `json:"pids,omitempty"`
	WallMS   int64             `json:"wall_ms,omitempty"`
	Egress   bool              `json:"egress"`
	Mounts   []manifestMount   `json:"mounts,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Command  []string          `json:"command,omitempty"`
	Serve    bool              `json:"serve"`
	ResultTo string            `json:"result_to,omitempty"`
}

// manifestMount is a host path the runtime mounts into the guest, always read-only.
type manifestMount struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	ReadOnly  bool   `json:"read_only"`
}

// buildManifest renders the boot request for a command (or a serve when argv drives a
// long-lived server). It is a pure function so the security-critical invariants are
// fuzz-testable in isolation: egress is forced denied, every mount is forced read-only,
// the command is carried as argv (never a shell string), and the resource caps come
// straight from the validated guarantees. It refuses a spec with no guest image, since a
// guest with no kernel or root filesystem cannot be a boundary.
func buildManifest(spec Spec, argv []string, serve bool, resultTo string) (manifest, error) {
	if !filepath.IsAbs(spec.Image.Kernel) || !filepath.IsAbs(spec.Image.RootFS) {
		return manifest{}, fault.New(fault.Terminal, "microvm_no_image",
			"microvm: a guest needs an absolute kernel and root filesystem path")
	}
	g := spec.Guarantees
	mounts := make([]manifestMount, 0, len(g.Mounts))
	for _, m := range g.Mounts {
		mounts = append(mounts, manifestMount{
			HostPath:  m.HostPath,
			GuestPath: m.GuestPath,
			ReadOnly:  true, // forced: an untrusted guest never gets a writable host mount
		})
	}
	return manifest{
		Kernel:   spec.Image.Kernel,
		RootFS:   spec.Image.RootFS,
		WorkDir:  spec.Root,
		VCPUs:    g.Limits.vcpus(),
		MemMiB:   g.Limits.MemMiB,
		PIDs:     g.Limits.PIDs,
		WallMS:   g.Limits.Wall.Milliseconds(),
		Egress:   false, // forced: the untrusted tier never opens outbound network
		Mounts:   mounts,
		Env:      g.Env,
		Command:  append([]string(nil), argv...),
		Serve:    serve,
		ResultTo: resultTo,
	}, nil
}

// result is the guest outcome the runtime writes back for a run-to-completion command.
type result struct {
	Exit   int    `json:"exit"`
	Output string `json:"output"`
}

// Exec boots a guest, runs the command to completion, reads back the result, and tears
// the guest down, the one-shot path. A non-zero guest exit is a result; an error means the
// runtime could not boot or run the guest at all.
func (c *commandMachine) Exec(ctx context.Context, line string) (ExecResult, error) {
	if c.isClosed() {
		return ExecResult{}, fault.New(fault.Terminal, "microvm_closed", "microvm: machine is closed")
	}
	// Each Exec gets its own result file so concurrent calls on the same machine
	// cannot overwrite each other's manifest or read back another call's result. The
	// files live in the private control directory and are removed when the call ends.
	resultFile, err := os.CreateTemp(c.control, "result-*.json")
	if err != nil {
		return ExecResult{}, fault.Wrap(fault.Terminal, "microvm_result", err)
	}
	resultPath := resultFile.Name()
	_ = resultFile.Close()
	defer func() { _ = os.Remove(resultPath) }() //nolint:gosec // resultPath is from os.CreateTemp under the private control dir, not caller-controlled
	man, err := buildManifest(c.spec, []string{"/bin/sh", "-c", line}, false, resultPath)
	if err != nil {
		return ExecResult{}, err
	}
	manPath, err := c.writeManifest("exec", man)
	if err != nil {
		return ExecResult{}, err
	}
	defer func() { _ = os.Remove(manPath) }()           //nolint:gosec // manPath is from os.CreateTemp under the private control dir, not caller-controlled
	cmd := exec.CommandContext(ctx, c.runtime, manPath) //nolint:gosec // runtime is a resolved, host-trusted path; the guest, not this exec, is the boundary
	// runCounted brackets the child-process registry around a confirmed start, so a
	// runtime that fails to launch is never counted as a live child.
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err = runCounted(cmd); err != nil {
		// A cancelled or timed-out context (the caller's deadline, or the wall-clock breaker)
		// surfaces as the context error so callers can tell "the guest was stopped" from "the
		// runtime is broken", rather than misreading a cancellation as a terminal failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ExecResult{}, ctxErr
		}
		return ExecResult{}, fault.Wrap(fault.Terminal, "microvm_run",
			fmt.Errorf("microvm: runtime failed to boot or run the guest: %w: %s", err, strings.TrimSpace(buf.String())))
	}
	data, err := os.ReadFile(resultPath) //nolint:gosec // resultPath is a Flynn-controlled path under the private control dir
	if err != nil {
		return ExecResult{}, fault.Wrap(fault.Terminal, "microvm_result",
			fmt.Errorf("microvm: runtime did not report a guest result: %w", err))
	}
	var r result
	if err := json.Unmarshal(data, &r); err != nil {
		return ExecResult{}, fault.Wrap(fault.Terminal, "microvm_result_decode",
			fmt.Errorf("microvm: malformed guest result: %w", err))
	}
	return ExecResult{Output: r.Output, ExitCode: r.Exit}, nil
}

// Serve boots a guest running a long-lived server and returns a handle once the runtime
// reports the forwarded host address. The guest still has no outbound network; only the
// single loopback endpoint the runtime forwards is reachable.
func (c *commandMachine) Serve(ctx context.Context, argv []string) (Serving, error) {
	if c.isClosed() {
		return nil, fault.New(fault.Terminal, "microvm_closed", "microvm: machine is closed")
	}
	man, err := buildManifest(c.spec, argv, true, "")
	if err != nil {
		return nil, err
	}
	manPath, err := c.writeManifest("serve", man)
	if err != nil {
		return nil, err
	}
	// The server outlives the call, so it is not tied to ctx; ctx only bounds the wait
	// for the forwarded address to appear.
	cmd := exec.Command(c.runtime, manPath) //nolint:gosec // runtime is a resolved, host-trusted path
	// The stdout pipe is created here rather than with cmd.StdoutPipe, because that pipe is
	// closed by cmd.Wait as soon as the runtime exits, which can cut the reader off before it
	// has drained the handshake. A runtime that prints its address and exits at once would then
	// have its address lost and be reported as a server that never came up. With an explicit
	// pipe the reader owns the read end: it drains everything the runtime wrote and sees EOF
	// when the last write end closes, whatever the wait does.
	stdout, pw, err := os.Pipe()
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "microvm_serve_pipe", err)
	}
	tail := newTailBuffer(tailBufferCap)
	cmd.Stdout = pw
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = stdout.Close()
		return nil, fault.Wrap(fault.Terminal, "microvm_serve_start", err)
	}
	// The child holds its own copy of the write end; this one must go, or the reader never
	// reaches EOF once the runtime exits.
	_ = pw.Close()
	s := &cmdServing{
		cmd: cmd, tail: tail, done: make(chan struct{}),
		scanned: make(chan struct{}), clk: clock.System{}, reaped: procs.Started(),
	}
	addrCh := make(chan string, 1)
	go s.readAddr(stdout, tail, addrCh)
	go s.reap()

	select {
	case <-ctx.Done():
		_ = s.Stop()
		return nil, ctx.Err()
	case <-s.done:
		// The runtime may have printed its address and exited in the same instant. Waiting for
		// the reader to finish draining stdout (it ends at EOF, once the exited runtime's write
		// end is gone) is what makes the outcome deterministic: an address that was printed is
		// in addrCh by then, so a server that did come up is never reported as one that did not.
		<-s.scanned
		select {
		case addr := <-addrCh:
			return c.adopt(s, addr)
		default:
			return nil, fault.New(fault.Terminal, "microvm_serve_exited",
				"microvm: the guest server exited before reporting its address:\n"+tail.String())
		}
	case addr := <-addrCh:
		return c.adopt(s, addr)
	}
}

// adopt records a started server's forwarded address and tracks it for teardown. If the
// machine was closed while the server was coming up, the server is stopped rather than
// leaked, so a Serve that races Close never leaves an orphan process behind.
func (c *commandMachine) adopt(s *cmdServing, addr string) (Serving, error) {
	s.setAddr(addr)
	if !c.track(s) {
		_ = s.Stop()
		return nil, fault.New(fault.Terminal, "microvm_closed", "microvm: machine is closed")
	}
	return s, nil
}

// addrPrefix is the line the runtime prints once the forwarded loopback endpoint is up.
const addrPrefix = "FLYNN-MICROVM-ADDR "

// ReadFile reads from the guest working area, which is the host working directory the
// runtime mounts into the guest, so the read is path-confined on the host.
func (c *commandMachine) ReadFile(ctx context.Context, p string) ([]byte, error) {
	return c.files.ReadFile(ctx, p)
}

// WriteFile writes into the guest working area (the host working directory).
func (c *commandMachine) WriteFile(ctx context.Context, p string, data []byte) error {
	return c.files.WriteFile(ctx, p, data)
}

// List returns regular-file paths under dir in the guest working area.
func (c *commandMachine) List(ctx context.Context, dir string) ([]string, error) {
	return c.files.Walk(ctx, dir)
}

// Close stops every background server and removes the private control directory.
func (c *commandMachine) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	servers := c.serving
	c.serving = nil
	c.mu.Unlock()
	for _, s := range servers {
		_ = s.Stop()
	}
	_ = c.files.Close()
	return os.RemoveAll(c.control) //nolint:gosec // c.control is a private temp dir this machine created and owns
}

func (c *commandMachine) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// track records a server for teardown, returning false if the machine has already closed
// (so the caller stops the server rather than leaking it past Close).
func (c *commandMachine) track(s *cmdServing) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.serving = append(c.serving, s)
	return true
}

// writeManifest serializes a manifest to a uniquely named file in the control
// directory and returns its path. The name is unique per call (os.CreateTemp), so
// concurrent Exec or Serve calls on the same machine never share a manifest path
// and cannot overwrite each other's boot request.
func (c *commandMachine) writeManifest(kind string, man manifest) (string, error) {
	data, err := json.Marshal(man)
	if err != nil {
		return "", fault.Wrap(fault.Terminal, "microvm_manifest", err)
	}
	f, err := os.CreateTemp(c.control, kind+"-manifest-*.json") //nolint:gosec // c.control is the private temp dir this machine owns
	if err != nil {
		return "", fault.Wrap(fault.Terminal, "microvm_manifest_write", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", fault.Wrap(fault.Terminal, "microvm_manifest_write", err)
	}
	if err := f.Close(); err != nil {
		return "", fault.Wrap(fault.Terminal, "microvm_manifest_write", err)
	}
	return f.Name(), nil
}

// cmdServing is the handle to a background guest server started by a commandMachine.
type cmdServing struct {
	cmd  *exec.Cmd
	tail *tailBuffer
	done chan struct{}
	// scanned is closed when the reader has drained the server's stdout, so a Serve that
	// observes the exit can still tell whether an address was printed before it.
	scanned chan struct{}
	clk     clock.Timing
	// reaped tells the process registry this child is no longer live. It runs from reap,
	// the one goroutine that waits on the command.
	reaped func()

	mu   sync.Mutex
	addr string
}

var _ Serving = (*cmdServing)(nil)

// readAddr drains the runtime's stdout, retaining every line in the tail and reporting the
// first forwarded address it announces. It owns the read end of the pipe: it runs to EOF (the
// exited runtime's write end closing) and closes it, so nothing else can cut it off mid-read.
func (s *cmdServing) readAddr(stdout io.ReadCloser, tail *tailBuffer, out chan<- string) {
	defer close(s.scanned)
	defer func() { _ = stdout.Close() }()
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		_, _ = tail.Write([]byte(line + "\n"))
		if strings.HasPrefix(line, addrPrefix) {
			select {
			case out <- strings.TrimSpace(strings.TrimPrefix(line, addrPrefix)):
			default:
			}
		}
	}
}

func (s *cmdServing) reap() {
	_ = s.cmd.Wait()
	s.reaped()
	close(s.done)
}

func (s *cmdServing) setAddr(a string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addr = a
}

func (s *cmdServing) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

func (s *cmdServing) Running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *cmdServing) Output() string { return s.tail.String() }

func (s *cmdServing) Done() <-chan struct{} { return s.done }

func (s *cmdServing) Stop() error {
	if !s.Running() {
		return nil
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	// Bound the wait so a runtime that ignores the kill cannot hang teardown forever.
	select {
	case <-s.done:
	case <-s.clk.After(5 * time.Second):
	}
	return nil
}

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
	"github.com/ionalpha/flynn/internal/closeerr"
	"github.com/ionalpha/flynn/internal/procexec"
	"github.com/ionalpha/flynn/procs"
)

// commandMachine hands a host runtime a JSON manifest and reads back the result,
// so Flynn embeds no hypervisor. File operations use a path-confined Local because
// the runtime mounts the host working directory into the guest.
type commandMachine struct {
	runtime string // absolute path to the host's microVM runtime binary
	spec    Spec   // the boot request: image, guarantees, root
	files   *Local // path-confined file access to the guest working area (the host Root)
	control string // host directory for manifests and result files, outside the guest-visible Root

	mu      sync.Mutex
	serving []*cmdServing // background servers to tear down on Close
	closed  bool
}

// newCommandMachine requires spec.Root and creates a control directory beside it
// that the guest never sees.
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

// manifest always denies egress and makes every mount read-only, whatever the
// caller built. A runtime that accepts it also runs the monitor jailed, gives
// each guest a unique uid/gid, caps the monitor, and attaches no network device
// while Egress is false.
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

type manifestMount struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	ReadOnly  bool   `json:"read_only"`
}

// buildManifest is pure so its invariants can be fuzzed. It refuses a spec with
// no guest image, since a guest without kernel or rootfs is no boundary.
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

type result struct {
	Exit   int    `json:"exit"`
	Output string `json:"output"`
}

// Exec returns a non-zero guest exit as a result; an error means the guest did
// not boot or run.
func (c *commandMachine) Exec(ctx context.Context, line string) (ExecResult, error) {
	if c.isClosed() {
		return ExecResult{}, fault.New(fault.Terminal, "microvm_closed", "microvm: machine is closed")
	}
	// A result file per call keeps concurrent calls from reading each other's.
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
	defer func() { _ = os.Remove(manPath) }()               //nolint:gosec // manPath is from os.CreateTemp under the private control dir, not caller-controlled
	cmd := procexec.CommandContext(ctx, c.runtime, manPath) //nolint:gosec // runtime is a resolved, host-trusted path; the guest, not this exec, is the boundary
	// runCounted counts a child only after a confirmed start.
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err = runCounted(cmd); err != nil {
		// The context error lets callers tell a stopped guest from a broken runtime.
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

// Serve returns once the runtime reports the forwarded address. The guest has no
// outbound network; only that loopback endpoint is reachable.
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
	// ctx bounds only the wait for the address; the server outlives the call.
	cmd := procexec.Command(c.runtime, manPath) //nolint:gosec // runtime is a resolved, host-trusted path
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "microvm_serve_pipe", err)
	}
	tail := newTailBuffer(tailBufferCap)
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, fault.Wrap(fault.Terminal, "microvm_serve_start", err)
	}
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
		// done closes after the reader hits EOF, so a printed address is already in
		// addrCh and a server that came up is never reported as failed.
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

// adopt stops the server if the machine closed meanwhile, so racing Close leaves
// no orphan.
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

func (c *commandMachine) ReadFile(ctx context.Context, p string) ([]byte, error) {
	return c.files.ReadFile(ctx, p)
}

func (c *commandMachine) WriteFile(ctx context.Context, p string, data []byte) error {
	return c.files.WriteFile(ctx, p, data)
}

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

// track returns false after Close, and the caller then stops the server.
func (c *commandMachine) track(s *cmdServing) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.serving = append(c.serving, s)
	return true
}

// writeManifest names the file with os.CreateTemp so concurrent calls never share
// a manifest.
func (c *commandMachine) writeManifest(kind string, man manifest) (path string, err error) {
	data, err := json.Marshal(man)
	if err != nil {
		return "", fault.Wrap(fault.Terminal, "microvm_manifest", err)
	}
	f, err := os.CreateTemp(c.control, kind+"-manifest-*.json") //nolint:gosec // c.control is the private temp dir this machine owns
	if err != nil {
		return "", fault.Wrap(fault.Terminal, "microvm_manifest_write", err)
	}
	defer func() {
		closeerr.Keep(&err, f)
		if err != nil {
			path, err = "", fault.Wrap(fault.Terminal, "microvm_manifest_write", err)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	return f.Name(), nil
}

type cmdServing struct {
	cmd  *exec.Cmd
	tail *tailBuffer
	done chan struct{}
	// scanned closes once stdout is drained, so Serve can tell whether an address
	// came before the exit.
	scanned chan struct{}
	clk     clock.Timing
	// reaped runs only from reap, the one goroutine that waits on the command.
	reaped func()

	mu   sync.Mutex
	addr string
}

var _ Serving = (*cmdServing)(nil)

// readAddr reads to EOF and closes scanned. reap waits for that before Wait,
// because Wait closes the pipe and would cut off a live read of the address.
func (s *cmdServing) readAddr(stdout io.Reader, tail *tailBuffer, out chan<- string) {
	defer close(s.scanned)
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
	// Wait closes stdout, so drain first or an address printed just before exit is
	// lost.
	<-s.scanned
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
	// A runtime that ignores the kill cannot hang teardown.
	select {
	case <-s.done:
	case <-s.clk.After(5 * time.Second):
	}
	return nil
}

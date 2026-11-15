package sandbox

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// OCIEngine is a container runtime for a workload Flynn does not ship as one
// binary, such as a GPU inference server. Both engines take the same run flags,
// so the choice changes only the binary.
type OCIEngine string

const (
	// EngineDocker is the Docker CLI.
	EngineDocker OCIEngine = "docker"
	// EnginePodman is the Podman CLI.
	EnginePodman OCIEngine = "podman"
)

// ContainerImage must be pinned to a content digest because a registry can move
// a tag.
type ContainerImage struct {
	// Ref is the reference without digest, e.g. "vllm/vllm-openai:v0.6.0".
	Ref string
	// Digest is "sha256:" and 64 hex characters. The run uses ref@digest.
	Digest string
}

// pinnedRef without a ref is the bare digest, which the engine resolves against
// an image already present.
func (i ContainerImage) pinnedRef() string {
	if i.Ref == "" {
		return i.Digest
	}
	return i.Ref + "@" + i.Digest
}

func (i ContainerImage) validate() error {
	d := i.Digest
	hex, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hex) != 64 || strings.TrimLeft(hex, "0123456789abcdefABCDEF") != "" {
		return fault.New(fault.Forbidden, "container_unpinned_image",
			"container: refusing an image that is not pinned to a sha256 digest")
	}
	return nil
}

// GPURequest grants the host GPU. The caller checks passthrough tooling against
// the hardware probe.
type GPURequest struct {
	// Enabled exposes the GPU to the container.
	Enabled bool
	// Device scopes access to ids such as "0" or "0,1". Empty exposes all GPUs.
	Device string
}

// flag returns the --gpus value, or "" for none.
func (g GPURequest) flag() string {
	switch {
	case !g.Enabled:
		return ""
	case g.Device == "":
		return "all"
	default:
		return "device=" + g.Device
	}
}

// ContainerSpec is a request to run a container under the microVM tier's
// [Guarantees].
type ContainerSpec struct {
	Image ContainerImage
	// Guarantees is validated as for a microVM, so an untrusted container cannot
	// run with open network, no memory cap or a writable mount.
	Guarantees Guarantees
	GPU        GPURequest
	// Network is the egress-controlled network a publishing container joins. It is
	// required with a port and must be empty without one, which means no network.
	Network string
	// HostPort and ContainerPort publish the container's server on a host loopback port.
	// ContainerPort 0 means the container publishes nothing and runs network-isolated.
	HostPort      int
	ContainerPort int
	// Command is argv, never a shell string, appended after the image so it
	// overrides the default command. Empty runs the image's entrypoint and command.
	Command []string
	// ExecScratch makes the in-memory /tmp executable. Off, the only writable area
	// cannot run code. A GPU inference server needs it on because it JIT-compiles
	// kernels into /tmp and fails to start under noexec.
	ExecScratch bool
}

func (s ContainerSpec) publishes() bool { return s.ContainerPort > 0 }

// validate adds container rules to the shared guarantee checks: a pinned image,
// a published port backed by a named network, and no network when nothing is
// published.
func (s ContainerSpec) validate() error {
	if err := s.Guarantees.validate(); err != nil {
		return err
	}
	if err := s.Image.validate(); err != nil {
		return err
	}
	if s.publishes() {
		if s.ContainerPort > 65535 || s.HostPort < 1 || s.HostPort > 65535 {
			return fault.New(fault.Terminal, "container_bad_port",
				"container: published ports must be in range")
		}
		if s.Network == "" {
			return fault.New(fault.Forbidden, "container_serve_no_network",
				"container: refusing to publish a port without a named egress-controlled network (an unnamed network would leave outbound open)")
		}
	} else if s.Network != "" {
		return fault.New(fault.Terminal, "container_idle_network",
			"container: a container that publishes no port must not name a network; it runs isolated")
	}
	return nil
}

// buildContainerArgv is pure so the posture is testable without a runtime: a
// read-only root, no capabilities or privilege gain, capped resources, only the
// spec's Env, read-only mounts, a loopback port and a pinned digest. It expects
// a spec that passed validate.
func buildContainerArgv(engine OCIEngine, spec ContainerSpec) []string {
	g := spec.Guarantees
	argv := []string{
		string(engine), "run", "--rm", "--detach",
		// The writable /tmp is in memory, so it never touches the host.
		"--read-only",
		"--tmpfs", tmpfsSpec(spec.ExecScratch),
		// A runtime parsing untrusted weights needs no capabilities or privilege gain.
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--memory", fmt.Sprintf("%dm", g.Limits.MemMiB),
		"--cpus", strconv.Itoa(g.Limits.vcpus()),
	}
	if g.Limits.PIDs > 0 {
		argv = append(argv, "--pids-limit", strconv.Itoa(g.Limits.PIDs))
	}
	// A container never joins the engine's open default bridge.
	if spec.publishes() {
		argv = append(argv, "--network", spec.Network)
	} else {
		argv = append(argv, "--network", "none")
	}
	// Mounts are read-only whatever the spec says.
	for _, m := range g.Mounts {
		argv = append(argv, "--mount",
			fmt.Sprintf("type=bind,source=%s,target=%s,readonly", m.HostPath, m.GuestPath))
	}
	// The host environment is never inherited.
	for _, k := range sortedKeys(g.Env) {
		argv = append(argv, "--env", k+"="+g.Env[k])
	}
	if f := spec.GPU.flag(); f != "" {
		argv = append(argv, "--gpus", f)
	}
	if spec.publishes() {
		// Loopback only, so the server is never exposed off the host.
		argv = append(argv, "--publish",
			fmt.Sprintf("127.0.0.1:%d:%d", spec.HostPort, spec.ContainerPort))
	}
	if len(spec.Command) > 0 {
		// Clearing the entrypoint stops the untrusted image prepending its own.
		argv = append(argv, "--entrypoint", "")
	}
	argv = append(argv, spec.Image.pinnedRef())
	argv = append(argv, spec.Command...)
	return argv
}

func tmpfsSpec(execScratch bool) string {
	if execScratch {
		return "/tmp:exec"
	}
	return "/tmp:noexec"
}

// sortedKeys keeps the composed command deterministic.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ContainerDriver runs the composed command on one engine and cannot change the
// posture. Implementations must be safe for concurrent use.
type ContainerDriver interface {
	// Name is "docker" or "podman", for logs, errors and the spine.
	Name() string
	// Detect reports whether the engine can run a container now, without running
	// one.
	Detect() Availability
	// Run returns a handle to the loopback endpoint. An error means nothing started.
	Run(ctx context.Context, spec ContainerSpec) (Serving, error)
}

// containerDrivers is in preference order.
var containerDrivers struct {
	drivers []ContainerDriver
}

// RegisterContainerDriver ignores nil. Call it at startup, because it is not safe
// concurrently with selection.
func RegisterContainerDriver(d ContainerDriver) {
	if d == nil {
		return
	}
	containerDrivers.drivers = append(containerDrivers.drivers, d)
}

// ContainerDrivers returns the engines in registration order.
func ContainerDrivers() []ContainerDriver {
	out := make([]ContainerDriver, len(containerDrivers.drivers))
	copy(out, containerDrivers.drivers)
	return out
}

// swapContainerDrivers returns a restore function so a fake engine does not leak
// between tests.
func swapContainerDrivers(ds ...ContainerDriver) func() {
	prev := containerDrivers.drivers
	containerDrivers.drivers = ds
	return func() { containerDrivers.drivers = prev }
}

// ErrNoContainerRuntime is Forbidden because the fix is to install a runtime;
// retrying does not help.
var ErrNoContainerRuntime = fault.New(fault.Forbidden, "container_unavailable",
	"container: no OCI runtime is available on this host")

// SelectContainerDriver returns the first available engine, or
// [ErrNoContainerRuntime] with every engine's diagnosis.
func SelectContainerDriver() (ContainerDriver, error) {
	var reasons []string
	for _, d := range ContainerDrivers() {
		av := d.Detect()
		if av.OK {
			return d, nil
		}
		reasons = append(reasons, fmt.Sprintf("%s: %s", d.Name(), av.Detail))
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "no OCI runtime is registered in this binary")
	}
	sort.Strings(reasons)
	return nil, fault.Wrap(fault.Forbidden, "container_select",
		fmt.Errorf("%w: %s", ErrNoContainerRuntime, strings.Join(reasons, "; ")))
}

// RunContainer refuses a weakening spec or a missing engine, so a started
// container is always contained.
func RunContainer(ctx context.Context, spec ContainerSpec) (Serving, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	d, err := SelectContainerDriver()
	if err != nil {
		return nil, err
	}
	return d.Run(ctx, spec)
}

// RunContainerWith runs on a given engine with the same validation as
// [RunContainer].
func RunContainerWith(ctx context.Context, d ContainerDriver, spec ContainerSpec) (Serving, error) {
	if d == nil {
		return nil, ErrNoContainerRuntime
	}
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if av := d.Detect(); !av.OK {
		return nil, fault.Wrap(fault.Forbidden, "container_unavailable_driver",
			fmt.Errorf("%w: %s: %s", ErrNoContainerRuntime, d.Name(), av.Detail))
	}
	return d.Run(ctx, spec)
}

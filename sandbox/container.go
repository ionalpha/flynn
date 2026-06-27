package sandbox

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// This file is the OCI-container tier: it runs a workload shipped as a container image
// under the same non-negotiable security composition the microVM tier enforces, reusing
// that tier's Limits, Mount, Guarantees, and Untrusted constructor. It is the boundary
// for a runtime Flynn does not ship as a single binary (a GPU inference server is the
// motivating case): the image is the unit, pinned by content digest, and the container
// is its isolation.
//
// As with the microVM tier, the load-bearing part is pure: buildContainerArgv composes
// the exact run command from a validated spec, forcing the posture (read-only root,
// dropped capabilities, no privilege escalation, capped memory/cpu/pids, secret-free
// environment, read-only host mounts, a loopback-only published port, and a
// digest-pinned image), and the spec validation refuses a request that would weaken it.
// A driver only has to run that command; the security invariants are decided here and
// are testable without a container runtime present.

// OCIEngine is a container runtime Flynn drives. Both accept the same run flags this tier
// composes, so the engine choice does not change the security posture, only the binary.
type OCIEngine string

const (
	// EngineDocker is the Docker CLI.
	EngineDocker OCIEngine = "docker"
	// EnginePodman is the Podman CLI, a drop-in for the run flags used here.
	EnginePodman OCIEngine = "podman"
)

// ContainerImage names the image to run and the content digest it must be pinned to. A
// tag alone is mutable (a registry can move it), so the tier refuses an unpinned image:
// the digest is the trust anchor, the same discipline the binary-runtime releases use.
type ContainerImage struct {
	// Ref is the image reference without the digest, e.g. "vllm/vllm-openai:v0.6.0". It is
	// recorded for diagnostics; the digest is what the run actually pins to.
	Ref string
	// Digest is the content digest the image is pinned to, "sha256:" followed by 64 hex
	// characters. The run uses ref@digest so a moved tag cannot substitute a different image.
	Digest string
}

// pinnedRef returns the digest-pinned reference the engine pulls and runs. When a ref is
// given it is "ref@digest"; with no ref the bare "digest" form is used, which the engine
// resolves against an already-present image.
func (i ContainerImage) pinnedRef() string {
	if i.Ref == "" {
		return i.Digest
	}
	return i.Ref + "@" + i.Digest
}

// validate refuses an image that is not pinned to a well-formed sha256 digest, so the
// tier never runs a tag a registry could have moved to a different image.
func (i ContainerImage) validate() error {
	d := i.Digest
	hex, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hex) != 64 || strings.TrimLeft(hex, "0123456789abcdefABCDEF") != "" {
		return fault.New(fault.Forbidden, "container_unpinned_image",
			"container: refusing an image that is not pinned to a sha256 digest")
	}
	return nil
}

// GPURequest grants a container access to the host GPU. It is off by default: a container
// sees no accelerator unless one is explicitly requested, and the host must have the
// passthrough tooling (checked by the caller against the hardware capability probe).
type GPURequest struct {
	// Enabled exposes the GPU to the container.
	Enabled bool
	// Device, when set, scopes access to specific device ids (e.g. "0" or "0,1"). Empty
	// with Enabled exposes all GPUs.
	Device string
}

// flag returns the --gpus value for the request, or "" when no GPU is requested. "all"
// exposes every device; a device list scopes to those ids.
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

// ContainerSpec is a request to run a container under the tier's guarantees: which image,
// the shared security composition, an optional GPU grant, the network it joins, and the
// loopback port its server is published on.
type ContainerSpec struct {
	// Image is the digest-pinned image to run.
	Image ContainerImage
	// Guarantees is the security composition (egress, caps, mounts, env) reused from the
	// microVM tier; it is validated the same way, so an untrusted container cannot run
	// with the network open, no memory cap, or a writable host mount.
	Guarantees Guarantees
	// GPU optionally grants the host accelerator.
	GPU GPURequest
	// Network is the name of the host-prepared, egress-controlled network a serving
	// container joins, so its only outbound path is the governed one. It is required when
	// a port is published (a published container cannot run network-isolated) and must be
	// empty otherwise, in which case the container runs with no network at all.
	Network string
	// HostPort and ContainerPort publish the container's server on a host loopback port.
	// ContainerPort 0 means the container publishes nothing and runs network-isolated.
	HostPort      int
	ContainerPort int
	// Command, when set, is the argv run inside the container, appended after the image so
	// it overrides the image's default command (the engine runs `run <opts> <image>
	// <command...>`). For a serving runtime it is the server invocation, for example the
	// vLLM serve arguments. It is argv, never a shell string, so nothing in it is
	// interpreted by a shell. Empty runs the image's own entrypoint and command.
	Command []string
}

// publishes reports whether the spec exposes a server port.
func (s ContainerSpec) publishes() bool { return s.ContainerPort > 0 }

// validate refuses a container spec that would weaken the untrusted posture, layering the
// container-specific rules on top of the shared guarantee validation: the image must be
// digest-pinned, a published port must be a usable loopback port backed by a named
// egress-controlled network, and a non-publishing container must name no network (it runs
// fully isolated). This is the refuse-rather-than-weaken rule for this tier.
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

// buildContainerArgv composes the engine run command for a validated spec. It is pure so
// the security-load-bearing invariants are testable in isolation, exactly as the microVM
// manifest builder is: the root filesystem is read-only with only an in-memory /tmp, all
// Linux capabilities are dropped, privilege escalation is blocked, memory/cpu/pids are
// capped, the host environment is never inherited (only the spec's secret-free Env is
// passed), every host mount is forced read-only, the published port is bound to loopback
// only, and the image is referenced by its pinned digest. A non-publishing container runs
// with no network; a publishing one joins the named egress-controlled network.
//
// It assumes the spec already passed validate, so it does not re-check policy; it only
// renders the command. The command is argv, never a shell string.
func buildContainerArgv(engine OCIEngine, spec ContainerSpec) []string {
	g := spec.Guarantees
	argv := []string{
		string(engine), "run", "--rm", "--detach",
		// A read-only root with a small in-memory /tmp: the container cannot persist a
		// change to its own image, and the writable area never touches the host.
		"--read-only",
		"--tmpfs", "/tmp",
		// Drop every capability and block gaining new privileges: a runtime parsing
		// untrusted weights has no need for either, and removing them shrinks the blast
		// radius of an exploit.
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--memory", fmt.Sprintf("%dm", g.Limits.MemMiB),
		"--cpus", strconv.Itoa(g.Limits.vcpus()),
	}
	if g.Limits.PIDs > 0 {
		argv = append(argv, "--pids-limit", strconv.Itoa(g.Limits.PIDs))
	}
	// Network: a published container joins the named egress-controlled network; an idle
	// one gets none. Either way it never lands on the engine's open default bridge.
	if spec.publishes() {
		argv = append(argv, "--network", spec.Network)
	} else {
		argv = append(argv, "--network", "none")
	}
	// Host mounts, every one read-only regardless of how the spec was assembled.
	for _, m := range g.Mounts {
		argv = append(argv, "--mount",
			fmt.Sprintf("type=bind,source=%s,target=%s,readonly", m.HostPath, m.GuestPath))
	}
	// Environment: only the explicit, secret-free set, in a stable order so the command is
	// deterministic. The host environment is never inherited.
	for _, k := range sortedKeys(g.Env) {
		argv = append(argv, "--env", k+"="+g.Env[k])
	}
	if f := spec.GPU.flag(); f != "" {
		argv = append(argv, "--gpus", f)
	}
	if spec.publishes() {
		// Bind the published port to loopback only, so the server is never exposed off the
		// host even though the container can answer it.
		argv = append(argv, "--publish",
			fmt.Sprintf("127.0.0.1:%d:%d", spec.HostPort, spec.ContainerPort))
	}
	argv = append(argv, spec.Image.pinnedRef())
	// The command to run inside the container, appended after the image so it overrides the
	// image default. It is argv, never a shell string.
	argv = append(argv, spec.Command...)
	return argv
}

// sortedKeys returns the map keys in sorted order, so an environment renders the same way
// every time and the composed command is deterministic.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ContainerDriver runs a container for a spec on a specific OCI engine. A driver only
// executes the composed command and adopts the running container as a Serving handle; the
// security posture is already fixed by buildContainerArgv and the spec validation, so a
// driver cannot weaken it. Implementations must be safe for concurrent use.
type ContainerDriver interface {
	// Name identifies the engine for logs, errors, and the spine ("docker", "podman").
	Name() string
	// Detect reports whether this engine can run a container on the host right now,
	// without running one, never silently downgrading.
	Detect() Availability
	// Run starts the container for spec and returns a handle to its loopback endpoint. An
	// error means no container was started.
	Run(ctx context.Context, spec ContainerSpec) (Serving, error)
}

// containerDrivers holds the OCI engines registered for this build, in preference order.
var containerDrivers struct {
	drivers []ContainerDriver
}

// RegisterContainerDriver adds an OCI engine to the registry in preference order. A nil
// driver is ignored. It is not safe to call concurrently with selection; drivers register
// at startup, as the microVM drivers do.
func RegisterContainerDriver(d ContainerDriver) {
	if d == nil {
		return
	}
	containerDrivers.drivers = append(containerDrivers.drivers, d)
}

// ContainerDrivers returns the registered OCI engines, in registration order.
func ContainerDrivers() []ContainerDriver {
	out := make([]ContainerDriver, len(containerDrivers.drivers))
	copy(out, containerDrivers.drivers)
	return out
}

// swapContainerDrivers replaces the registry and returns a restore function, so a test can
// install a fake engine without leaking it into another test.
func swapContainerDrivers(ds ...ContainerDriver) func() {
	prev := containerDrivers.drivers
	containerDrivers.drivers = ds
	return func() { containerDrivers.drivers = prev }
}

// ErrNoContainerRuntime is returned when no registered OCI engine can run a container on
// the host. It is Forbidden (a policy refusal), not transient: the fix is to install a
// container runtime, not to retry.
var ErrNoContainerRuntime = fault.New(fault.Forbidden, "container_unavailable",
	"container: no OCI runtime is available on this host")

// SelectContainerDriver returns the first registered engine that reports itself available,
// or ErrNoContainerRuntime with a combined diagnosis when none can. Picking an available
// engine and refusing when there is none is the no-silent-downgrade rule applied to the
// container tier.
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

// RunContainer validates the spec, selects an available OCI engine, and runs the
// container, the entry point a runtime uses. It refuses (never downgrades) when the spec
// would weaken the untrusted posture or when no engine is available, so a container that
// starts is always both contained and running on a real runtime.
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

// RunContainerWith runs a container on a specific engine, the path used once an engine is
// selected and the path a test uses with a fake engine. It applies the same validation as
// RunContainer, so neither route can start a weakened container.
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

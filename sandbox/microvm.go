package sandbox

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// This file is the hardware-virtualization tier: a guest with its own kernel, so a
// code-execution exploit inside the sandbox cannot reach the host kernel. It is the
// boundary for untrusted code, the worst case being a downloaded model parsed by a
// runtime with a history of remote-code-execution flaws. The tier reports
// ContainmentMicroVM, the only level the gate admits untrusted work at.
//
// The pure-Go core here is the whole tier minus the hypervisor: the composition
// guarantees every guest runs under (no egress, capped resources, no secrets,
// read-only weights), the driver port a platform's VMM implements, host-capability
// detection and selection, and the refuse-rather-than-downgrade gate. A backend
// (KVM, HVF, Hyper-V) implements only the Machine and Driver surface and inherits the
// full Sandbox port and the gate through MicroVM, exactly as a cloud backend inherits
// it through Remote. The core is fully testable with a fake Machine and a fake Driver,
// so the load-bearing invariant (untrusted code never boots with a weakened posture,
// and never on a host that cannot contain it) is locked without a hypervisor present.

// Limits caps a guest's resource use, the runaway and cost breaker enforced at the
// virtualization boundary rather than left to the guest. A guest with no memory bound
// is refused: an unbounded VM is a denial-of-service surface, not a contained one.
type Limits struct {
	// VCPUs is the number of virtual CPUs the guest may use; a value <= 0 means a
	// single vCPU, the safe floor.
	VCPUs int
	// MemMiB is the guest's RAM ceiling in MiB. It must be > 0: a microVM with no
	// memory cap is refused.
	MemMiB int
	// PIDs caps the number of processes in the guest, bounding a fork bomb; a value
	// <= 0 applies the driver's default cap rather than leaving it unbounded.
	PIDs int
	// Wall is a hard wall-clock lifetime for the guest, after which it is torn down
	// regardless of progress; 0 leaves the lifetime to the caller's context.
	Wall time.Duration
}

// vcpus returns the effective vCPU count, defaulting an unset value to a single CPU.
func (l Limits) vcpus() int {
	if l.VCPUs <= 0 {
		return 1
	}
	return l.VCPUs
}

// Mount is a host path exposed inside the guest. Untrusted weights are mounted
// read-only so the guest cannot tamper with them or use a writable mount to plant a
// persistent change on the host. A writable host mount into an untrusted guest is a
// hole in the boundary, so the tier refuses one (see Guarantees.validate).
type Mount struct {
	// HostPath is the absolute host path to expose. It must be absolute so the mount
	// cannot be reinterpreted relative to the guest's working directory.
	HostPath string
	// GuestPath is the absolute path the mount appears at inside the guest.
	GuestPath string
	// ReadOnly gives the guest a read-only view of HostPath. For an untrusted guest
	// every host mount must be read-only.
	ReadOnly bool
}

// Guarantees is the non-negotiable security composition every microVM workload runs
// under: the egress posture, the resource caps, the secret-free environment, and the
// read-only host mounts. The tier validates them before a guest boots and refuses a
// spec that would weaken the untrusted-code posture, so a caller cannot accidentally
// launch a guest with the network open, unbounded memory, or a writable host mount.
// This is the boundary's aggregation of every defense-in-depth layer in one place.
type Guarantees struct {
	// EgressDenied withholds all outbound network from the guest, so an inference
	// process cannot exfiltrate data or reach a command-and-control host. It must be
	// true for the untrusted posture; the network is denied at the virtualization
	// boundary, not merely inside the guest.
	EgressDenied bool
	// Limits caps the guest's CPU, memory, processes, and lifetime.
	Limits Limits
	// Mounts are the host paths exposed to the guest; for an untrusted guest each one
	// must be read-only.
	Mounts []Mount
	// Env is the explicit, secret-free set of variables the guest sees. The host
	// environment is never inherited, so a secret the agent holds cannot leak into the
	// guest; only what is named here is passed.
	Env map[string]string
}

// Untrusted builds the Guarantees for running untrusted code (a downloaded model's
// runtime, an unsigned plugin): egress denied, the given resource caps, and every host
// mount forced read-only. It is the safe constructor, so a caller does not assemble the
// untrusted posture by hand and risk leaving a field open.
func Untrusted(lim Limits, mounts ...Mount) Guarantees {
	ro := make([]Mount, len(mounts))
	for i, m := range mounts {
		m.ReadOnly = true
		ro[i] = m
	}
	return Guarantees{EgressDenied: true, Limits: lim, Mounts: ro}
}

// validate refuses a spec whose guarantees do not meet the untrusted-code posture, the
// refuse-rather-than-weaken rule applied to composition: a microVM is only worth its
// cost if it is actually airtight, so an unbounded, network-open, or writably-mounted
// guest is rejected before it boots rather than run as a false boundary.
func (g Guarantees) validate() error {
	if !g.EgressDenied {
		return fault.New(fault.Forbidden, "microvm_egress_open",
			"microvm: refusing to boot an untrusted guest with outbound network allowed")
	}
	if g.Limits.MemMiB <= 0 {
		return fault.New(fault.Forbidden, "microvm_no_mem_cap",
			"microvm: refusing to boot a guest with no memory cap (an unbounded VM is a denial-of-service surface)")
	}
	for _, m := range g.Mounts {
		if !filepath.IsAbs(m.HostPath) {
			return fault.Wrap(fault.Forbidden, "microvm_rel_mount",
				fmt.Errorf("microvm: host mount %q must be an absolute path", m.HostPath))
		}
		if !m.ReadOnly {
			return fault.Wrap(fault.Forbidden, "microvm_writable_mount",
				fmt.Errorf("microvm: refusing a writable host mount %q into an untrusted guest", m.HostPath))
		}
	}
	return nil
}

// Image names the guest's kernel and root filesystem, the bytes the VMM boots. Flynn
// does not ship a hypervisor or a guest image; a host provisions them and names them
// here, so the tier stays a thin, auditable boundary over whatever virtualization the
// host already trusts.
type Image struct {
	// Kernel is the absolute host path to the guest kernel to boot.
	Kernel string
	// RootFS is the absolute host path to the guest root filesystem image.
	RootFS string
}

// Spec is a request to boot a confined guest: where its working area lives on the host,
// which image to boot, and the guarantees it runs under.
type Spec struct {
	// Root is the host working directory the guest's output and staging area live in.
	// File operations through the Sandbox port are confined to it, the same default-deny
	// boundary the Local and Remote tiers enforce.
	Root string
	// Image is the guest kernel and root filesystem to boot.
	Image Image
	// Guarantees is the security composition the guest must run under.
	Guarantees Guarantees
}

// Availability reports whether a driver can run a microVM on this host, and why or why
// not in plain language for an operator. Detection never silently downgrades: a driver
// that cannot guarantee the hardware boundary reports OK=false with a reason, and the
// gate refuses the untrusted work rather than running it somewhere weaker.
type Availability struct {
	// OK is true when the driver can boot a guest with a real hardware boundary on this
	// host right now.
	OK bool
	// Detail explains the verdict: what was found, or what is missing (no KVM device,
	// no hypervisor support, no runtime configured), so an operator knows what to install.
	Detail string
}

// Machine is a running microVM a driver provides: the minimal primitives the tier needs
// to use a guest with its own kernel. It is the boundary a hardware-virtualization
// backend implements, so a backend writes only this surface and the guest is already
// booted confined to its Guarantees by the driver that created it. Implementations must
// be safe for concurrent use.
type Machine interface {
	// Exec runs a command to completion inside the guest and returns its result. A
	// non-zero exit is a result, not an error; an error means the command could not be
	// run at all.
	Exec(ctx context.Context, line string) (ExecResult, error)
	// Serve starts a long-lived process inside the guest and returns a handle. The
	// guest's loopback endpoint is forwarded to the host address the handle reports, so
	// a model server in the guest is reachable without giving the guest host network.
	Serve(ctx context.Context, argv []string) (Serving, error)
	// ReadFile reads a file from the guest working area.
	ReadFile(ctx context.Context, p string) ([]byte, error)
	// WriteFile writes a file into the guest working area, creating parents.
	WriteFile(ctx context.Context, p string, data []byte) error
	// List returns the regular-file paths under dir inside the guest, relative to its
	// root. It is the primitive Glob and Walk build on.
	List(ctx context.Context, dir string) ([]string, error)
	// Close shuts the guest down and releases the VM's resources.
	Close() error
}

// Serving is a handle to a long-lived server running inside a guest. It mirrors the
// background-process contract the serve layer consumes, but the address is a host
// loopback endpoint forwarded into the guest rather than a direct host port.
type Serving interface {
	// Addr is the host loopback address (host:port) the guest's server is reachable at.
	Addr() string
	// Running reports whether the server has not yet exited.
	Running() bool
	// Output returns the retained tail of the server's combined output, for diagnostics.
	Output() string
	// Done is closed when the server exits.
	Done() <-chan struct{}
	// Stop ends the server and tears down its guest. It is idempotent.
	Stop() error
}

// The microVM tier defends two trust boundaries, and a backend must hold both:
//
//   - Guest-to-VMM: the untrusted guest runs under its own kernel on hardware
//     virtualization, so a code-execution exploit inside it cannot reach the host kernel.
//     This is what Containment=microVM claims, and it is the guest's own boundary.
//   - VMM-to-host: the virtual-machine monitor process is itself host-side attack surface,
//     so a backend must run it as the jailed, least-privilege process it can be: dropped
//     privileges, a tailored syscall filter, its own resource cgroup or job object, a
//     unique uid/gid per guest, and a minimal device model that attaches no network
//     interface when egress is denied (the strongest posture is no device at all, not a
//     blocked one). A backend that boots the monitor unconfined would turn a guest escape
//     into a host compromise, so this obligation is part of the contract, not optional.
//
// Flynn does not implement the monitor; it drives the host's, and the contract above is
// the bar that monitor must clear. The reference command backend passes the guest-side
// posture (egress denied, read-only mounts, resource caps) in its manifest and relies on
// the configured runtime to hold the VMM-to-host boundary.

// Driver is a platform's hardware-virtualization backend: it detects whether the host
// can run a microVM and boots a confined guest. Each platform registers its driver in
// init under its build tag, so the core binary carries only the detection surface and a
// platform pulls in only its own VMM primitives.
type Driver interface {
	// Name identifies the backend for logs, errors, and the spine (for example "kvm",
	// "hvf", "hyperv").
	Name() string
	// Detect reports whether this backend can boot a guest with a real hardware boundary
	// on this host right now, without booting one.
	Detect() Availability
	// Boot launches a guest confined to spec.Guarantees and returns it. An error means no
	// guest was created.
	Boot(ctx context.Context, spec Spec) (Machine, error)
}

// driverRegistry holds the microVM backends registered for this build. Platform files
// register their driver in init; tests register a fake. It is guarded so registration
// and detection are safe across goroutines.
var driverRegistry struct {
	mu      sync.RWMutex
	drivers []Driver
}

// RegisterDriver adds a microVM backend to the registry. Platform adapters call it from
// init under their build tag; a test calls it to install a fake. A nil driver is ignored.
func RegisterDriver(d Driver) {
	if d == nil {
		return
	}
	driverRegistry.mu.Lock()
	defer driverRegistry.mu.Unlock()
	driverRegistry.drivers = append(driverRegistry.drivers, d)
}

// Drivers returns the registered microVM backends, in registration order.
func Drivers() []Driver {
	driverRegistry.mu.RLock()
	defer driverRegistry.mu.RUnlock()
	out := make([]Driver, len(driverRegistry.drivers))
	copy(out, driverRegistry.drivers)
	return out
}

// swapDrivers replaces the registry and returns a restore function, so a test can install
// fakes without leaking them into another test. It is unexported: only tests in this
// package use it.
func swapDrivers(ds ...Driver) func() {
	driverRegistry.mu.Lock()
	prev := driverRegistry.drivers
	driverRegistry.drivers = ds
	driverRegistry.mu.Unlock()
	return func() {
		driverRegistry.mu.Lock()
		driverRegistry.drivers = prev
		driverRegistry.mu.Unlock()
	}
}

// ErrNoMicroVM is returned when no registered backend can provide a hardware boundary on
// this host. It is Forbidden (a policy refusal), not transient: the answer is to install
// or enable virtualization, not to retry. Untrusted work is refused, never downgraded.
var ErrNoMicroVM = fault.New(fault.Forbidden, "microvm_unavailable",
	"microvm: no hardware-virtualization backend is available on this host")

// SelectDriver returns the first registered backend that reports itself available, plus
// its availability, or ErrNoMicroVM with a combined diagnosis when none can. Picking an
// available backend and refusing when there is none is the no-silent-downgrade rule for
// the untrusted tier: a guest boots only where the host can genuinely contain it.
func SelectDriver() (Driver, Availability, error) {
	var reasons []string
	for _, d := range Drivers() {
		av := d.Detect()
		if av.OK {
			return d, av, nil
		}
		reasons = append(reasons, fmt.Sprintf("%s: %s", d.Name(), av.Detail))
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "no microVM backend is built into this binary for the current platform")
	}
	sort.Strings(reasons)
	return nil, Availability{}, fault.Wrap(fault.Forbidden, "microvm_select",
		fmt.Errorf("%w: %s", ErrNoMicroVM, strings.Join(reasons, "; ")))
}

// MicroVM is the hardware-virtualization sandbox tier: it implements the Sandbox port
// over a Machine the host's VMM provides, so isolation runs in a guest with its own
// kernel. It reports ContainmentMicroVM, the level the gate requires for untrusted work,
// and confines every file operation to the guest root as defense-in-depth on top of the
// guest's own boundary, matching the Local and Remote tiers' default-deny rule.
type MicroVM struct {
	m      Machine
	driver string
	guard  Guarantees
}

var (
	_ Sandbox   = (*MicroVM)(nil)
	_ Contained = (*MicroVM)(nil)
)

// BootMicroVM selects an available backend, validates the spec's guarantees, boots a
// confined guest, and returns the tier wrapping it. It refuses (never downgrades) when no
// backend can provide a hardware boundary, or when the guarantees are weaker than the
// untrusted posture, so a guest that boots is always genuinely contained.
func BootMicroVM(ctx context.Context, spec Spec) (*MicroVM, error) {
	d, _, err := SelectDriver()
	if err != nil {
		return nil, err
	}
	return bootWith(ctx, d, spec)
}

// BootMicroVMWith boots a guest on a specific backend, the path the model runner uses
// once it has selected a driver, and the path a test uses with a fake driver. It applies
// the same guarantee validation as BootMicroVM, so neither route can launch a weakened
// guest.
func BootMicroVMWith(ctx context.Context, d Driver, spec Spec) (*MicroVM, error) {
	if d == nil {
		return nil, ErrNoMicroVM
	}
	if av := d.Detect(); !av.OK {
		return nil, fault.Wrap(fault.Forbidden, "microvm_unavailable_driver",
			fmt.Errorf("%w: %s: %s", ErrNoMicroVM, d.Name(), av.Detail))
	}
	return bootWith(ctx, d, spec)
}

// bootWith validates and boots a guest on driver d. It is the single place a guest is
// created, so the guarantee check cannot be bypassed by either entry point.
func bootWith(ctx context.Context, d Driver, spec Spec) (*MicroVM, error) {
	if err := spec.Guarantees.validate(); err != nil {
		return nil, err
	}
	m, err := d.Boot(ctx, spec)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "microvm_boot", err)
	}
	return &MicroVM{m: m, driver: d.Name(), guard: spec.Guarantees}, nil
}

// Driver names the backend this guest booted on, for the spine and diagnostics.
func (v *MicroVM) Driver() string { return v.driver }

// Containment reports microVM: a guest with its own kernel on hardware virtualization, a
// code-execution exploit inside cannot reach the host kernel. This is the only level the
// gate admits untrusted work at.
func (v *MicroVM) Containment() Containment { return ContainmentMicroVM }

// Exec runs a command inside the guest. The guest, not this method, is the boundary, so
// the command runs under the guest's own kernel, egress posture, and resource caps.
func (v *MicroVM) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	return v.m.Exec(ctx, cmd.Line)
}

// Serve starts a long-lived process inside the guest and returns its handle, the path a
// model server runs through. The server is reachable on the handle's host loopback
// address; the guest itself still has no outbound network.
func (v *MicroVM) Serve(ctx context.Context, argv []string) (Serving, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, fault.New(fault.Terminal, "microvm_serve_no_cmd", "microvm: serve: no command")
	}
	return v.m.Serve(ctx, argv)
}

// ReadFile reads a confined file from the guest working area.
func (v *MicroVM) ReadFile(ctx context.Context, p string) ([]byte, error) {
	c, err := confine(p)
	if err != nil {
		return nil, err
	}
	return v.m.ReadFile(ctx, c)
}

// WriteFile writes a confined file into the guest working area.
func (v *MicroVM) WriteFile(ctx context.Context, p string, data []byte) error {
	c, err := confine(p)
	if err != nil {
		return err
	}
	return v.m.WriteFile(ctx, c, data)
}

// Glob lists guest paths matching a shell pattern, relative to the root, matched with
// path.Match so behavior does not depend on a guest shell.
func (v *MicroVM) Glob(ctx context.Context, pattern string) ([]string, error) {
	entries, err := v.m.List(ctx, ".")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if ok, err := path.Match(pattern, e); err == nil && ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// Walk returns the regular-file paths under a confined root inside the guest.
func (v *MicroVM) Walk(ctx context.Context, root string) ([]string, error) {
	c, err := confine(root)
	if err != nil {
		return nil, err
	}
	return v.m.List(ctx, c)
}

// Close tears the guest down and releases the VM's resources.
func (v *MicroVM) Close() error { return v.m.Close() }

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

// This file is the hardware-virtualization tier: the guest has its own kernel,
// so an exploit inside cannot reach the host kernel. It reports
// ContainmentMicroVM, the only level the gate admits untrusted work at.
//
// Everything except the hypervisor is here, in pure Go. A backend (KVM, HVF,
// Hyper-V) implements Machine and Driver and gets the Sandbox port and the gate
// from MicroVM. Tests use a fake Machine and Driver, so the rule that untrusted
// code never boots weakened or on a host that cannot contain it is tested without
// a hypervisor.

// Limits are enforced by the VMM, outside the guest's control. A guest with no
// memory bound is refused.
type Limits struct {
	// VCPUs <= 0 means one vCPU.
	VCPUs int
	// MemMiB is the RAM ceiling in MiB and must be > 0.
	MemMiB int
	// PIDs caps guest processes; <= 0 applies the driver's default cap.
	PIDs int
	// Wall is the guest's hard lifetime; 0 leaves it to the caller's context.
	Wall time.Duration
}

func (l Limits) vcpus() int {
	if l.VCPUs <= 0 {
		return 1
	}
	return l.VCPUs
}

// Mount is a host path exposed in the guest. A writable mount into an untrusted
// guest is refused (see Guarantees.validate).
type Mount struct {
	// HostPath must be absolute so the guest cannot reinterpret it relative to its
	// working directory.
	HostPath  string
	GuestPath string
	// ReadOnly is required for every mount into an untrusted guest.
	ReadOnly bool
}

// Guarantees is validated before boot, and a spec with open network, unbounded
// memory or a writable mount is refused.
type Guarantees struct {
	// EgressDenied must be true. The VMM denies the network, so the guest cannot
	// lift it.
	EgressDenied bool
	Limits       Limits
	// Mounts must each be read-only for an untrusted guest.
	Mounts []Mount
	// Env is the guest's whole environment; nothing from the host is inherited.
	Env map[string]string
}

// Untrusted denies egress and forces every mount read-only, so a caller cannot
// leave a field open by assembling Guarantees by hand.
func Untrusted(lim Limits, mounts ...Mount) Guarantees {
	ro := make([]Mount, len(mounts))
	for i, m := range mounts {
		m.ReadOnly = true
		ro[i] = m
	}
	return Guarantees{EgressDenied: true, Limits: lim, Mounts: ro}
}

// validate rejects an unbounded, network-open or writably mounted guest before
// boot.
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

// Image names a host-provisioned kernel and root filesystem; Flynn ships neither
// a hypervisor nor an image.
type Image struct {
	Kernel string
	RootFS string
}

// Spec is a request to boot a confined guest.
type Spec struct {
	// Root is the host working directory; Sandbox file operations are confined to
	// it.
	Root       string
	Image      Image
	Guarantees Guarantees
}

// Availability reports whether a driver can run a microVM here. When OK is false
// the gate refuses untrusted work.
type Availability struct {
	OK bool
	// Detail tells an operator what is missing, such as a KVM device.
	Detail string
}

// Machine is a guest a Driver booted under its Guarantees. Implementations must
// be safe for concurrent use.
type Machine interface {
	// Exec returns a non-zero exit as a result; an error means it could not run.
	Exec(ctx context.Context, line string) (ExecResult, error)
	// Serve forwards the guest's loopback endpoint to the handle's host address,
	// so the guest needs no host network.
	Serve(ctx context.Context, argv []string) (Serving, error)
	ReadFile(ctx context.Context, p string) ([]byte, error)
	// WriteFile creates parents.
	WriteFile(ctx context.Context, p string, data []byte) error
	// List returns root-relative regular files under dir; Glob and Walk use it.
	List(ctx context.Context, dir string) ([]string, error)
	Close() error
}

// Serving is a server inside a guest, reached on a host loopback address
// forwarded into it.
type Serving interface {
	// Addr is the host loopback host:port.
	Addr() string
	Running() bool
	// Output returns the retained tail of combined output.
	Output() string
	Done() <-chan struct{}
	// Stop also tears down the guest. It is idempotent.
	Stop() error
}

// A backend must hold two boundaries:
//
//   - Guest to VMM: the guest's own kernel stops an exploit reaching the host
//     kernel. This is what ContainmentMicroVM claims.
//   - VMM to host: the monitor is host attack surface, so it runs with dropped
//     privileges, a syscall filter, its own cgroup or job object, a uid/gid per
//     guest, and no network device at all when egress is denied. An unconfined
//     monitor turns a guest escape into a host compromise.
//
// Flynn drives the host's monitor. The command backend passes the guest posture
// in its manifest and relies on the runtime for the VMM-to-host boundary.

// Driver is a platform's microVM backend, registered in init under its build tag
// so each binary carries only its own platform's code.
type Driver interface {
	// Name is for logs and the spine, such as "kvm", "hvf" or "hyperv".
	Name() string
	// Detect does not boot a guest.
	Detect() Availability
	// Boot confines the guest to spec.Guarantees. An error means no guest exists.
	Boot(ctx context.Context, spec Spec) (Machine, error)
}

// driverRegistry is locked because registration and detection can race.
var driverRegistry struct {
	mu      sync.RWMutex
	drivers []Driver
}

// RegisterDriver is called from a platform's init. A nil driver is ignored.
func RegisterDriver(d Driver) {
	if d == nil {
		return
	}
	driverRegistry.mu.Lock()
	defer driverRegistry.mu.Unlock()
	driverRegistry.drivers = append(driverRegistry.drivers, d)
}

// Drivers returns the backends in registration order.
func Drivers() []Driver {
	driverRegistry.mu.RLock()
	defer driverRegistry.mu.RUnlock()
	out := make([]Driver, len(driverRegistry.drivers))
	copy(out, driverRegistry.drivers)
	return out
}

// swapDrivers returns a restore function so a test's fakes do not leak.
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

// ErrNoMicroVM means no backend can run here. It is Forbidden because a retry
// cannot help; the host needs virtualization enabled.
var ErrNoMicroVM = fault.New(fault.Forbidden, "microvm_unavailable",
	"microvm: no hardware-virtualization backend is available on this host")

// SelectDriver returns the first available backend, or ErrNoMicroVM with every
// backend's diagnosis.
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

// MicroVM implements Sandbox over a Machine. It also confines file operations to
// the guest root, as a second check behind the guest kernel.
type MicroVM struct {
	m      Machine
	driver string
	guard  Guarantees
}

var (
	_ Sandbox   = (*MicroVM)(nil)
	_ Contained = (*MicroVM)(nil)
)

// BootMicroVM boots on SelectDriver's choice. It refuses when no backend is
// available or the guarantees are too weak.
func BootMicroVM(ctx context.Context, spec Spec) (*MicroVM, error) {
	d, _, err := SelectDriver()
	if err != nil {
		return nil, err
	}
	return bootWith(ctx, d, spec)
}

// BootMicroVMWith boots on d with the same validation as BootMicroVM.
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

// bootWith is the only place a guest is created, so neither entry point can skip
// validation.
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

// Driver names the backend this guest booted on.
func (v *MicroVM) Driver() string { return v.driver }

// Containment reports ContainmentMicroVM.
func (v *MicroVM) Containment() Containment { return ContainmentMicroVM }

// Exec runs under the guest's kernel, egress posture and caps.
func (v *MicroVM) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	return v.m.Exec(ctx, cmd.Line)
}

// Serve exposes the server on a host loopback address; the guest still has no
// outbound network.
func (v *MicroVM) Serve(ctx context.Context, argv []string) (Serving, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, fault.New(fault.Terminal, "microvm_serve_no_cmd", "microvm: serve: no command")
	}
	return v.m.Serve(ctx, argv)
}

// ReadFile reads a confined file from the guest.
func (v *MicroVM) ReadFile(ctx context.Context, p string) ([]byte, error) {
	c, err := confine(p)
	if err != nil {
		return nil, err
	}
	return v.m.ReadFile(ctx, c)
}

// WriteFile writes a confined file into the guest.
func (v *MicroVM) WriteFile(ctx context.Context, p string, data []byte) error {
	c, err := confine(p)
	if err != nil {
		return err
	}
	return v.m.WriteFile(ctx, c, data)
}

// Glob matches with path.Match so the result does not depend on a guest shell.
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

// Walk returns regular files under a confined root in the guest.
func (v *MicroVM) Walk(ctx context.Context, root string) ([]string, error) {
	c, err := confine(root)
	if err != nil {
		return nil, err
	}
	return v.m.List(ctx, c)
}

// Close tears the guest down.
func (v *MicroVM) Close() error { return v.m.Close() }

package sandbox

import (
	"fmt"

	"github.com/ionalpha/flynn/fault"
)

// Containment is how strongly a sandbox tier isolates the work inside it, on an
// ordered scale: a higher level contains strictly more. The load-bearing axis is the
// last one a level adds, the kernel/hardware exploit boundary, because the worst
// case for untrusted code is arbitrary execution that escapes a shared kernel.
//
// The level is what lets the run gate decide, before anything executes, whether a
// tier is strong enough for the work's trust level, and refuse rather than run
// untrusted code somewhere it could break out.
type Containment int

const (
	// ContainmentNone is a process jail: the work is confined to a directory and runs
	// without the agent's environment, but shares the host kernel, network, and
	// syscalls. Suitable only for trusted code (the agent's own tools).
	ContainmentNone Containment = iota
	// ContainmentKernel adds kernel-enforced confinement of the filesystem, syscalls,
	// and namespaces (Landlock/seccomp, Seatbelt, AppContainer). Suitable for
	// semi-trusted, model-authored code; still a shared kernel.
	ContainmentKernel
	// ContainmentContainer adds a container's namespacing and resource control, but
	// still over a shared host kernel, so it is not a boundary for untrusted code.
	ContainmentContainer
	// ContainmentUserKernel re-implements syscalls in user space off the host kernel
	// (a user-space-kernel sandbox), a strong boundary with some compatibility cost.
	ContainmentUserKernel
	// ContainmentMicroVM gives the work its own kernel on hardware virtualization, so
	// a kernel exploit inside cannot reach the host. The boundary for untrusted code.
	ContainmentMicroVM
	// ContainmentRemote runs the work off this host entirely, the strongest
	// blast-radius separation.
	ContainmentRemote
)

// String names the level for logs and errors.
func (c Containment) String() string {
	switch c {
	case ContainmentNone:
		return "process-jail"
	case ContainmentKernel:
		return "kernel-confined"
	case ContainmentContainer:
		return "container"
	case ContainmentUserKernel:
		return "userspace-kernel"
	case ContainmentMicroVM:
		return "microvm"
	case ContainmentRemote:
		return "remote"
	default:
		return fmt.Sprintf("containment(%d)", int(c))
	}
}

// Trust is how far the code about to run is trusted, which sets the containment it
// requires.
type Trust int

const (
	// TrustTrusted is the agent's own built-in tools: code we ship and vet.
	TrustTrusted Trust = iota
	// TrustSemi is code the model authored this run (a shell command, a script):
	// not hostile by construction, but not vetted either.
	TrustSemi
	// TrustUntrusted is code from outside that we cannot vouch for: a downloaded
	// model's weights parsed by a runtime with a history of remote-code-execution
	// flaws, or an unsigned plugin. The worst case is host compromise.
	TrustUntrusted
)

// String names the trust level for logs, errors, and provenance.
func (t Trust) String() string {
	switch t {
	case TrustUntrusted:
		return "untrusted"
	case TrustSemi:
		return "semi-trusted"
	default:
		return "trusted"
	}
}

// Required is the minimum containment a trust level may run under. Untrusted code
// requires a hardware boundary by default, the strict posture: a kernel exploit in,
// say, a model parser must not be able to reach the host. A policy may later relax
// untrusted to a user-space-kernel tier explicitly, but the safe default does not.
func Required(t Trust) Containment {
	switch t {
	case TrustUntrusted:
		return ContainmentMicroVM
	case TrustSemi:
		return ContainmentKernel
	default: // TrustTrusted
		return ContainmentNone
	}
}

// Contained is the optional capability a sandbox implements to report how strongly
// it isolates work. A sandbox that does not implement it is treated as the weakest
// level, so an unknown tier is never assumed to contain more than it proves.
type Contained interface {
	Containment() Containment
}

// ContainmentOf reports a sandbox's containment level, defaulting to the weakest when
// the sandbox does not declare one. Defaulting down is the safe direction: an
// undeclared tier can never satisfy a requirement it has not earned.
func ContainmentOf(sb Sandbox) Containment {
	if c, ok := sb.(Contained); ok {
		return c.Containment()
	}
	return ContainmentNone
}

// ErrInsufficientContainment is returned when a tier is too weak for the work's
// trust level. It is Forbidden (a policy refusal), not transient: the answer is a
// stronger tier, not a retry.
var ErrInsufficientContainment = fault.New(fault.Forbidden, "containment_insufficient",
	"sandbox: the available isolation cannot contain this work; refusing")

// Admit reports whether sb may run work of the given trust level, the gate that keeps
// untrusted code out of a tier that cannot contain it. It returns nil when the
// sandbox's containment meets or exceeds the requirement, and a Forbidden error
// naming the gap otherwise.
func Admit(sb Sandbox, t Trust) error {
	have, need := ContainmentOf(sb), Required(t)
	if have < need {
		return fault.Wrap(fault.Forbidden, "containment_gate",
			fmt.Errorf("%w: %s work needs %s isolation but the sandbox provides only %s",
				ErrInsufficientContainment, trustName(t), need, have))
	}
	return nil
}

// Select returns the strongest candidate sandbox that may run work of the given trust
// level, or a Forbidden error when none is strong enough. Choosing the strongest
// available tier, and refusing when there is none, is the no-silent-downgrade rule:
// the system runs untrusted work only where it is genuinely contained, or not at all.
func Select(t Trust, candidates ...Sandbox) (Sandbox, error) {
	need := Required(t)
	var best Sandbox
	bestLevel := Containment(-1)
	for _, sb := range candidates {
		if sb == nil {
			continue
		}
		if lvl := ContainmentOf(sb); lvl >= need && lvl > bestLevel {
			best, bestLevel = sb, lvl
		}
	}
	if best == nil {
		return nil, fault.Wrap(fault.Forbidden, "containment_select",
			fmt.Errorf("%w: no available isolation tier meets %s for %s work",
				ErrInsufficientContainment, need, trustName(t)))
	}
	return best, nil
}

// trustName labels a trust level for messages.
func trustName(t Trust) string { return t.String() }

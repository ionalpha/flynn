package sandbox

import (
	"fmt"

	"github.com/ionalpha/flynn/fault"
)

// Containment orders sandbox tiers so a higher level contains strictly more. The
// run gate compares it with the work's trust before anything executes. The kernel
// boundary decides the scale, because the worst case is code escaping a shared
// kernel.
//
//flynn:policy
type Containment int

const (
	// ContainmentNone confines the work to a directory and strips the agent's
	// environment, sharing kernel, network and syscalls. Trusted code only.
	ContainmentNone Containment = iota
	// ContainmentKernel adds Landlock/seccomp, Seatbelt or AppContainer. It suits
	// model-authored code; the kernel is still shared.
	ContainmentKernel
	// ContainmentContainer shares the host kernel, so it does not contain
	// untrusted code.
	ContainmentContainer
	// ContainmentUserKernel implements syscalls in user space, at some
	// compatibility cost.
	ContainmentUserKernel
	// ContainmentMicroVM gives the work its own kernel, so a kernel exploit cannot
	// reach the host. Untrusted code needs it.
	ContainmentMicroVM
	// ContainmentRemote runs the work on another host.
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

// Trust sets the containment code requires, through [Required].
//
//flynn:policy
type Trust int

const (
	// TrustTrusted is the agent's own built-in tools.
	TrustTrusted Trust = iota
	// TrustSemi is code the model wrote this run, such as a shell command.
	TrustSemi
	// TrustUntrusted is outside code, such as model weights for a parser with a
	// history of RCE flaws, or an unsigned plugin.
	TrustUntrusted
)

// String names the trust level for logs, errors, and provenance.
func (t Trust) String() string {
	switch t {
	case TrustUntrusted:
		return "untrusted"
	case TrustSemi:
		return "semi-trusted"
	case TrustTrusted:
		return "trusted"
	default:
		return fmt.Sprintf("trust(%d)", int(t))
	}
}

// Required is the minimum containment for t; untrusted code needs a microVM. The
// zero Trust is [TrustTrusted], because an action that declares none is the
// agent's own tool. An undefined level is treated as untrusted, so a corrupt or
// newer value cannot run anywhere.
func Required(t Trust) Containment {
	switch t {
	case TrustTrusted:
		return ContainmentNone
	case TrustSemi:
		return ContainmentKernel
	default: // TrustUntrusted, and any level this binary does not define
		return ContainmentMicroVM
	}
}

// Contained is optional; a sandbox without it counts as [ContainmentNone].
type Contained interface {
	Containment() Containment
}

// ContainmentOf returns [ContainmentNone] for an undeclared or out-of-range level,
// because a level above [ContainmentRemote] would satisfy every requirement.
func ContainmentOf(sb Sandbox) Containment {
	if c, ok := sb.(Contained); ok {
		if lvl := c.Containment(); lvl >= ContainmentNone && lvl <= ContainmentRemote {
			return lvl
		}
	}
	return ContainmentNone
}

// ErrInsufficientContainment is Forbidden: the remedy is a stronger tier, and a
// retry cannot help.
var ErrInsufficientContainment = fault.New(fault.Forbidden, "containment_insufficient",
	"sandbox: the available isolation cannot contain this work; refusing")

// Admit returns a Forbidden error naming the gap when sb is too weak for t.
func Admit(sb Sandbox, t Trust) error {
	have, need := ContainmentOf(sb), Required(t)
	if have < need {
		return fault.Wrap(fault.Forbidden, "containment_gate",
			fmt.Errorf("%w: %s work needs %s isolation but the sandbox provides only %s",
				ErrInsufficientContainment, trustName(t), need, have))
	}
	return nil
}

// Select returns the strongest admissible candidate, or a Forbidden error, so work
// never runs in a weaker tier than it needs.
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

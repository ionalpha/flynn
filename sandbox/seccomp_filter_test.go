//go:build linux

package sandbox

import (
	"testing"

	"golang.org/x/sys/unix"
)

// evalSeccomp interprets the BPF program buildSeccompFilter produces, returning the
// action the kernel would take for a syscall number under a given architecture. It
// understands exactly the instruction subset the filter uses (load a word from the
// fixed offsets of seccomp_data, compare-and-jump on equality, and return), and
// panics on anything else so a future change to the filter that introduces an
// uninterpreted instruction fails loudly here rather than passing silently.
func evalSeccomp(prog []unix.SockFilter, nr int, arch uint32) uint32 {
	dataAt := func(off uint32) uint32 {
		switch off {
		case 0:
			return uint32(nr) //nolint:gosec // test interpreter mirrors the kernel's 32-bit load of the syscall number
		case 4:
			return arch
		default:
			return 0
		}
	}
	var a uint32
	for pc := 0; pc < len(prog); {
		ins := prog[pc]
		switch ins.Code & 0x07 {
		case unix.BPF_LD:
			a = dataAt(ins.K)
			pc++
		case unix.BPF_JMP:
			if a == ins.K {
				pc += int(ins.Jt) + 1
			} else {
				pc += int(ins.Jf) + 1
			}
		case unix.BPF_RET:
			return ins.K
		default:
			panic("seccomp filter contains an instruction the test interpreter does not model")
		}
	}
	panic("seccomp filter fell through without returning")
}

// deniedSet is the denied syscalls as a set, the source of truth the interpreted
// filter is checked against.
func deniedSet() map[int]bool {
	m := make(map[int]bool, len(deniedSyscalls))
	for _, nr := range deniedSyscalls {
		m[nr] = true
	}
	return m
}

// TestSeccompFilterClassifies checks the filter end to end on representative inputs:
// every denied syscall is refused with a permission error, ordinary syscalls are
// allowed, and a foreign architecture is killed outright regardless of the number.
func TestSeccompFilterClassifies(t *testing.T) {
	prog := buildSeccompFilter()
	deny := retErrno | uint32(unix.EPERM)

	for _, nr := range deniedSyscalls {
		if got := evalSeccomp(prog, nr, unix.AUDIT_ARCH_X86_64); got != deny {
			t.Errorf("syscall %d must be denied, got %#x", nr, got)
		}
	}

	// A spread of syscalls an ordinary command relies on; none may be filtered.
	for _, nr := range []int{
		unix.SYS_READ, unix.SYS_WRITE, unix.SYS_OPENAT, unix.SYS_CLOSE,
		unix.SYS_MMAP, unix.SYS_EXECVE, unix.SYS_CLONE, unix.SYS_EXIT_GROUP,
		unix.SYS_FORK, unix.SYS_WAIT4, unix.SYS_FSTAT, unix.SYS_BRK,
	} {
		if got := evalSeccomp(prog, nr, unix.AUDIT_ARCH_X86_64); got != retAllow {
			t.Errorf("ordinary syscall %d must be allowed, got %#x", nr, got)
		}
	}

	// A foreign architecture is killed whatever the syscall number, since the numbers
	// would mean something else there.
	for _, nr := range []int{unix.SYS_READ, unix.SYS_PTRACE, 0} {
		if got := evalSeccomp(prog, nr, unix.AUDIT_ARCH_AARCH64); got != retKillProcess {
			t.Errorf("a foreign architecture must be killed (nr %d), got %#x", nr, got)
		}
	}
}

// FuzzSeccompClassifies proves the property across every syscall number: for the
// native architecture, the filter denies a number exactly when it is in the denied
// set and allows it otherwise, with no number slipping through either way.
func FuzzSeccompClassifies(f *testing.F) {
	for _, nr := range []int{0, 1, unix.SYS_PTRACE, unix.SYS_READ, unix.SYS_BPF, 1000, 462} {
		f.Add(nr)
	}
	prog := buildSeccompFilter()
	denied := deniedSet()
	deny := retErrno | uint32(unix.EPERM)
	f.Fuzz(func(t *testing.T, nr int) {
		if nr < 0 {
			return // real syscall numbers are non-negative
		}
		var want uint32 = retAllow
		if denied[nr] {
			want = deny
		}
		if got := evalSeccomp(prog, nr, unix.AUDIT_ARCH_X86_64); got != want {
			t.Fatalf("syscall %d classified as %#x, want %#x", nr, got, want)
		}
	})
}

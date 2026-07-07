//go:build linux

package sandbox

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seccomp return actions. A denied syscall returns an error (EPERM) rather than
// killing the process, so a command that probes a forbidden call simply fails that
// call and carries on, which is both safer to reason about and less surprising than
// a sudden kill. A wrong architecture is killed outright, since the syscall numbers
// checked below only have meaning for the architecture the filter was built for.
const (
	retAllow       = 0x7fff0000
	retErrno       = 0x00050000
	retKillProcess = 0x80000000
)

// The architecture and its denied-syscall set are defined per architecture
// (seccomp_linux_amd64.go, seccomp_linux_arm64.go), because a syscall number means
// different things on different architectures and some calls do not exist at all off
// x86. An architecture with no filter of its own refuses confinement rather than
// running a command unfiltered (seccomp_linux_other.go).

// installFilter installs a syscall filter on the calling thread (and the command it
// will exec into) that refuses the given syscalls, checking the given audit
// architecture first. It sets the no-new-privileges bit, which both lets an
// unprivileged process install a filter and prevents a setuid program from regaining
// what the filter takes away. The filter is a classic BPF program: confirm the
// architecture, load the syscall number, and refuse it if it appears in the denied
// set, otherwise allow it.
func installFilter(auditArch uint32, denied []int) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privs: %w", err)
	}
	prog := buildSeccompFilter(auditArch, denied)
	fprog := &unix.SockFprog{
		//nolint:gosec // the program length is fixed by the denied-syscall list, a few dozen entries, far below uint16
		Len:    uint16(len(prog)),
		Filter: &prog[0],
	}
	//nolint:gosec // prctl takes the filter program by address; fprog and its backing slice are live across the call
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(fprog)), 0, 0); err != nil {
		return fmt.Errorf("install seccomp filter: %w", err)
	}
	return nil
}

// buildSeccompFilter assembles the BPF program installed by installFilter. The
// syscall number and architecture live at fixed offsets in the kernel's seccomp_data
// argument: the number at offset 0 and the architecture at offset 4.
func buildSeccompFilter(auditArch uint32, denied []int) []unix.SockFilter {
	const (
		offNr   = 0
		offArch = 4
	)
	filter := []unix.SockFilter{
		// Refuse to run under an unexpected architecture, where these syscall numbers
		// would mean something else entirely.
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, offArch),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, auditArch, 1, 0),
		bpfStmt(unix.BPF_RET|unix.BPF_K, retKillProcess),
		// Load the syscall number for the comparisons that follow.
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, offNr),
	}
	for _, nr := range denied {
		// If the number matches, fall through to the refusal; otherwise skip it.
		filter = append(
			filter,
			//nolint:gosec // syscall numbers are small non-negative constants, well within uint32
			bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, uint32(nr), 0, 1),
			bpfStmt(unix.BPF_RET|unix.BPF_K, retErrno|uint32(unix.EPERM)),
		)
	}
	// Anything not denied above is allowed.
	filter = append(filter, bpfStmt(unix.BPF_RET|unix.BPF_K, retAllow))
	return filter
}

// bpfStmt builds a non-branching BPF instruction.
func bpfStmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

// bpfJump builds a conditional BPF instruction: jt is the instruction offset taken
// when the comparison is true, jf when it is false.
func bpfJump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

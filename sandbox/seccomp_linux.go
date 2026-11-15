//go:build linux

package sandbox

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seccomp return actions. A denied syscall gets EPERM, so a command probing it
// carries on. A wrong architecture is killed, because the numbers checked mean
// something else there.
const (
	retAllow       = 0x7fff0000
	retErrno       = 0x00050000
	retKillProcess = 0x80000000
)

// The denied set is per architecture because syscall numbers differ and some calls
// exist only on x86. An architecture without a filter refuses confinement.

// installFilter applies to the calling thread and what it execs. no-new-privileges
// lets an unprivileged process install the filter and stops a setuid program
// regaining what it removes.
func installFilter(auditArch uint32, denied []int) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("%s: %w", msgNoNewPrivs, err)
	}
	prog := buildSeccompFilter(auditArch, denied)
	fprog := &unix.SockFprog{
		//nolint:gosec // the program length is fixed by the denied-syscall list, a few dozen entries, far below uint16
		Len:    uint16(len(prog)),
		Filter: &prog[0],
	}
	//nolint:gosec // prctl takes the filter program by address; fprog and its backing slice are live across the call
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(fprog)), 0, 0); err != nil {
		return fmt.Errorf("%s: %w", msgInstallFilter, err)
	}
	return nil
}

// buildSeccompFilter reads seccomp_data: the syscall number at offset 0, the
// architecture at offset 4.
func buildSeccompFilter(auditArch uint32, denied []int) []unix.SockFilter {
	const (
		offNr   = 0
		offArch = 4
	)
	filter := []unix.SockFilter{
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, offArch),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, auditArch, 1, 0),
		bpfStmt(unix.BPF_RET|unix.BPF_K, retKillProcess),
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, offNr),
	}
	for _, nr := range denied {
		filter = append(
			filter,
			//nolint:gosec // syscall numbers are small non-negative constants, well within uint32
			bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, uint32(nr), 0, 1),
			bpfStmt(unix.BPF_RET|unix.BPF_K, retErrno|uint32(unix.EPERM)),
		)
	}
	filter = append(filter, bpfStmt(unix.BPF_RET|unix.BPF_K, retAllow))
	return filter
}

// bpfStmt builds a non-branching BPF instruction.
func bpfStmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

// bpfJump takes jt instructions forward when the comparison is true, jf when false.
func bpfJump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

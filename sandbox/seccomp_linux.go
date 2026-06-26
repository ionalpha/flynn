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
// below only have meaning for the architecture they were built for.
const (
	retAllow       = 0x7fff0000
	retErrno       = 0x00050000
	retKillProcess = 0x80000000
)

// deniedSyscalls is the set of syscalls a confined command may not make. It targets
// the calls that let a process escalate privilege, escape its confinement, tamper
// with the kernel, or reach into other processes, none of which a command working in
// its own directory has any honest need for. Ordinary file, process, memory, and IO
// calls are left allowed, so normal commands run unaffected.
var deniedSyscalls = []int{
	// Reaching into or controlling other processes.
	unix.SYS_PTRACE,
	unix.SYS_PROCESS_VM_READV,
	unix.SYS_PROCESS_VM_WRITEV,
	// Changing the mount table or the process's namespaces (confinement escape).
	unix.SYS_MOUNT,
	unix.SYS_UMOUNT2,
	unix.SYS_PIVOT_ROOT,
	unix.SYS_CHROOT,
	unix.SYS_UNSHARE,
	unix.SYS_SETNS,
	// Resolving a file by kernel handle sidesteps the directory the command is
	// confined to, so both halves of that interface are refused.
	unix.SYS_OPEN_BY_HANDLE_AT,
	unix.SYS_NAME_TO_HANDLE_AT,
	// Loading or unloading kernel code, or booting a new kernel.
	unix.SYS_INIT_MODULE,
	unix.SYS_FINIT_MODULE,
	unix.SYS_DELETE_MODULE,
	unix.SYS_KEXEC_LOAD,
	unix.SYS_KEXEC_FILE_LOAD,
	unix.SYS_CREATE_MODULE,
	unix.SYS_GET_KERNEL_SYMS,
	unix.SYS_QUERY_MODULE,
	// Loading kernel programs, performance counters, and the in-kernel keyring,
	// each a known privilege-escalation surface.
	unix.SYS_BPF,
	unix.SYS_PERF_EVENT_OPEN,
	unix.SYS_KEYCTL,
	unix.SYS_ADD_KEY,
	unix.SYS_REQUEST_KEY,
	// Fault handling and segment descriptors used as exploitation primitives.
	unix.SYS_USERFAULTFD,
	unix.SYS_MODIFY_LDT,
	// Direct port and IO-privilege access on x86.
	unix.SYS_IOPL,
	unix.SYS_IOPERM,
	// Changing global machine state: time, hostname, swap, accounting, the kernel
	// log, rebooting, and the old filesystem export and library-loading calls.
	unix.SYS_SETTIMEOFDAY,
	unix.SYS_CLOCK_SETTIME,
	unix.SYS_CLOCK_ADJTIME,
	unix.SYS_ADJTIMEX,
	unix.SYS_SETHOSTNAME,
	unix.SYS_SETDOMAINNAME,
	unix.SYS_SWAPON,
	unix.SYS_SWAPOFF,
	unix.SYS_ACCT,
	unix.SYS_SYSLOG,
	unix.SYS_REBOOT,
	unix.SYS_NFSSERVCTL,
	unix.SYS_USELIB,
}

// installSeccomp installs a syscall filter on the calling thread (and the command it
// will exec into) that refuses the syscalls in deniedSyscalls. It first sets the
// no-new-privileges bit, which both lets an unprivileged process install a filter and
// prevents a setuid program from regaining what the filter takes away. The filter is
// a classic BPF program: confirm the architecture, load the syscall number, and
// refuse it if it appears in the denied set, otherwise allow it.
func installSeccomp() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privs: %w", err)
	}
	prog := buildSeccompFilter()
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

// buildSeccompFilter assembles the BPF program installed by installSeccomp. The
// syscall number and architecture live at fixed offsets in the kernel's seccomp_data
// argument: the number at offset 0 and the architecture at offset 4.
func buildSeccompFilter() []unix.SockFilter {
	const (
		offNr   = 0
		offArch = 4
	)
	filter := []unix.SockFilter{
		// Refuse to run under an unexpected architecture, where these syscall numbers
		// would mean something else entirely.
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, offArch),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, unix.AUDIT_ARCH_X86_64, 1, 0),
		bpfStmt(unix.BPF_RET|unix.BPF_K, retKillProcess),
		// Load the syscall number for the comparisons that follow.
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, offNr),
	}
	for _, nr := range deniedSyscalls {
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

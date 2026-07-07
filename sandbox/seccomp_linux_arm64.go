//go:build linux && arm64

package sandbox

import "golang.org/x/sys/unix"

// seccompAuditArch is the audit-architecture token the filter checks against, so a
// program built for another architecture (where these numbers mean other calls) is
// killed rather than mis-filtered.
const seccompAuditArch = uint32(unix.AUDIT_ARCH_AARCH64)

// installSeccomp installs the AArch64 syscall filter for a confined command.
func installSeccomp() error { return installFilter(seccompAuditArch, deniedSyscalls) }

// deniedSyscalls is the set of syscalls a confined command may not make on AArch64. It
// mirrors the x86-64 set for every call that exists on this architecture. The obsolete
// x86-only calls (create_module, get_kernel_syms, query_module, modify_ldt, iopl,
// ioperm, uselib) and nfsservctl are absent from the AArch64 (generic) syscall table,
// so there is no number to deny and they are simply omitted; a process cannot invoke a
// call the architecture does not define.
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
	// Loading kernel programs, performance counters, and the in-kernel keyring,
	// each a known privilege-escalation surface.
	unix.SYS_BPF,
	unix.SYS_PERF_EVENT_OPEN,
	unix.SYS_KEYCTL,
	unix.SYS_ADD_KEY,
	unix.SYS_REQUEST_KEY,
	// Fault handling used as an exploitation primitive.
	unix.SYS_USERFAULTFD,
	// Changing global machine state: time, hostname, swap, accounting, the kernel
	// log, and rebooting.
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
}

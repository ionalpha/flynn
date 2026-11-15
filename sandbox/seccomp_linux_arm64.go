//go:build linux && arm64

package sandbox

import "golang.org/x/sys/unix"

// seccompAuditArch kills a program of another architecture, where these numbers
// mean other calls.
const seccompAuditArch = uint32(unix.AUDIT_ARCH_AARCH64)

// installSeccomp installs the AArch64 filter.
func installSeccomp() error { return installFilter(seccompAuditArch, deniedSyscalls) }

// deniedSyscalls mirrors the x86-64 set. create_module, get_kernel_syms,
// query_module, modify_ldt, iopl, ioperm, uselib and nfsservctl are absent from the
// AArch64 table, so there is nothing to deny.
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
	// A file handle sidesteps the confined directory.
	unix.SYS_OPEN_BY_HANDLE_AT,
	unix.SYS_NAME_TO_HANDLE_AT,
	// Loading or unloading kernel code, or booting a new kernel.
	unix.SYS_INIT_MODULE,
	unix.SYS_FINIT_MODULE,
	unix.SYS_DELETE_MODULE,
	unix.SYS_KEXEC_LOAD,
	unix.SYS_KEXEC_FILE_LOAD,
	// Kernel programs, performance counters and the keyring: escalation routes.
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

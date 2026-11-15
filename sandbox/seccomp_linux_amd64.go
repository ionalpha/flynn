//go:build linux && amd64

package sandbox

import "golang.org/x/sys/unix"

// seccompAuditArch kills a program of another architecture, where these numbers
// mean other calls.
const seccompAuditArch = uint32(unix.AUDIT_ARCH_X86_64)

// installSeccomp installs the x86-64 filter.
func installSeccomp() error { return installFilter(seccompAuditArch, deniedSyscalls) }

// deniedSyscalls covers escalation, escape, kernel tampering and reaching other
// processes. Ordinary file, process, memory and IO calls stay allowed.
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
	unix.SYS_CREATE_MODULE,
	unix.SYS_GET_KERNEL_SYMS,
	unix.SYS_QUERY_MODULE,
	// Kernel programs, performance counters and the keyring: escalation routes.
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

//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/ionalpha/flynn/procs"
)

// This file holds the low-level Windows calls that run a command inside an
// AppContainer: deriving the container identity, granting it the working directory,
// and launching the process under the container with combined-output capture. The
// policy that decides which confinement to apply lives in confine_windows.go.

// procThreadAttributeSecurityCapabilities (PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES)
// tags a process with the AppContainer identity and capabilities at creation time, so
// the kernel builds the container's token and object namespace before the command
// runs. It is not exported by the syscall bindings, so it is defined here.
const procThreadAttributeSecurityCapabilities = 0x00020009

// errAlreadyExists is HRESULT_FROM_WIN32(ERROR_ALREADY_EXISTS): the AppContainer
// profile for this moniker already exists, in which case the SID is derived instead.
const errAlreadyExists = 0x800700B7

// procThreadAttributeMitigationPolicy (PROC_THREAD_ATTRIBUTE_MITIGATION_POLICY) applies
// a set of process-mitigation policies to the child at creation time. It is not
// exported by the syscall bindings, so it is defined here.
const procThreadAttributeMitigationPolicy = 0x00020007

// The process-mitigation policy bits applied to every confined command, hardening the
// child beyond the AppContainer boundary. They deny code-injection and DLL-planting
// avenues and enable the standard exploit mitigations. Policies that would break
// ordinary developer commands are deliberately excluded: prohibit-dynamic-code (breaks
// just-in-time compilers), block-non-Microsoft-binaries (breaks ordinary third-party
// tools), strict-handle-checks (terminates a process on a double-close some tools do
// benignly), and the Win32k system-call disable (see below).
const (
	mitigationDEPEnable       = 0x01
	mitigationSEHOPEnable     = 0x04
	mitigationBottomUpASLR    = 0x01 << 16
	mitigationHighEntropyASLR = 0x01 << 20
	// mitigationWin32kSystemCallDisable removes the kernel's window-manager syscall
	// surface from the child. It is NOT applied, and must not be: user32.dll cannot
	// initialize without those syscalls, so any command that loads it dies during DLL
	// initialization with STATUS_DLL_INIT_FAILED before reaching main. That is most of
	// them (node, git, python, powershell, and the external agent CLIs among them), and
	// the failure surfaces only as an opaque 0xC0000142 exit code. The lockdown suits a
	// purpose-built child compiled to avoid user32; it cannot be imposed on the arbitrary
	// third-party commands this sandbox exists to run. It stays defined so the exclusion
	// is explicit and testable rather than a silent omission.
	mitigationWin32kSystemCallDisable = 0x01 << 28
	mitigationExtensionPointDisable   = 0x01 << 32
	mitigationImageLoadNoRemote       = 0x01 << 52
	mitigationImageLoadNoLowLabel     = 0x01 << 56
	mitigationImageLoadPreferSystem32 = 0x01 << 60
)

const sandboxMitigationPolicy = mitigationDEPEnable |
	mitigationSEHOPEnable |
	mitigationBottomUpASLR |
	mitigationHighEntropyASLR |
	mitigationExtensionPointDisable |
	mitigationImageLoadNoRemote |
	mitigationImageLoadNoLowLabel |
	mitigationImageLoadPreferSystem32

// securityCapabilities mirrors the Win32 SECURITY_CAPABILITIES structure, which is not
// exported by the syscall bindings. It carries the container's package SID and the
// capability SIDs granted to it.
type securityCapabilities struct {
	AppContainerSid *windows.SID
	Capabilities    *windows.SIDAndAttributes
	CapabilityCount uint32
	Reserved        uint32
}

var (
	userenv                           = windows.NewLazySystemDLL("userenv.dll")
	procCreateAppContainerProfile     = userenv.NewProc("CreateAppContainerProfile")
	procDeleteAppContainerProfile     = userenv.NewProc("DeleteAppContainerProfile")
	procDeriveAppContainerSidFromName = userenv.NewProc("DeriveAppContainerSidFromAppContainerName")

	kernelbase                       = windows.NewLazySystemDLL("kernelbase.dll")
	procDeriveCapabilitySidsFromName = kernelbase.NewProc("DeriveCapabilitySidsFromName")
)

// liveProfiles counts the AppContainer profiles this process has registered and not yet
// deleted. Registering one creates an operating-system object that outlives the process,
// so a sandbox that is never closed leaks it. The counter makes that leak observable from
// inside the process, which a count of the profile directory cannot be: helper child
// processes and concurrently running test binaries register profiles of their own there.
var liveProfiles atomic.Int64

// LiveProfileCount reports how many AppContainer profiles this process has registered and
// not yet deleted. It is zero for a process whose sandboxes were all closed, and it is the
// only leak signal that is not confounded by other processes on the machine. Off Windows
// no profile is ever registered and it is always zero.
func LiveProfileCount() int { return int(liveProfiles.Load()) }

// createOrDeriveACSID registers the AppContainer profile for a moniker and returns its
// package SID. Registering the profile is what creates the container's object
// namespace, without which the launch cannot set up the container. The call is
// idempotent: if the profile already exists the SID is derived from the moniker
// instead. The returned SID is freed by the caller with windows.FreeSid.
func createOrDeriveACSID(moniker string) (*windows.SID, error) {
	m, err := windows.UTF16PtrFromString(moniker)
	if err != nil {
		return nil, err
	}
	disp, _ := windows.UTF16PtrFromString("Flynn sandbox")
	desc, _ := windows.UTF16PtrFromString("Flynn sandboxed command")
	var sid *windows.SID
	r, _, _ := procCreateAppContainerProfile.Call(
		uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(disp)), uintptr(unsafe.Pointer(desc)),
		0, 0, uintptr(unsafe.Pointer(&sid)),
	)
	if r == 0 {
		liveProfiles.Add(1)
		return sid, nil
	}
	if uint32(r) == errAlreadyExists {
		return deriveACSID(moniker)
	}
	return nil, fmt.Errorf("CreateAppContainerProfile: hresult=0x%x", uint32(r))
}

// deriveACSID derives the package SID for an existing AppContainer moniker. The
// returned SID is freed by the caller with windows.FreeSid.
func deriveACSID(moniker string) (*windows.SID, error) {
	m, err := windows.UTF16PtrFromString(moniker)
	if err != nil {
		return nil, err
	}
	var sid *windows.SID
	r, _, _ := procDeriveAppContainerSidFromName.Call(uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(&sid)))
	if r != 0 {
		return nil, fmt.Errorf("DeriveAppContainerSidFromAppContainerName: hresult=0x%x", uint32(r))
	}
	return sid, nil
}

// deleteAppContainerProfile removes the registered AppContainer profile for a moniker
// and its on-disk folder. Deleting a profile that was never created succeeds, so an error
// here means a profile is still registered: its folder stays on disk and, because the
// moniker is derived from the workspace path, its SID stays re-derivable and any access
// entry granted to it stays live rather than becoming a dead one. The caller reports the
// failure rather than dropping it.
func deleteAppContainerProfile(moniker string) error {
	m, err := windows.UTF16PtrFromString(moniker)
	if err != nil {
		return fmt.Errorf("DeleteAppContainerProfile %q: %w", moniker, err)
	}
	if r, _, _ := procDeleteAppContainerProfile.Call(uintptr(unsafe.Pointer(m))); r != 0 {
		return fmt.Errorf("DeleteAppContainerProfile %q: hresult=0x%x", moniker, uint32(r))
	}
	// Never below zero: deleting a profile this process did not register (an earlier run's,
	// swept by the janitor) succeeds too, and must not make the counter lie.
	if liveProfiles.Load() > 0 {
		liveProfiles.Add(-1)
	}
	return nil
}

// capabilitySID returns the capability SID for a well-known capability name (for
// example internetClient, which re-grants outbound network access). The returned SID
// is a copy owned by the garbage collector, so the caller does not free it.
func capabilitySID(name string) (*windows.SID, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	// The two out parameters are arrays of SID pointers (PSID*), allocated by the call.
	var groupSids, capSids **windows.SID
	var groupCount, capCount uint32
	r, _, e := procDeriveCapabilitySidsFromName.Call(
		uintptr(unsafe.Pointer(n)),
		uintptr(unsafe.Pointer(&groupSids)), uintptr(unsafe.Pointer(&groupCount)),
		uintptr(unsafe.Pointer(&capSids)), uintptr(unsafe.Pointer(&capCount)),
	)
	if r == 0 {
		return nil, fmt.Errorf("DeriveCapabilitySidsFromName: %w", e)
	}
	defer func() {
		if capSids != nil {
			_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(capSids)))
		}
		if groupSids != nil {
			_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(groupSids)))
		}
	}()
	if capCount == 0 || capSids == nil {
		return nil, fmt.Errorf("DeriveCapabilitySidsFromName: no capability sid for %q", name)
	}
	// A capability name maps to a single capability SID for our purposes; the group
	// array is not used. Copy the SID into memory we own before freeing the call's.
	caps := unsafe.Slice(capSids, capCount)
	return caps[0].Copy()
}

// fileAllAccess and fileGenericReadExecute are the file-object access masks the grants
// use, named in specific rights rather than generic ones. A generic right stored in an
// access-list entry is mapped to an object's specific rights only when a child object
// inherits the entry, never for the entry on the directory it is set on. A directory
// granted GENERIC_ALL therefore still denies the list and traverse rights that name
// implies: files under it are readable through their inherited entries, while the
// directory itself cannot be opened or enumerated. Naming the specific bits grants the
// directory and its contents alike.
const (
	fileAllAccess          = 0x1F01FF // FILE_ALL_ACCESS
	fileGenericReadExecute = 0x1200A9 // FILE_GENERIC_READ | FILE_GENERIC_EXECUTE
)

// grantDir adds an inheritable full-access entry for the AppContainer SID to dir's
// access list, merged with the existing list so the user keeps its own access. This is
// the one writable location the contained command is given; everything else on the
// host stays default-deny.
func grantDir(dir string, sid *windows.SID) error {
	return mergeAccessEntry(dir, windows.EXPLICIT_ACCESS{
		AccessPermissions: fileAllAccess,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee:           sidTrustee(sid, windows.TRUSTEE_IS_GROUP),
	})
}

// jobActiveProcessLimit is a generous backstop on the number of processes a confined
// command's job may hold at once. Ordinary builds use far fewer; the cap exists to stop
// a fork bomb from exhausting the host, not to constrain legitimate work.
const jobActiveProcessLimit = 4096

// jobLimitFlags are the containment limits set on a confined command's job: kill every
// process in the job when the last handle to it closes (reap any survivor), cap the
// number of processes (fork-bomb backstop), and end a process on an unhandled exception
// rather than hanging on an error dialog.
const jobLimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
	windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS |
	windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION

// jobObjectLimitJobMemory caps the memory committed by the whole job (every process in
// it) via JOBOBJECT_EXTENDED_LIMIT_INFORMATION.JobMemoryLimit. x/sys/windows does not
// export the flag, so it is defined here from the Windows SDK (JOB_OBJECT_LIMIT_JOB_MEMORY).
const jobObjectLimitJobMemory = 0x00000200

// applyJobLimits places process in a new job object that contains a runaway command.
// Every process in the job is killed when the last handle to the job closes, so a child
// the command spawned cannot outlive the run; the number of processes is capped as a
// fork-bomb backstop; an unhandled exception ends the process instead of hanging on an
// error dialog; and the job is denied the user-interface surfaces a command has no need
// for. Child processes inherit the job, so the whole tree is contained. When lim sets a
// memory cap or a tighter process cap, the job enforces those too, so a memory bomb or a
// fork storm is bounded at this tier as well as at the stronger tiers. It returns the job
// handle, which the caller closes when the command is done; closing it reaps any survivors.
func applyJobLimits(process windows.Handle, lim ResourceLimits) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	activeProcs := uint32(jobActiveProcessLimit)
	if lim.MaxProcesses > 0 {
		activeProcs = uint32(lim.MaxProcesses)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags:         jobLimitFlags,
			ActiveProcessLimit: activeProcs,
		},
	}
	if lim.MemoryMiB > 0 {
		limits.BasicLimitInformation.LimitFlags |= jobObjectLimitJobMemory
		limits.JobMemoryLimit = uintptr(lim.MemoryMiB) * 1024 * 1024
	}
	if _, err := windows.SetInformationJobObject(job, uint32(windows.JobObjectExtendedLimitInformation), uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("set job limits: %w", err)
	}
	// Deny the user-interface surfaces (clipboard, desktop, global atoms, and so on).
	// Best-effort defense in depth: a failure here does not weaken the containment above.
	ui := windows.JOBOBJECT_BASIC_UI_RESTRICTIONS{
		UIRestrictionsClass: windows.JOB_OBJECT_UILIMIT_DESKTOP |
			windows.JOB_OBJECT_UILIMIT_DISPLAYSETTINGS |
			windows.JOB_OBJECT_UILIMIT_EXITWINDOWS |
			windows.JOB_OBJECT_UILIMIT_GLOBALATOMS |
			windows.JOB_OBJECT_UILIMIT_HANDLES |
			windows.JOB_OBJECT_UILIMIT_READCLIPBOARD |
			windows.JOB_OBJECT_UILIMIT_SYSTEMPARAMETERS |
			windows.JOB_OBJECT_UILIMIT_WRITECLIPBOARD,
	}
	_, _ = windows.SetInformationJobObject(job, uint32(windows.JobObjectBasicUIRestrictions), uintptr(unsafe.Pointer(&ui)), uint32(unsafe.Sizeof(ui)))
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("assign to job: %w", err)
	}
	return job, nil
}

// acProcess is a started AppContainer process and the handles that own its lifetime:
// the process and its initial thread, the job object that contains it (closing the last
// job handle reaps any survivor the command spawned), and the read end of its combined
// output pipe. The blocking launch (launchAppContainer) and the streaming launch
// (startStreamAppContainer) both build on spawnAppContainer; each owns closing these
// handles once the process is done.
type acProcess struct {
	pi   windows.ProcessInformation
	job  windows.Handle
	read windows.Handle // parent's read end: the child's combined stdout+stderr, or (duplex) stdout alone
	// errRead and writeIn are set only for a duplex session launch: the parent's read end of
	// the child's separate stderr, and the parent's write end of the child's stdin, held open
	// for an interactive conversation. They are zero for the combined one-shot and streaming
	// launches, whose stderr is folded into read and whose stdin is fed once and closed.
	errRead windows.Handle
	writeIn windows.Handle
	// reaped tells the process registry this child is no longer live. Both launch paths
	// reach closeProcess only after the child has been waited on, so that is where it runs.
	reaped func()
}

// confinedIO configures how a confined child's standard streams are wired. The zero value
// is a child with a single combined stdout+stderr pipe and no input, the shape the one-shot
// and streaming launches use. stdinOnce feeds a one-time value on stdin and then closes it.
// duplex instead gives the child three separate pipes: a live stdin the parent keeps open,
// and separate stdout and stderr the parent reads, the shape an MCP session needs so its
// JSON-RPC stdout is never corrupted by the child's stderr logging.
type confinedIO struct {
	duplex    bool
	stdinOnce []byte
}

// closeProcess releases the process, thread, and job handles. It does not close read,
// errRead, or writeIn, whose ownership passes to the caller: the blocking path drains and
// closes read directly, the streaming and session paths wrap the handles in *os.File and
// close those.
func (p *acProcess) closeProcess() {
	_ = windows.CloseHandle(p.pi.Thread)
	_ = windows.CloseHandle(p.pi.Process)
	_ = windows.CloseHandle(p.job) // closing the last job handle reaps any survivor
	p.reaped()
}

// spawnAppContainer creates and starts a command inside the AppContainer named by sid,
// with the given capabilities, working directory, and environment, and returns a handle
// to the running process. It is the AppContainer face of spawnConfined: the container
// identity and capabilities become the process's security-capabilities attribute.
func spawnAppContainer(appName, cmdline, dir string, env *uint16, sid *windows.SID, caps []*windows.SID, io confinedIO, resLimits ResourceLimits) (*acProcess, error) {
	capAttrs := make([]windows.SIDAndAttributes, 0, len(caps))
	for _, c := range caps {
		capAttrs = append(capAttrs, windows.SIDAndAttributes{Sid: c, Attributes: windows.SE_GROUP_ENABLED})
	}
	sc := securityCapabilities{AppContainerSid: sid, CapabilityCount: uint32(len(capAttrs))}
	if len(capAttrs) > 0 {
		sc.Capabilities = &capAttrs[0]
	}
	return spawnConfined(appName, cmdline, dir, env, &sc, 0, io, resLimits)
}

// confinedPipes holds the pipe handles wired to a confined child's standard streams.
// Non-duplex: one output pipe carrying the child's combined stdout+stderr (rdOut is the
// parent read end, wrOut the child write end) plus an optional one-shot stdin. Duplex:
// three pipes so an interactive child has a live stdin the parent keeps open (wrIn) and
// separate stdout (rdOut) and stderr (rdErr) the parent reads, which keeps the child's
// stderr logging out of its JSON-RPC stdout.
//
// The parent-side ends (rdOut, rdErr, wrIn) are the ones handed to the caller on success;
// the child-side ends (wrOut, wrErr, rdIn) are the only handles the child inherits, and
// the parent's copies of them are closed as soon as the child holds its own. Naming the
// two sets apart is what makes the unwind paths checkable by eye: each failure closes one
// named set, and releaseChildEnds zeroes what it closes so nothing is closed twice.
type confinedPipes struct {
	rdOut, wrOut windows.Handle // stdout (combined output when !duplex)
	rdErr, wrErr windows.Handle // stderr (duplex only)
	rdIn, wrIn   windows.Handle // stdin
}

// newConfinedPipes creates the pipes io asks for. Every parent-side end is made
// non-inheritable so it does not leak into the child; only the child-side ends are
// inherited, and only those the attribute list names explicitly. A failure closes
// everything already open and returns no handles.
func newConfinedPipes(io confinedIO) (*confinedPipes, error) {
	sa := &windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))

	p := &confinedPipes{}
	if err := windows.CreatePipe(&p.rdOut, &p.wrOut, sa, 0); err != nil {
		return nil, fmt.Errorf("sandbox: pipe: %w", err)
	}
	if err := windows.SetHandleInformation(p.rdOut, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		p.closeAll()
		return nil, fmt.Errorf("sandbox: handle info: %w", err)
	}
	if io.duplex {
		if err := windows.CreatePipe(&p.rdErr, &p.wrErr, sa, 0); err != nil {
			p.closeAll()
			return nil, fmt.Errorf("sandbox: stderr pipe: %w", err)
		}
		if err := windows.SetHandleInformation(p.rdErr, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			p.closeAll()
			return nil, fmt.Errorf("sandbox: stderr handle info: %w", err)
		}
	}
	// A stdin pipe is created for a duplex session (kept open for the parent to write) or a
	// one-shot stdin feed. The read end is inheritable (the child reads it); the write end
	// stays with the parent. With neither, the child simply has no input.
	if io.duplex || len(io.stdinOnce) > 0 {
		if err := windows.CreatePipe(&p.rdIn, &p.wrIn, sa, 0); err != nil {
			p.closeAll()
			return nil, fmt.Errorf("sandbox: stdin pipe: %w", err)
		}
		if err := windows.SetHandleInformation(p.wrIn, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			p.closeAll()
			return nil, fmt.Errorf("sandbox: stdin handle info: %w", err)
		}
	}
	return p, nil
}

// closeAll closes every pipe handle still open; used to unwind a setup failure before the
// child holds its own copies.
func (p *confinedPipes) closeAll() {
	for _, h := range []windows.Handle{p.rdOut, p.wrOut, p.rdErr, p.wrErr, p.rdIn, p.wrIn} {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}
}

// closeParentEnds closes the ends that would have been handed back to the caller on
// success. It is the unwind for every failure after the pipes exist.
func (p *confinedPipes) closeParentEnds() {
	for _, h := range []windows.Handle{p.rdOut, p.rdErr, p.wrIn} {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}
}

// closeChildEnds releases the child-side (inherited) ends on a path that fails before the
// child holds its own copies.
func (p *confinedPipes) closeChildEnds() {
	for _, h := range []windows.Handle{p.wrOut, p.wrErr, p.rdIn} {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}
}

// releaseChildEnds closes the parent's copies of the inherited ends once the child holds
// its own, and zeroes them so a later failure cleanup cannot double-close. It runs whether
// or not process creation succeeded: on failure the child never started, and the parent's
// copies are dead either way.
func (p *confinedPipes) releaseChildEnds() {
	for _, h := range []*windows.Handle{&p.wrOut, &p.wrErr, &p.rdIn} {
		if *h != 0 {
			_ = windows.CloseHandle(*h)
			*h = 0
		}
	}
}

// inheritable lists the child-side ends the child is allowed to inherit: the output write
// end, the separate stderr write end when duplex, and the stdin read end when there is
// one. Whatever other inheritable handles this process happens to hold stay behind.
func (p *confinedPipes) inheritable() []windows.Handle {
	handles := []windows.Handle{p.wrOut}
	if p.wrErr != 0 {
		handles = append(handles, p.wrErr)
	}
	if p.rdIn != 0 {
		handles = append(handles, p.rdIn)
	}
	return handles
}

// startupInfo wires the child's standard handles and the attribute list into the extended
// startup info. stderr goes to the combined output pipe unless the session is duplex,
// which gives it its own. A zero StdInput means the child has no input.
func (p *confinedPipes) startupInfo(al *windows.ProcThreadAttributeListContainer, duplex bool) *windows.StartupInfoEx {
	si := new(windows.StartupInfoEx)
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags |= windows.STARTF_USESTDHANDLES
	si.StdOutput = p.wrOut
	si.StdErr = p.wrOut // combined by default; the duplex path redirects stderr to its own pipe
	if duplex {
		si.StdErr = p.wrErr
	}
	si.StdInput = p.rdIn // zero when no stdin: the child has no input
	si.ProcThreadAttributeList = al.List()
	return si
}

// confinedAttributes builds the proc-thread attribute list that carries the confinement
// into CreateProcess: the AppContainer identity when sc is non-nil (the security-
// capabilities attribute is what makes the kernel build the container token at creation),
// the exact set of handles the child may inherit, and the process-mitigation policies
// that harden the child on top of the container.
//
// The caller owns the returned list and must Delete it, and must keep it alive until
// CreateProcess returns. That is not only hygiene: Update retains the pointer it is given
// rather than copying the value, so the handle slice and the policy word built here stay
// reachable only through the list. Dropping the list early would free memory the kernel
// still reads.
func confinedAttributes(sc *securityCapabilities, handles []windows.Handle) (*windows.ProcThreadAttributeListContainer, error) {
	al, err := windows.NewProcThreadAttributeList(3)
	if err != nil {
		return nil, fmt.Errorf("sandbox: attribute list: %w", err)
	}
	if sc != nil {
		if err := al.Update(procThreadAttributeSecurityCapabilities, unsafe.Pointer(sc), unsafe.Sizeof(*sc)); err != nil {
			al.Delete()
			return nil, fmt.Errorf("sandbox: security capabilities: %w", err)
		}
	}
	if err := al.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		al.Delete()
		return nil, fmt.Errorf("sandbox: handle list: %w", err)
	}
	// Win32k lockdown is deliberately not in sandboxMitigationPolicy; see the constant.
	policy := uint64(sandboxMitigationPolicy)
	if err := al.Update(procThreadAttributeMitigationPolicy, unsafe.Pointer(&policy), unsafe.Sizeof(policy)); err != nil {
		al.Delete()
		return nil, fmt.Errorf("sandbox: mitigation policy: %w", err)
	}
	return al, nil
}

// createConfinedProcess creates the child suspended, so it can be placed in its job with
// the limits in force before it runs a single instruction. A non-zero token is the
// write-restricted tier: the child's primary token is the restricted one, and
// CreateProcessAsUser accepts a restricted version of the caller's own token without any
// assign-primary-token privilege.
func createConfinedProcess(appName, cmdline, dir string, env *uint16, token windows.Token, si *windows.StartupInfoEx) (windows.ProcessInformation, error) {
	appPtr, _ := windows.UTF16PtrFromString(appName)
	clPtr, _ := windows.UTF16PtrFromString(cmdline)
	dirPtr, _ := windows.UTF16PtrFromString(dir)

	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	var err error
	if token != 0 {
		err = windows.CreateProcessAsUser(token, appPtr, clPtr, nil, nil, true, flags, env, dirPtr, &si.StartupInfo, &pi)
	} else {
		err = windows.CreateProcess(appPtr, clPtr, nil, nil, true, flags, env, dirPtr, &si.StartupInfo, &pi)
	}
	if err != nil {
		return windows.ProcessInformation{}, fmt.Errorf("sandbox: create process: %w", err)
	}
	return pi, nil
}

// feedStdinOnce writes a one-shot stdin value on a separate goroutine, so a value larger
// than the pipe buffer cannot deadlock against a child that has not started reading, then
// closes the writer so the child sees end-of-input. The bytes never touch the command
// line. Ownership of w passes to the goroutine.
func feedStdinOnce(w windows.Handle, b []byte) {
	go func() {
		for len(b) > 0 {
			n, werr := windows.Write(w, b)
			if n > 0 {
				b = b[n:]
			}
			if werr != nil {
				break
			}
		}
		_ = windows.CloseHandle(w)
	}()
}

// spawnConfined creates and starts a confined command and returns a handle to the
// running process. The confinement identity comes from exactly one of two mechanisms:
// a non-nil sc launches the child inside that AppContainer (the security-capabilities
// attribute builds the container token at creation), while a non-zero token launches
// the child with that token as its primary token (the write-restricted tier). Only the
// single output-pipe write handle (and the stdin read handle when present) is inherited
// by the child; the child is created suspended and placed in its job before it runs a
// single instruction, and the mitigation policies are applied at creation. When
// resLimits sets a memory or process cap, the job object that contains the child
// enforces it. The caller reads p.read and, when done, calls p.closeProcess and closes
// p.read. A failure to launch is an error and leaves no handles for the caller.
//
// The phases are: build the pipes, describe the confinement as attributes, create the
// child suspended, contain it in a job, then resume it. Each phase has one unwind, and
// the success flag is what keeps a late failure from handing back live handles.
func spawnConfined(appName, cmdline, dir string, env *uint16, sc *securityCapabilities, token windows.Token, io confinedIO, resLimits ResourceLimits) (*acProcess, error) {
	pipes, err := newConfinedPipes(io)
	if err != nil {
		return nil, err
	}

	// On any failure from here on, the parent-side ends we would otherwise hand back are
	// closed here. The child-side ends are closed by closeChildEnds before process creation
	// and by releaseChildEnds after it; the process, thread, and job handles are guarded by
	// this same flag below.
	success := false
	defer func() {
		if !success {
			pipes.closeParentEnds()
		}
	}()

	al, err := confinedAttributes(sc, pipes.inheritable())
	if err != nil {
		pipes.closeChildEnds()
		return nil, err
	}
	defer al.Delete()

	pi, err := createConfinedProcess(appName, cmdline, dir, env, token, pipes.startupInfo(al, io.duplex))
	// The child now holds its own copies of the inherited (child-side) ends; close the
	// parent's copies and zero them so the failure cleanup does not double-close.
	pipes.releaseChildEnds()
	if err != nil {
		// The parent-side ends are closed by the deferred cleanup.
		return nil, err
	}
	// The process, thread, and job handles are kept for the caller on success and closed
	// here on any error path below, guarded by the same success flag.
	defer func() {
		if !success {
			_ = windows.CloseHandle(pi.Thread)
			_ = windows.CloseHandle(pi.Process)
		}
	}()

	// Contain the command in a job object (fork-bomb cap, reap any child it spawns when
	// the run ends), then start it.
	job, err := applyJobLimits(pi.Process, resLimits)
	if err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	defer func() {
		if !success {
			_ = windows.CloseHandle(job) // closing the last job handle reaps any survivors
		}
	}()
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		return nil, fmt.Errorf("sandbox: resume: %w", err)
	}

	// A duplex session keeps wrIn open and hands it to the caller; a one-shot feed instead
	// gives the handle away to the writer goroutine and hands back nothing.
	if !io.duplex && pipes.wrIn != 0 {
		w := pipes.wrIn
		pipes.wrIn = 0 // ownership passes to the goroutine; do not hand it back or close it here
		feedStdinOnce(w, io.stdinOnce)
	}

	success = true
	return &acProcess{pi: pi, job: job, read: pipes.rdOut, errRead: pipes.rdErr, writeIn: pipes.wrIn, reaped: procs.Started()}, nil
}

// launchAppContainer runs a command inside the AppContainer named by sid and returns its
// combined output and exit code. A non-zero exit is a normal result; only a failure to
// launch or a cancelled context is an error. Output is drained on a separate goroutine so
// a command that writes more than the pipe buffer cannot deadlock, and the process is
// killed if ctx is cancelled before it exits.
func launchAppContainer(ctx context.Context, appName, cmdline, dir string, env *uint16, sid *windows.SID, caps []*windows.SID, stdin []byte, resLimits ResourceLimits) (ExecResult, error) {
	p, err := spawnAppContainer(appName, cmdline, dir, env, sid, caps, confinedIO{stdinOnce: stdin}, resLimits)
	if err != nil {
		return ExecResult{}, err
	}
	return drainProcess(ctx, p)
}

// launchWriteRestricted runs a command under the workspace's write-restricted token and
// returns its combined output and exit code, with the same draining and cancellation
// semantics as launchAppContainer.
func launchWriteRestricted(ctx context.Context, appName, cmdline, root string, env *uint16, stdin []byte, resLimits ResourceLimits) (ExecResult, error) {
	p, err := spawnWriteRestricted(appName, cmdline, root, env, confinedIO{stdinOnce: stdin}, resLimits)
	if err != nil {
		return ExecResult{}, err
	}
	return drainProcess(ctx, p)
}

// drainProcess waits for a started confined process, collecting its combined output. A
// non-zero exit is a normal result; only a cancelled context is an error. Output is
// drained on a separate goroutine so a command that writes more than the pipe buffer
// cannot deadlock, and the process is killed if ctx is cancelled before it exits.
func drainProcess(ctx context.Context, p *acProcess) (ExecResult, error) {
	defer p.closeProcess()
	defer func() { _ = windows.CloseHandle(p.read) }()

	outCh := make(chan []byte, 1)
	go func() {
		out, buf := []byte(nil), make([]byte, 4096)
		for {
			var n uint32
			e := windows.ReadFile(p.read, buf, &n, nil)
			if n > 0 {
				out = append(out, buf[:n]...)
			}
			if e != nil { // ERROR_BROKEN_PIPE at end of output
				break
			}
		}
		outCh <- out
	}()

	waited := make(chan struct{})
	go func() {
		_, _ = windows.WaitForSingleObject(p.pi.Process, windows.INFINITE)
		close(waited)
	}()

	select {
	case <-ctx.Done():
		_ = windows.TerminateProcess(p.pi.Process, 1)
		<-waited
		<-outCh
		return ExecResult{}, fmt.Errorf("sandbox: exec: %w", ctx.Err())
	case <-waited:
	}

	out := <-outCh
	var code uint32
	_ = windows.GetExitCodeProcess(p.pi.Process, &code)
	return ExecResult{Output: string(out), ExitCode: int(code)}, nil
}

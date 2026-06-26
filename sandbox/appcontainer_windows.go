//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
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
// child beyond the AppContainer boundary. The headline is the Win32k system-call
// disable, which removes the kernel's window-manager and graphics syscall surface (a
// large, historically exploited attack surface) from a command that has no legitimate
// need for it. The rest deny code-injection and DLL-planting avenues and enable the
// standard exploit mitigations. Policies that would break ordinary developer commands
// are deliberately excluded: prohibit-dynamic-code (breaks just-in-time compilers),
// block-non-Microsoft-binaries (breaks ordinary third-party tools), and
// strict-handle-checks (terminates a process on a double-close some tools do benignly).
const (
	mitigationDEPEnable               = 0x01
	mitigationSEHOPEnable             = 0x04
	mitigationBottomUpASLR            = 0x01 << 16
	mitigationHighEntropyASLR         = 0x01 << 20
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
	mitigationWin32kSystemCallDisable |
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
// and its on-disk folder. It is best-effort: a profile that was never created, or is
// in use by another sandbox on the same working directory, is left as is.
func deleteAppContainerProfile(moniker string) {
	m, err := windows.UTF16PtrFromString(moniker)
	if err != nil {
		return
	}
	_, _, _ = procDeleteAppContainerProfile.Call(uintptr(unsafe.Pointer(m)))
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

// grantDir adds an inheritable full-access entry for the AppContainer SID to dir's
// access list, merged with the existing list so the user keeps its own access. This is
// the one writable location the contained command is given; everything else on the
// host stays default-deny.
func grantDir(dir string, sid *windows.SID) error {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read access list: %w", err)
	}
	existing, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("access list: %w", err)
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}
	merged, err := windows.ACLFromEntries(entries, existing)
	if err != nil {
		return fmt.Errorf("merge access list: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, merged, nil); err != nil {
		return fmt.Errorf("apply access list: %w", err)
	}
	return nil
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

// applyJobLimits places process in a new job object that contains a runaway command.
// Every process in the job is killed when the last handle to the job closes, so a child
// the command spawned cannot outlive the run; the number of processes is capped as a
// fork-bomb backstop; an unhandled exception ends the process instead of hanging on an
// error dialog; and the job is denied the user-interface surfaces a command has no need
// for. Child processes inherit the job, so the whole tree is contained. It returns the
// job handle, which the caller closes when the command is done; closing it reaps any
// survivors.
func applyJobLimits(process windows.Handle) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags:         jobLimitFlags,
			ActiveProcessLimit: jobActiveProcessLimit,
		},
	}
	if _, err := windows.SetInformationJobObject(job, uint32(windows.JobObjectExtendedLimitInformation), uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
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
		windows.CloseHandle(job)
		return 0, fmt.Errorf("assign to job: %w", err)
	}
	return job, nil
}

// launchAppContainer runs a command inside the AppContainer named by sid, with the
// given capabilities, working directory, and environment, and returns its combined
// output and exit code. A non-zero exit is a normal result; only a failure to launch
// or a cancelled context is an error. Output is drained on a separate goroutine so a
// command that writes more than the pipe buffer cannot deadlock, and only the single
// output-pipe handle is inherited by the child.
func launchAppContainer(ctx context.Context, appName, cmdline, dir string, env *uint16, sid *windows.SID, caps []*windows.SID) (ExecResult, error) {
	capAttrs := make([]windows.SIDAndAttributes, 0, len(caps))
	for _, c := range caps {
		capAttrs = append(capAttrs, windows.SIDAndAttributes{Sid: c, Attributes: windows.SE_GROUP_ENABLED})
	}
	sc := securityCapabilities{AppContainerSid: sid, CapabilityCount: uint32(len(capAttrs))}
	if len(capAttrs) > 0 {
		sc.Capabilities = &capAttrs[0]
	}

	// Combined-output pipe. The write end is inheritable so the child can use it; the
	// read end is made non-inheritable so it does not leak into the child.
	var rd, wr windows.Handle
	sa := &windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	if err := windows.CreatePipe(&rd, &wr, sa, 0); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: pipe: %w", err)
	}
	defer windows.CloseHandle(rd)
	if err := windows.SetHandleInformation(rd, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		windows.CloseHandle(wr)
		return ExecResult{}, fmt.Errorf("sandbox: handle info: %w", err)
	}

	al, err := windows.NewProcThreadAttributeList(3)
	if err != nil {
		windows.CloseHandle(wr)
		return ExecResult{}, fmt.Errorf("sandbox: attribute list: %w", err)
	}
	defer al.Delete()
	if err := al.Update(procThreadAttributeSecurityCapabilities, unsafe.Pointer(&sc), unsafe.Sizeof(sc)); err != nil {
		windows.CloseHandle(wr)
		return ExecResult{}, fmt.Errorf("sandbox: security capabilities: %w", err)
	}
	// Inherit only the output pipe, not whatever other inheritable handles this process
	// happens to hold.
	handles := []windows.Handle{wr}
	if err := al.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		windows.CloseHandle(wr)
		return ExecResult{}, fmt.Errorf("sandbox: handle list: %w", err)
	}
	// Harden the child with process-mitigation policies (Win32k lockdown, no code
	// injection or DLL planting, standard exploit mitigations) on top of the container.
	policy := uint64(sandboxMitigationPolicy)
	if err := al.Update(procThreadAttributeMitigationPolicy, unsafe.Pointer(&policy), unsafe.Sizeof(policy)); err != nil {
		windows.CloseHandle(wr)
		return ExecResult{}, fmt.Errorf("sandbox: mitigation policy: %w", err)
	}

	si := new(windows.StartupInfoEx)
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(*si))
	si.StartupInfo.Flags |= windows.STARTF_USESTDHANDLES
	si.StartupInfo.StdOutput = wr
	si.StartupInfo.StdErr = wr
	si.ProcThreadAttributeList = al.List()

	appPtr, _ := windows.UTF16PtrFromString(appName)
	clPtr, _ := windows.UTF16PtrFromString(cmdline)
	dirPtr, _ := windows.UTF16PtrFromString(dir)

	var pi windows.ProcessInformation
	// The child is created suspended so it is placed in its job, with the limits in
	// force, before it runs a single instruction.
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	err = windows.CreateProcess(appPtr, clPtr, nil, nil, true, flags, env, dirPtr, &si.StartupInfo, &pi)
	windows.CloseHandle(wr) // the parent never writes; the child holds its own copy
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: create process: %w", err)
	}
	defer windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)

	// Contain the command in a job object (fork-bomb cap, reap any child it spawns when
	// the run ends), then start it.
	job, err := applyJobLimits(pi.Process)
	if err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		return ExecResult{}, fmt.Errorf("sandbox: %w", err)
	}
	defer windows.CloseHandle(job) // closing the last job handle reaps any survivors
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		return ExecResult{}, fmt.Errorf("sandbox: resume: %w", err)
	}

	outCh := make(chan []byte, 1)
	go func() {
		out, buf := []byte(nil), make([]byte, 4096)
		for {
			var n uint32
			e := windows.ReadFile(rd, buf, &n, nil)
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
		_, _ = windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		close(waited)
	}()

	select {
	case <-ctx.Done():
		_ = windows.TerminateProcess(pi.Process, 1)
		<-waited
		<-outCh
		return ExecResult{}, fmt.Errorf("sandbox: exec: %w", ctx.Err())
	case <-waited:
	}

	out := <-outCh
	var code uint32
	_ = windows.GetExitCodeProcess(pi.Process, &code)
	return ExecResult{Output: string(out), ExitCode: int(code)}, nil
}

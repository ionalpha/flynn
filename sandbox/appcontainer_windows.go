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

// This file holds the Win32 calls that launch a confined process. The choice of
// confinement is in confine_windows.go.

// procThreadAttributeSecurityCapabilities is PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES,
// which x/sys/windows does not export. Set at creation, it makes the kernel build
// the container token and namespace before the command runs.
const procThreadAttributeSecurityCapabilities = 0x00020009

// errAlreadyExists is HRESULT_FROM_WIN32(ERROR_ALREADY_EXISTS), returned when the
// profile exists; the SID is then derived.
const errAlreadyExists = 0x800700B7

// procThreadAttributeMitigationPolicy is PROC_THREAD_ATTRIBUTE_MITIGATION_POLICY,
// which x/sys/windows does not export.
const procThreadAttributeMitigationPolicy = 0x00020007

// Mitigation policy bits for every confined command, against code injection and
// DLL planting. Excluded because they break ordinary commands:
// prohibit-dynamic-code (JIT compilers), block-non-Microsoft-binaries
// (third-party tools), strict-handle-checks (kills a process on a benign double
// close), and the Win32k syscall disable (below).
const (
	mitigationDEPEnable       = 0x01
	mitigationSEHOPEnable     = 0x04
	mitigationBottomUpASLR    = 0x01 << 16
	mitigationHighEntropyASLR = 0x01 << 20
	// mitigationWin32kSystemCallDisable must never be applied. user32.dll cannot
	// initialize without the window-manager syscalls, so node, git, python,
	// powershell and the agent CLIs die in DLL init with STATUS_DLL_INIT_FAILED
	// (exit 0xC0000142) before main. It is defined so a test can check its absence.
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

// securityCapabilities mirrors SECURITY_CAPABILITIES, which x/sys/windows does not
// export.
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

// liveProfiles counts profiles this process registered and has not deleted. A
// profile outlives the process, so an unclosed sandbox leaks one. The profile
// directory cannot show this, because other processes register there too.
var liveProfiles atomic.Int64

// LiveProfileCount reports AppContainer profiles this process registered and has
// not deleted, so a nonzero count after every Close is a leak. It is always zero
// off Windows.
func LiveProfileCount() int { return int(liveProfiles.Load()) }

// createOrDeriveACSID registers the profile, which creates the object namespace
// the launch needs, or derives the SID if it exists. Free the SID with
// windows.FreeSid.
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

// deriveACSID returns a SID to free with windows.FreeSid.
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

// deleteAppContainerProfile succeeds for a profile never created, so an error
// means one is still registered. The moniker comes from the workspace path, so
// its SID and every access entry for it stay live; the caller must report it.
func deleteAppContainerProfile(moniker string) error {
	m, err := windows.UTF16PtrFromString(moniker)
	if err != nil {
		return fmt.Errorf("DeleteAppContainerProfile %q: %w", moniker, err)
	}
	if r, _, _ := procDeleteAppContainerProfile.Call(uintptr(unsafe.Pointer(m))); r != 0 {
		return fmt.Errorf("DeleteAppContainerProfile %q: hresult=0x%x", moniker, uint32(r))
	}
	// Deleting another run's profile also succeeds, so clamp at zero.
	if liveProfiles.Load() > 0 {
		liveProfiles.Add(-1)
	}
	return nil
}

// capabilitySID returns a garbage-collected copy, so the caller does not free it.
// internetClient, for example, grants outbound network.
func capabilitySID(name string) (*windows.SID, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	// Both out parameters are PSID* arrays the call allocates.
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
	// The group array is unused. Copy the SID before freeing the call's memory.
	caps := unsafe.Slice(capSids, capCount)
	return caps[0].Copy()
}

// fileAllAccess and fileGenericReadExecute use specific rights because a generic
// right in an entry is mapped only when a child inherits it. A directory granted
// GENERIC_ALL cannot itself be opened or listed, though its files can.
const (
	fileAllAccess          = 0x1F01FF // FILE_ALL_ACCESS
	fileGenericReadExecute = 0x1200A9 // FILE_GENERIC_READ | FILE_GENERIC_EXECUTE
)

// grantDir adds an inheritable full-access entry for sid, merged so the user keeps
// its own access.
func grantDir(dir string, sid *windows.SID) error {
	return mergeAccessEntry(dir, windows.EXPLICIT_ACCESS{
		AccessPermissions: fileAllAccess,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee:           sidTrustee(sid, windows.TRUSTEE_IS_GROUP),
	})
}

// jobActiveProcessLimit is set far above what a build uses, to stop a fork bomb.
const jobActiveProcessLimit = 4096

// jobLimitFlags kill the job when its last handle closes, cap processes, and end
// a process on an unhandled exception instead of showing an error dialog.
const jobLimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
	windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS |
	windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION

// jobObjectLimitJobMemory is JOB_OBJECT_LIMIT_JOB_MEMORY, which x/sys/windows does
// not export. It caps the whole job via JobMemoryLimit.
const jobObjectLimitJobMemory = 0x00000200

// applyJobLimits puts process in a new job with jobLimitFlags, lim's caps and no
// UI access. Children inherit the job. Closing the returned handle kills every
// survivor, so no descendant outlives the run.
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
	// Best effort: the limits above hold even if this fails.
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

// acProcess holds a started confined process's handles. Closing job kills any
// survivor.
type acProcess struct {
	pi   windows.ProcessInformation
	job  windows.Handle
	read windows.Handle // parent's read end: the child's combined stdout+stderr, or (duplex) stdout alone
	// errRead and writeIn are set only for a duplex launch; otherwise stderr is in
	// read and stdin is fed once.
	errRead windows.Handle
	writeIn windows.Handle
	// reaped runs in closeProcess, which both paths reach only after the wait.
	reaped func()
}

// confinedIO's zero value is one combined output pipe and no input. duplex gives
// three pipes, so an MCP child's stderr logging cannot corrupt its JSON-RPC stdout.
type confinedIO struct {
	duplex    bool
	stdinOnce []byte
}

// closeProcess leaves read, errRead and writeIn to the caller.
func (p *acProcess) closeProcess() {
	_ = windows.CloseHandle(p.pi.Thread)
	_ = windows.CloseHandle(p.pi.Process)
	_ = windows.CloseHandle(p.job) // closing the last job handle reaps any survivor
	p.reaped()
}

// spawnAppContainer is spawnConfined with sid and caps as the security-capabilities
// attribute.
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

// confinedPipes splits parent ends (rdOut, rdErr, wrIn), returned on success, from
// child ends (wrOut, wrErr, rdIn), the only handles the child inherits. Each
// failure closes one named set, and releaseChildEnds zeroes what it closes so
// nothing is closed twice.
type confinedPipes struct {
	rdOut, wrOut windows.Handle // stdout (combined output when !duplex)
	rdErr, wrErr windows.Handle // stderr (duplex only)
	rdIn, wrIn   windows.Handle // stdin
}

// newConfinedPipes makes parent ends non-inheritable so they cannot leak into the
// child. A failure closes everything it opened.
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
	// Without duplex or stdinOnce the child has no input.
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

func (p *confinedPipes) closeAll() {
	for _, h := range []windows.Handle{p.rdOut, p.wrOut, p.rdErr, p.wrErr, p.rdIn, p.wrIn} {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}
}

func (p *confinedPipes) closeParentEnds() {
	for _, h := range []windows.Handle{p.rdOut, p.rdErr, p.wrIn} {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}
}

func (p *confinedPipes) closeChildEnds() {
	for _, h := range []windows.Handle{p.wrOut, p.wrErr, p.rdIn} {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}
}

// releaseChildEnds runs whether or not creation succeeded, because the parent's
// copies are useless either way.
func (p *confinedPipes) releaseChildEnds() {
	for _, h := range []*windows.Handle{&p.wrOut, &p.wrErr, &p.rdIn} {
		if *h != 0 {
			_ = windows.CloseHandle(*h)
			*h = 0
		}
	}
}

// inheritable lists the only handles the child inherits, so other inheritable
// handles in this process stay behind.
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

// confinedAttributes carries sc, the inheritable handles and the mitigation
// policies into CreateProcess. Keep the list alive until CreateProcess returns,
// then Delete it: Update stores pointers, so the handle slice and policy word are
// reachable only through the list.
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
	// Win32k lockdown is excluded; see mitigationWin32kSystemCallDisable.
	policy := uint64(sandboxMitigationPolicy)
	if err := al.Update(procThreadAttributeMitigationPolicy, unsafe.Pointer(&policy), unsafe.Sizeof(policy)); err != nil {
		al.Delete()
		return nil, fmt.Errorf("sandbox: mitigation policy: %w", err)
	}
	return al, nil
}

// createConfinedProcess starts the child suspended so it is in its job before it
// runs. A non-zero token is the write-restricted tier; CreateProcessAsUser takes a
// restricted copy of the caller's token without assign-primary-token privilege.
func createConfinedProcess(appName, cmdline, dir string, env *uint16, token windows.Token, si *windows.StartupInfoEx) (windows.ProcessInformation, error) {
	appPtr, _ := windows.UTF16PtrFromString(appName)
	clPtr, _ := windows.UTF16PtrFromString(cmdline)
	dirPtr, _ := windows.UTF16PtrFromString(dir)

	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	if token == 0 {
		// Without this a console child of a console-less parent opens a window
		// (internal/procexec sets it for exec.Cmd). The write-restricted child dies
		// with STATUS_DLL_INIT_FAILED under it, because its restricting SIDs cannot
		// reach the console host. That tier never starts without a parent console,
		// so it has no window to suppress.
		flags |= windows.CREATE_NO_WINDOW
	}
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

// feedStdinOnce writes in a goroutine, so a value larger than the pipe buffer
// cannot deadlock, then closes w. The goroutine owns w.
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

// spawnConfined confines by exactly one of a non-nil sc (AppContainer) or a
// non-zero token (write-restricted). The caller reads p.read, then calls
// p.closeProcess and closes p.read. A failure leaves the caller no handles.
//
// Phases: pipes, attributes, create suspended, job, resume. Each has one unwind,
// and the success flag stops a late failure from returning live handles.
func spawnConfined(appName, cmdline, dir string, env *uint16, sc *securityCapabilities, token windows.Token, io confinedIO, resLimits ResourceLimits) (*acProcess, error) {
	pipes, err := newConfinedPipes(io)
	if err != nil {
		return nil, err
	}

	// Parent ends close here on failure. Child ends close in closeChildEnds before
	// creation and releaseChildEnds after.
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
	pipes.releaseChildEnds()
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			_ = windows.CloseHandle(pi.Thread)
			_ = windows.CloseHandle(pi.Process)
		}
	}()

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

	// Duplex returns wrIn; a one-shot feed gives it to the writer goroutine.
	if !io.duplex && pipes.wrIn != 0 {
		w := pipes.wrIn
		pipes.wrIn = 0 // ownership passes to the goroutine; do not hand it back or close it here
		feedStdinOnce(w, io.stdinOnce)
	}

	success = true
	return &acProcess{pi: pi, job: job, read: pipes.rdOut, errRead: pipes.rdErr, writeIn: pipes.wrIn, reaped: procs.Started()}, nil
}

// launchAppContainer runs cmdline in the AppContainer sid; see drainProcess.
func launchAppContainer(ctx context.Context, appName, cmdline, dir string, env *uint16, sid *windows.SID, caps []*windows.SID, stdin []byte, resLimits ResourceLimits) (ExecResult, error) {
	p, err := spawnAppContainer(appName, cmdline, dir, env, sid, caps, confinedIO{stdinOnce: stdin}, resLimits)
	if err != nil {
		return ExecResult{}, err
	}
	return drainProcess(ctx, p)
}

// launchWriteRestricted runs cmdline under root's write-restricted token; see
// drainProcess.
func launchWriteRestricted(ctx context.Context, appName, cmdline, root string, env *uint16, stdin []byte, resLimits ResourceLimits) (ExecResult, error) {
	p, err := spawnWriteRestricted(appName, cmdline, root, env, confinedIO{stdinOnce: stdin}, resLimits)
	if err != nil {
		return ExecResult{}, err
	}
	return drainProcess(ctx, p)
}

// drainProcess reads output in a goroutine so a full pipe cannot deadlock, and
// kills the process on cancellation. A non-zero exit is a result.
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

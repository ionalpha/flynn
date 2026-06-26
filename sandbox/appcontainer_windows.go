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

	al, err := windows.NewProcThreadAttributeList(2)
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
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	err = windows.CreateProcess(appPtr, clPtr, nil, nil, true, flags, env, dirPtr, &si.StartupInfo, &pi)
	windows.CloseHandle(wr) // the parent never writes; the child holds its own copy
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: create process: %w", err)
	}
	defer windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)

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

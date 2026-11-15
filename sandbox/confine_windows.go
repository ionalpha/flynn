//go:build windows

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/sys/windows"
)

// kernelConfinementSupported is true through AppContainer.
func kernelConfinementSupported() bool { return true }

// egressEnforceable is false until the AppContainer plus Windows Filtering
// Platform leg exists, so a governed-egress launch refuses.
func egressEnforceable() bool { return false }

// backgroundConfinementExpressible is false because launchAppContainer blocks on
// the child's exit and returns no handle to background. Foreground Exec keeps
// the AppContainer tier.
func backgroundConfinementExpressible() bool { return false }

// confine is a no-op because exec.Cmd cannot carry AppContainer security
// attributes; confined commands use runAppContainer.
func (l *Local) confine(_ *exec.Cmd) error { return nil }

// closePlatform revokes best effort, since a leftover entry names a SID unique to
// this workspace. A profile delete failure is returned because the registered
// identity and its folder persist unseen. CleanStaleProfiles collects profiles
// from runs that crashed or skipped Close.
func (l *Local) closePlatform() error {
	scratchErr := l.removeScratch()
	if l.hostReadable {
		// The write-restricted tier has only write grants and no profile.
		_ = revokeRestrictedDir(l.root, l.root)
		l.revokeWritableDirs()
		return scratchErr
	}
	l.revokeReadableDirs()
	l.revokeWritableDirs()
	return errors.Join(deleteAppContainerProfile(appContainerMoniker(l.root)), scratchErr)
}

// runShell runs a confined command in an AppContainer: reads denied by default,
// only the working directory writable.
func (l *Local) runShell(ctx context.Context, name string, args []string, stdin []byte, confined bool) (ExecResult, error) {
	if !confined {
		return l.runWithExecCmd(ctx, name, args, stdin, false)
	}
	if l.hostReadable {
		return l.runWriteRestricted(ctx, args, stdin)
	}
	return l.runAppContainer(ctx, args, stdin)
}

// runWriteRestricted composes the command line as the container path does, so
// both tiers run identical text. The token restricts only writes, so the child
// keeps the network; [Local.NetworkDenied] reports it and guardNetworkDeny
// refuses a denial unless the Local accepted that.
func (l *Local) runWriteRestricted(ctx context.Context, args []string, stdin []byte) (ExecResult, error) {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}
	if err := grantRestrictedDir(l.root, l.root); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: grant working directory: %w", err)
	}
	if err := l.grantRestrictedWritableDirs(); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: %w", err)
	}
	line := args[len(args)-1]
	cmdline := `"` + comspec + `" /s /c "` + line + `"`
	return launchWriteRestricted(ctx, comspec, cmdline, l.root, l.appContainerEnv(), stdin, l.resLimits)
}

// runAppContainer uses an identity unique to the working directory, so two
// confined roots cannot reach each other's files.
func (l *Local) runAppContainer(ctx context.Context, args []string, stdin []byte) (ExecResult, error) {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}

	sid, err := createOrDeriveACSID(appContainerMoniker(l.root))
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: appcontainer profile: %w", err)
	}
	defer func() { _ = windows.FreeSid(sid) }()

	if err := grantDir(l.root, sid); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: grant working directory: %w", err)
	}
	if err := l.grantReadableDirs(sid); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: %w", err)
	}
	if err := l.grantWritableDirs(sid); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: %w", err)
	}

	var caps []*windows.SID
	if !l.denyNetwork {
		netCap, err := capabilitySID("internetClient")
		if err != nil {
			return ExecResult{}, fmt.Errorf("sandbox: network capability: %w", err)
		}
		caps = append(caps, netCap)
	}

	// cmd.exe /s /c runs the text between the first and last quote verbatim.
	// Ordinary composition backslash-escapes inner quotes, which cmd.exe does not
	// understand. The loader finds the binary by the explicit application name.
	line := args[len(args)-1]
	cmdline := `"` + comspec + `" /s /c "` + line + `"`
	return launchAppContainer(ctx, comspec, cmdline, l.root, l.appContainerEnv(), sid, caps, stdin, l.resLimits)
}

// appContainerMoniker hashes the root to fit the name's length and character
// limits and stay unique per root.
func appContainerMoniker(root string) string {
	sum := sha256.Sum256([]byte(root))
	return profilePrefix + hex.EncodeToString(sum[:])[:16]
}

// acProfileEnvKeys must be present or process creation fails. They hold paths
// and account names, never credentials.
var acProfileEnvKeys = []string{
	"SystemDrive", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "USERNAME", "HOMEDRIVE", "HOMEPATH",
}

// appContainerEnv points temp and toolchain caches at [Local.scratchDir].
func (l *Local) appContainerEnv() *uint16 {
	return l.appContainerEnvBlock(l.env())
}

// appContainerEnvBlock serves the one-shot path (base = env()) and streaming
// (base = streamEnv(grants)).
func (l *Local) appContainerEnvBlock(base []string) *uint16 {
	m := make(map[string]string)
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	for _, k := range acProfileEnvKeys {
		if _, ok := m[k]; ok {
			continue
		}
		if v, ok := os.LookupEnv(k); ok {
			m[k] = v
		}
	}
	if scratch, err := l.scratchDir(); err == nil {
		withScratchEnv(m, scratch, os.Getenv, runtime.GOOS)
	}
	return envBlock(m)
}

// envBlock renders a sorted, double-null-terminated UTF-16 block.
func envBlock(m map[string]string) *uint16 {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []uint16
	for _, k := range keys {
		s, err := windows.UTF16FromString(k + "=" + m[k]) // trailing NUL closes each entry
		if err != nil {
			continue
		}
		b = append(b, s...)
	}
	b = append(b, 0) // final NUL closes the block
	return &b[0]
}

// platformConfinementTier is AppContainer, or the write-restricted token under
// WithHostReadable. Both are ContainmentKernel but confine reads differently, so
// a run records the name.
func (l *Local) platformConfinementTier() string {
	if l.hostReadable {
		return "write-restricted-token"
	}
	return "appcontainer"
}

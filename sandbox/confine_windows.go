//go:build windows

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"golang.org/x/sys/windows"
)

// kernelConfinementSupported reports whether this platform can enforce the network,
// filesystem, and syscall confinement, which it can on Windows through AppContainer.
func kernelConfinementSupported() bool { return true }

// confine is a no-op on Windows. Kernel confinement here is an AppContainer, which is
// applied at process creation through security attributes that an exec.Cmd cannot
// carry, so a confined command runs through runAppContainer rather than the standard
// library. confine stays defined so the unconfined path (which does use exec.Cmd) is
// uniform with the other platforms; it is only ever called when no confinement was
// requested.
func (l *Local) confine(_ *exec.Cmd) error { return nil }

// closePlatform removes the per-working-directory AppContainer profile registered for
// confined commands, so profiles do not accumulate across runs. It is best-effort: the
// container's temporary directory is redirected into the working tree, so the profile
// folder holds no command output, and a profile still in use by another sandbox on the
// same directory is simply left in place.
func (l *Local) closePlatform() error {
	deleteAppContainerProfile(appContainerMoniker(l.root))
	return nil
}

// runShell runs a shell command, choosing the execution path by whether confinement
// was requested. Unconfined, it runs through the standard library exactly like the
// other platforms. Confined, it runs inside an AppContainer: filesystem default-deny
// with only the working directory writable, and the network allowed only when it was
// not denied. The confined flag is decided by the caller (Exec), so the always-on
// secure-by-default baseline confines a Windows command through the container too.
func (l *Local) runShell(ctx context.Context, name string, args []string, confined bool) (ExecResult, error) {
	if !confined {
		return l.runWithExecCmd(ctx, name, args, false)
	}
	return l.runAppContainer(ctx, name, args)
}

// runAppContainer builds the AppContainer policy from the Local's options and launches
// the command inside it. The container's identity is unique to the working directory,
// so commands in different sandbox roots cannot reach each other's files even though
// both are confined. The network is granted only when it was not denied; the working
// directory is the one writable location.
func (l *Local) runAppContainer(ctx context.Context, name string, args []string) (ExecResult, error) {
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

	var caps []*windows.SID
	if !l.denyNetwork {
		netCap, err := capabilitySID("internetClient")
		if err != nil {
			return ExecResult{}, fmt.Errorf("sandbox: network capability: %w", err)
		}
		caps = append(caps, netCap)
	}

	// cmd.exe with /s /c runs the text between the first and last quote verbatim, so
	// the command line (with its own quotes and redirects) passes through unchanged;
	// composing the arguments the ordinary way would backslash-escape the inner quotes,
	// which cmd.exe does not understand. The interpreter and its flags come from
	// shell(); the final argument is the command text. The explicit application name is
	// what the loader uses to find the binary.
	line := args[len(args)-1]
	cmdline := `"` + comspec + `" /s /c "` + line + `"`
	return launchAppContainer(ctx, comspec, cmdline, l.root, l.appContainerEnv(), sid, caps)
}

// appContainerMoniker derives a stable, unique AppContainer name from the absolute
// working directory. A hash keeps the name within the allowed length and character
// set while making it unique per root, so each sandbox root gets its own container
// identity.
func appContainerMoniker(root string) string {
	sum := sha256.Sum256([]byte(root))
	return "flynn.sbx." + hex.EncodeToString(sum[:])[:16]
}

// acProfileEnvKeys are the AppContainer profile-location variables that have to be
// present in a launched command's environment, otherwise process creation fails. They
// are path and account-name values, never credentials, so passing them keeps the
// no-secrets-in-the-environment guarantee.
var acProfileEnvKeys = []string{
	"SystemDrive", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "USERNAME", "HOMEDRIVE", "HOMEPATH",
}

// appContainerEnv builds the environment block for a contained command: the sandbox's
// scrubbed baseline plus the profile-location variables AppContainer requires, with
// the temporary directory pointed at the working tree so a command that needs scratch
// space writes it inside the one writable location rather than failing against the
// read-only host.
func (l *Local) appContainerEnv() *uint16 {
	m := make(map[string]string)
	for _, kv := range l.env() {
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
	m["TEMP"] = l.root
	m["TMP"] = l.root
	return envBlock(m)
}

// envBlock renders an environment map as a sorted, double-null-terminated UTF-16 block
// for process creation. Sorting keeps the block stable and testable.
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

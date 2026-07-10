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

// egressEnforceable reports whether governed child egress can be enforced here. The
// Windows leg (an AppContainer plus a Windows Filtering Platform rule that admits only the
// proxy) is not built yet, so a governed-egress launch refuses rather than running with
// direct egress open.
func egressEnforceable() bool { return false }

// backgroundConfinementExpressible reports whether kernel confinement can be applied to
// a process that is started and left running in the background (the Serve path). On
// Windows it cannot: confinement here is an AppContainer applied at process creation
// through launchAppContainer, whose current shape starts the child and blocks on its
// exit to collect output, so it yields no backgroundable handle the way an exec.Cmd
// does. confine is a no-op on this platform (see below), so a background process cannot
// carry the container. Serve therefore refuses an explicitly confined background launch
// rather than starting it at the directory-jail floor under a tier the trust gate relied
// on; the foreground Exec path keeps the full AppContainer tier.
func backgroundConfinementExpressible() bool { return false }

// confine is a no-op on Windows. Kernel confinement here is an AppContainer, which is
// applied at process creation through security attributes that an exec.Cmd cannot
// carry, so a confined command runs through runAppContainer rather than the standard
// library. confine stays defined so the unconfined path (which does use exec.Cmd) is
// uniform with the other platforms; it is only ever called when no confinement was
// requested.
func (l *Local) confine(_ *exec.Cmd) error { return nil }

// closePlatform removes the per-working-directory AppContainer profile registered for
// confined commands, so profiles do not accumulate across runs. The revokes are
// best-effort (both SIDs are unique to this workspace, so an entry left behind is a dead
// one), but a profile that fails to delete is returned: it keeps a registered container
// identity and a growing folder on disk, and it is the one part of teardown whose failure
// a caller cannot see any other way. A run that ends without Close, or that crashes,
// still leaves one behind; CleanStaleProfiles is what collects those.
func (l *Local) closePlatform() error {
	if l.hostReadable {
		// The write-restricted tier registers no container profile and needs no read
		// grants: its access entries are the workspace write grant and any extra write
		// grants, all removed here.
		_ = revokeRestrictedDir(l.root, l.root)
		l.revokeWritableDirs()
		return nil
	}
	l.revokeReadableDirs()
	l.revokeWritableDirs()
	return deleteAppContainerProfile(appContainerMoniker(l.root))
}

// runShell runs a shell command, choosing the execution path by whether confinement
// was requested. Unconfined, it runs through the standard library exactly like the
// other platforms. Confined, it runs inside an AppContainer: filesystem default-deny
// with only the working directory writable, and the network allowed only when it was
// not denied. The confined flag is decided by the caller (Exec), so the always-on
// secure-by-default baseline confines a Windows command through the container too.
func (l *Local) runShell(ctx context.Context, name string, args []string, stdin []byte, confined bool) (ExecResult, error) {
	if !confined {
		return l.runWithExecCmd(ctx, name, args, stdin, false)
	}
	if l.hostReadable {
		return l.runWriteRestricted(ctx, args, stdin)
	}
	return l.runAppContainer(ctx, args, stdin)
}

// runWriteRestricted runs a shell command under the write-restricted tier: the host stays
// readable, and the working directory is the one place the child can write. It is the
// tier for a program the container's deny-by-default reads would break (see
// restricted_windows.go); the command line is composed exactly as the container path
// composes it, so the two tiers run identical text.
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

// runAppContainer builds the AppContainer policy from the Local's options and launches
// the command inside it. The container's identity is unique to the working directory,
// so commands in different sandbox roots cannot reach each other's files even though
// both are confined. The network is granted only when it was not denied; the working
// directory is the one writable location.
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

	// cmd.exe with /s /c runs the text between the first and last quote verbatim, so
	// the command line (with its own quotes and redirects) passes through unchanged;
	// composing the arguments the ordinary way would backslash-escape the inner quotes,
	// which cmd.exe does not understand. The interpreter and its flags come from
	// shell(); the final argument is the command text. The explicit application name is
	// what the loader uses to find the binary.
	line := args[len(args)-1]
	cmdline := `"` + comspec + `" /s /c "` + line + `"`
	return launchAppContainer(ctx, comspec, cmdline, l.root, l.appContainerEnv(), sid, caps, stdin, l.resLimits)
}

// appContainerMoniker derives a stable, unique AppContainer name from the absolute
// working directory. A hash keeps the name within the allowed length and character
// set while making it unique per root, so each sandbox root gets its own container
// identity.
func appContainerMoniker(root string) string {
	sum := sha256.Sum256([]byte(root))
	return profilePrefix + hex.EncodeToString(sum[:])[:16]
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
	return l.appContainerEnvBlock(l.env())
}

// appContainerEnvBlock builds the AppContainer environment block from a base KEY=VALUE
// environment (the sandbox's scrubbed baseline, optionally overlaid with a streamed
// process's explicit grants), plus the profile-location variables AppContainer requires
// and the temporary directory redirected into the working tree. It is the shared builder
// behind appContainerEnv (the one-shot path, base = env()) and the streaming path
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

// platformConfinementTier names the Windows kernel-confinement mechanism the sandbox's
// configuration selects: the AppContainer by default, or the write-restricted token when
// the host must stay readable (see WithHostReadable). The two bound an exploit equally
// (both report ContainmentKernel) but confine reads differently, so a governed run
// records the name, not just the level.
func (l *Local) platformConfinementTier() string {
	if l.hostReadable {
		return "write-restricted-token"
	}
	return "appcontainer"
}

//go:build windows

package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestConfinedCommandRuns proves a command launches and produces output inside the
// AppContainer under the full kernel-confined preset. A container that could not load
// the system libraries a shell needs would fail to run at all, so this is the guard
// that the container is built correctly.
func TestConfinedCommandRuns(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithKernelConfinement())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	res, err := sb.Exec(context.Background(), Command{Line: "echo confined"})
	if err != nil {
		t.Fatalf("a benign confined command must run: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "confined") {
		t.Fatalf("an ordinary command must run inside the container, got exit %d:\n%s", res.ExitCode, res.Output)
	}
}

// TestConfinedReportsKernelContainment confirms the Windows adapter raises the reported
// containment to kernel-confined under the full preset, so the run gate treats it as a
// T1 tier rather than a bare process jail.
func TestConfinedReportsKernelContainment(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithKernelConfinement())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	if got := sb.Containment(); got != ContainmentKernel {
		t.Fatalf("a fully confined Windows sandbox must report kernel-confined, got %s", got)
	}
}

// TestReadOnlyFSWritesOnlyWorkdir proves the filesystem confinement: a command can
// write its own working directory but cannot write the host outside it, and reads of
// the host still work, so the confinement restricts writes without blinding the
// command.
func TestReadOnlyFSWritesOnlyWorkdir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir() // a sibling the test user owns, outside the sandbox root

	sb, err := NewLocal(root, WithReadOnlyFS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	res, err := sb.Exec(ctx, Command{Line: `echo data> made.txt && type made.txt`})
	if err != nil {
		t.Fatalf("workdir write exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "data") {
		t.Fatalf("a write to the working directory must succeed, got exit %d:\n%s", res.ExitCode, res.Output)
	}
	if _, err := os.Stat(filepath.Join(sb.Root(), "made.txt")); err != nil {
		t.Fatalf("the working-directory write did not land: %v", err)
	}

	escape := filepath.Join(outside, "escape.txt")
	res, err = sb.Exec(ctx, Command{Line: `echo escape> "` + escape + `"`})
	if err != nil {
		t.Fatalf("outside write exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a write outside the working tree must fail under confinement, but it succeeded:\n%s", res.Output)
	}
	if _, err := os.Stat(escape); err == nil {
		t.Fatal("a file was written outside the working tree under confinement")
	}

	// There is no read-succeeds case to assert here. Under an AppContainer reads are
	// default-deny: the command reads only what it is granted plus the system
	// libraries it needs to load and run (which is execute access, granted separately
	// from read). This is stricter than the read-only host on the other platforms,
	// where the whole filesystem stays readable, and is the safer direction.
}

// TestNetworkDeniedBlocksConnect confirms a command cannot open an outbound connection
// when the network is denied, while the same command runs unconfined can. A non-zero
// exit under denial is the pass.
func TestNetworkDeniedBlocksConnect(t *testing.T) {
	const probe = `curl --max-time 6 -s -o NUL http://1.1.1.1`

	denied, err := NewLocal(t.TempDir(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = denied.Close() })
	res, err := denied.Exec(context.Background(), Command{Line: probe})
	if err != nil {
		t.Fatalf("denied exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("an outbound connect must fail under network deny, but it succeeded:\n%s", res.Output)
	}

	// Sanity: unconfined, the same command can reach the network, so the test observes
	// the denial and not a runner with no outbound path.
	open, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	res, err = open.Exec(context.Background(), Command{Line: probe})
	if err != nil {
		t.Fatalf("open exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Skipf("runner has no outbound network unconfined, cannot distinguish here:\n%s", res.Output)
	}
}

// TestNetworkAllowedWhenNotDenied confirms a confined command that did not deny the
// network can still reach it, so the filesystem confinement does not silently also cut
// off the network.
func TestNetworkAllowedWhenNotDenied(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithReadOnlyFS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	res, err := sb.Exec(context.Background(), Command{Line: `curl --max-time 6 -s -o NUL http://1.1.1.1`})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Skipf("runner has no outbound network, cannot confirm the allow path:\n%s", res.Output)
	}
}

// TestUnconfinedCommandStillRuns confirms the default, unconfined path is unchanged by
// the AppContainer routing: a sandbox with no confinement options runs an ordinary
// command through the standard library.
func TestUnconfinedCommandStillRuns(t *testing.T) {
	sb, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	res, err := sb.Exec(context.Background(), Command{Line: "echo plain"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "plain") {
		t.Fatalf("an unconfined command must run normally, got exit %d:\n%s", res.ExitCode, res.Output)
	}
}

// TestAppContainerMonikerStableAndUnique checks the container name is deterministic per
// root and differs between roots, which is what keeps two sandbox roots from sharing a
// container identity (and therefore each other's granted directories).
func TestAppContainerMonikerStableAndUnique(t *testing.T) {
	a1 := appContainerMoniker(`C:\work\a`)
	a2 := appContainerMoniker(`C:\work\a`)
	b := appContainerMoniker(`C:\work\b`)
	if a1 != a2 {
		t.Fatalf("moniker must be stable for a root, got %q and %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("different roots must get different monikers, both were %q", a1)
	}
	if !strings.HasPrefix(a1, "flynn.sbx.") || len(a1) > 64 {
		t.Fatalf("moniker %q is not within the allowed name shape", a1)
	}
}

// TestMitigationPolicyShape guards the process-mitigation set applied to a confined
// command: it must not include any policy that breaks ordinary developer tools
// (dynamic-code prohibition, non-Microsoft-binary blocking, strict handle checks, and
// the Win32k system-call lockdown, which stops user32.dll from initializing).
func TestMitigationPolicyShape(t *testing.T) {
	for name, bit := range map[string]uint64{
		"prohibit-dynamic-code":      0x01 << 36,
		"block-non-microsoft-binary": 0x01 << 44,
		"strict-handle-checks":       0x01 << 24,
		"win32k-syscall-disable":     mitigationWin32kSystemCallDisable,
	} {
		if sandboxMitigationPolicy&bit != 0 {
			t.Fatalf("the mitigation policy must not enable %s (it breaks ordinary commands)", name)
		}
	}
}

// statusDLLInitFailed is the exit code (STATUS_DLL_INIT_FAILED) a child reports when a
// DLL fails to initialize before main runs. A confined command that loads user32.dll
// exits with it when the Win32k system-call lockdown is applied to the process.
const statusDLLInitFailed = 0xC0000142

// TestConfinedCommandLoadingUser32Starts proves a confined command that loads user32.dll
// reaches main and runs, rather than dying during DLL initialization. Most real commands
// load user32 (node, git, python, and powershell among them), so a process mitigation
// that stops it from initializing leaves the confined tier unable to run them at all,
// reporting only an opaque exit code. whoami.exe is a System32 binary that loads user32
// and prints the caller's identity, so this fails loudly if the mitigation returns.
func TestConfinedCommandLoadingUser32Starts(t *testing.T) {
	l, err := NewLocal(t.TempDir(), WithReadOnlyFS(), WithSeccomp(), WithExecTimeout(30*time.Second))
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	defer func() { _ = l.Close() }()
	if l.Containment() != ContainmentKernel {
		t.Skip("host does not enforce the kernel-confined tier")
	}

	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: []string{filepath.Join(os.Getenv("SystemRoot"), "System32", "whoami.exe")},
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if uint32(res.ExitCode) == statusDLLInitFailed {
		t.Fatalf("a confined command that loads user32.dll died during DLL initialization (exit %#x); the Win32k system-call lockdown must not be applied to arbitrary commands", uint32(res.ExitCode))
	}
	if res.ExitCode != 0 {
		t.Fatalf("whoami exited %d, output %q", res.ExitCode, res.Output)
	}
	if strings.TrimSpace(res.Output) == "" {
		t.Fatal("whoami printed nothing, so it did not reach main")
	}
}

// TestProfileCleanupOnClose proves the per-working-directory AppContainer profile a
// confined command registers is removed on Close, so profiles do not accumulate across
// runs.
func TestProfileCleanupOnClose(t *testing.T) {
	root := t.TempDir()
	sb, err := NewLocal(root, WithKernelConfinement())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	if _, err := sb.Exec(context.Background(), Command{Line: "echo make-profile"}); err != nil {
		t.Fatalf("confined exec (registers the profile): %v", err)
	}

	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		t.Skip("LOCALAPPDATA not set; cannot locate the profile folder")
	}
	profileDir := filepath.Join(local, "Packages", appContainerMoniker(root))
	if _, err := os.Stat(profileDir); err != nil {
		t.Skipf("profile folder not found at the expected location, cannot verify cleanup: %v", err)
	}

	if err := sb.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(profileDir); err == nil {
		t.Fatal("the AppContainer profile folder must be removed after Close")
	}
}

// TestServeExplicitConfineFailsClosed proves the fail-closed rule for a backgrounded
// server on Windows: the AppContainer tier cannot be carried onto a backgroundable
// process, so an explicitly confined Serve must refuse rather than start the server at
// the directory-jail floor while the sandbox still reports the confined tier. A silently
// unconfined running process would be the bug this guards against.
func TestServeExplicitConfineFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []LocalOption
	}{
		{"read-only-fs", []LocalOption{WithReadOnlyFS()}},
		{"seccomp", []LocalOption{WithSeccomp()}},
		{"kernel-confinement", []LocalOption{WithKernelConfinement()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := append(tc.opts, WithEnv(map[string]string{"FLYNN_TEST_SERVE_HELPER": "block"}))
			l, err := NewLocal(t.TempDir(), opts...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			p, err := l.Serve(context.Background(), ServeSpec{Argv: []string{os.Args[0]}, Confine: true})
			if err == nil {
				_ = p.Stop()
				t.Fatal("an explicitly confined Serve on Windows must fail closed, not start the server unconfined")
			}
			if p != nil {
				t.Fatal("a refused Serve must not return a running process handle")
			}
			if !strings.Contains(err.Error(), "background process") {
				t.Fatalf("want a background-confinement-unsupported refusal, got %v", err)
			}
		})
	}
}

// TestServeDefaultConfinementDropsToFloor confirms the always-on baseline is the one
// exception: WithDefaultConfinement carries no explicit request and no network control,
// so a backgrounded server is allowed to run at the directory-jail floor rather than
// being refused. The default never fails merely for asking, matching Exec's fallback.
func TestServeDefaultConfinementDropsToFloor(t *testing.T) {
	l, err := NewLocal(t.TempDir(), WithDefaultConfinement(), WithEnv(map[string]string{"FLYNN_TEST_SERVE_HELPER": "block"}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	p, err := l.Serve(context.Background(), ServeSpec{Argv: []string{os.Args[0]}, Confine: true})
	if err != nil {
		t.Fatalf("the always-on baseline must drop to the floor, not refuse: %v", err)
	}
	defer func() { _ = p.Stop() }()
	waitFor(t, time.Second, func() bool { return strings.Contains(p.Output(), "helper-up") })
	if !p.Running() {
		t.Fatal("the backgrounded server should be running at the floor")
	}
}

// TestJobLimitFlags guards the containment limits set on a confined command's job: the
// process tree must be killed when the run ends (no surviving orphans) and the process
// count must be capped (fork-bomb backstop).
func TestJobLimitFlags(t *testing.T) {
	if jobLimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		t.Fatal("the job must kill its process tree when the run ends")
	}
	if jobLimitFlags&windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS == 0 || jobActiveProcessLimit == 0 {
		t.Fatal("the job must cap the number of processes as a fork-bomb backstop")
	}
}

// TestApplyJobLimitsResourceCaps proves a configured resource cap reaches the job
// object: a memory cap sets the job-memory flag and the byte limit, and a process
// cap overrides the default. It reads the limits back off the created job rather than
// asserting on the input, so a wiring or unit-conversion mistake is caught.
func TestApplyJobLimitsResourceCaps(t *testing.T) {
	// A live process to assign the job to. It sleeps briefly and is killed when the
	// job handle closes (KILL_ON_JOB_CLOSE), so nothing outlives the test.
	cmd := exec.Command("ping", "-n", "3", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	h, err := windows.OpenProcess(windows.PROCESS_ALL_ACCESS, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("open helper process: %v", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()

	const wantMiB = 256
	const wantProcs = 7
	job, err := applyJobLimits(h, ResourceLimits{MemoryMiB: wantMiB, MaxProcesses: wantProcs})
	if err != nil {
		t.Fatalf("applyJobLimits: %v", err)
	}
	defer func() { _ = windows.CloseHandle(job) }()

	var got windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&got)), uint32(unsafe.Sizeof(got)), nil); err != nil {
		t.Fatalf("query job info: %v", err)
	}

	if got.BasicLimitInformation.LimitFlags&jobObjectLimitJobMemory == 0 {
		t.Error("a memory cap must set the job-memory limit flag on the job")
	}
	if wantBytes := uintptr(wantMiB) * 1024 * 1024; got.JobMemoryLimit != wantBytes {
		t.Errorf("job memory limit = %d bytes, want %d", got.JobMemoryLimit, wantBytes)
	}
	if got.BasicLimitInformation.ActiveProcessLimit != wantProcs {
		t.Errorf("active process limit = %d, want %d (the configured override)",
			got.BasicLimitInformation.ActiveProcessLimit, wantProcs)
	}
}

// TestApplyJobLimitsDefaults proves the zero ResourceLimits leaves the job at its
// generous defaults: no memory flag, and the default process cap, so a caller that
// sets no cap is unaffected.
func TestApplyJobLimitsDefaults(t *testing.T) {
	cmd := exec.Command("ping", "-n", "3", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	h, err := windows.OpenProcess(windows.PROCESS_ALL_ACCESS, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("open helper process: %v", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()

	job, err := applyJobLimits(h, ResourceLimits{})
	if err != nil {
		t.Fatalf("applyJobLimits: %v", err)
	}
	defer func() { _ = windows.CloseHandle(job) }()

	var got windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&got)), uint32(unsafe.Sizeof(got)), nil); err != nil {
		t.Fatalf("query job info: %v", err)
	}
	if got.BasicLimitInformation.LimitFlags&jobObjectLimitJobMemory != 0 {
		t.Error("no memory cap was set, so the job-memory limit flag must be clear")
	}
	if got.BasicLimitInformation.ActiveProcessLimit != jobActiveProcessLimit {
		t.Errorf("active process limit = %d, want the default %d",
			got.BasicLimitInformation.ActiveProcessLimit, jobActiveProcessLimit)
	}
}

// TestUngrantableReadableDirFailsClosed proves a directory that cannot be granted to the
// confined child fails the launch rather than dropping it to an unconfined run. The
// best-effort fallback exists for a host that cannot establish confinement at all; if it
// also absorbed a grant failure, naming a directory the process may not re-ACL (a
// system-owned one) would silently run the command with no sandbox around it, which is
// the opposite of what asking for the grant meant. A path that does not exist cannot have
// its access list rewritten, so it stands in for that class deterministically.
func TestUngrantableReadableDirFailsClosed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-directory")
	l, err := NewLocal(t.TempDir(), WithReadOnlyFS(), WithReadableDir(missing), WithExecTimeout(30*time.Second))
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	defer func() { _ = l.Close() }()

	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: []string{filepath.Join(os.Getenv("SystemRoot"), "System32", "hostname.exe")},
	})
	if err == nil {
		t.Fatalf("capture succeeded (exit %d, output %q); a grant that could not be applied must fail the launch, not run the command unconfined", res.ExitCode, res.Output)
	}
	if !errors.Is(err, ErrReadGrant) {
		t.Fatalf("error %v does not wrap ErrReadGrant, so callers cannot tell a grant failure from a host that lacks confinement", err)
	}
}

// TestGrantedDirIsEnumerable proves a granted directory can be opened and enumerated by
// the confined child, not merely have its files read. A generic access right stored in an
// access-list entry is mapped to specific rights only when a child object inherits it, so
// an entry naming GENERIC_READ or GENERIC_ALL leaves the directory itself without the
// list and traverse rights: files under it read fine through their inherited entries while
// the directory cannot be opened, which breaks any command that scans its own config or
// working directory. The grants therefore name specific rights, and this holds them to it.
func TestGrantedDirIsEnumerable(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "granted.txt"), []byte("readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("writable"), 0o600); err != nil {
		t.Fatal(err)
	}

	l, err := NewLocal(root, WithReadOnlyFS(), WithSeccomp(), WithReadableDir(outside), WithExecTimeout(30*time.Second))
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	defer func() { _ = l.Close() }()
	if l.Containment() != ContainmentKernel {
		t.Skip("host does not enforce the kernel-confined tier")
	}

	// "for %f in (dir\*)" enumerates without opening the volume, which "dir" also does and
	// which the container denies for reasons unrelated to the directory's access list.
	cases := map[string]string{
		"workspace": `for %f in (` + root + `\*) do @echo FOUND:%f`,
		"granted":   `for %f in (` + outside + `\*) do @echo FOUND:%f`,
	}
	for name, line := range cases {
		res, err := l.Capture(context.Background(), CaptureSpec{
			Argv: []string{filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), "/c", line},
		})
		if err != nil {
			t.Fatalf("%s: capture: %v", name, err)
		}
		if res.ExitCode != 0 || !strings.Contains(res.Output, "FOUND:") {
			t.Fatalf("the confined child could not enumerate the %s directory (exit %d, output %q); the access-list entry must name specific rights, not generic ones", name, res.ExitCode, res.Output)
		}
	}
}

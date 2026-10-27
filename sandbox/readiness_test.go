package sandbox

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// fakeProc is a /proc with only the named files readable, so the classification runs
// against any kernel's settings on any OS.
type fakeProc map[string]string

func (f fakeProc) read(path string) (string, bool) {
	v, ok := f[path]
	return v, ok
}

const (
	statusFiltered   = "Name:\tflynn\nSeccomp:\t2\nSeccomp_filters:\t1\n"
	statusUnfiltered = "Name:\tflynn\nSeccomp:\t0\n"
)

func TestClassify(t *testing.T) {
	healthy := fakeProc{
		procMaxUserNS:   "15000\n",
		procCloneSysctl: "1\n",
		procSelfStatus:  statusUnfiltered,
	}
	with := func(base fakeProc, kv ...string) fakeProc {
		out := fakeProc{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	launcher := func(msg string) failure {
		return failure{stage: stageLauncher, raw: "exit status 126", output: "sandbox: confinement launcher: " + msg + "\n"}
	}

	tests := []struct {
		name      string
		f         failure
		proc      fakeProc
		cause     Cause
		also      []Cause
		fixHas    string
		detailHas string
	}{
		{
			name:      "RestrictNamespaces=true blocks the user namespace",
			f:         failure{stage: stageUser, errno: syscall.EPERM, raw: "fork/exec /bin/sh: operation not permitted"},
			proc:      with(healthy, procSelfStatus, statusFiltered),
			cause:     CauseNamespaceRestricted,
			fixHas:    "RestrictNamespaces=user net mnt",
			detailHas: "seccomp filter active",
		},
		{
			name:   "RestrictNamespaces with the AppArmor sysctl on names the unit and lists AppArmor",
			f:      failure{stage: stageUser, errno: syscall.EPERM, raw: "operation not permitted"},
			proc:   with(healthy, procSelfStatus, statusFiltered, procAppArmor, "1\n"),
			cause:  CauseNamespaceRestricted,
			also:   []Cause{CauseAppArmorUserns},
			fixHas: "RestrictNamespaces",
		},
		{
			name:   "RestrictNamespaces=user lets the user namespace through and blocks net",
			f:      failure{stage: stageNet, errno: syscall.EPERM, raw: "operation not permitted"},
			proc:   with(healthy, procSelfStatus, statusFiltered),
			cause:  CauseNamespaceRestricted,
			fixHas: "RestrictNamespaces=user net mnt",
		},
		{
			name:   "RestrictNamespaces blocks the mount namespace with EACCES",
			f:      failure{stage: stageMount, errno: syscall.EACCES, raw: "permission denied"},
			proc:   with(healthy, procSelfStatus, statusFiltered),
			cause:  CauseNamespaceRestricted,
			fixHas: "daemon-reload",
		},
		{
			// Measured on Docker 29 with its default profile: the user namespace is refused
			// with EPERM under the runtime's filter.
			name:      "Docker's default seccomp profile blocks the user namespace",
			f:         failure{stage: stageUser, errno: syscall.EPERM, raw: "operation not permitted"},
			proc:      with(healthy, procSelfStatus, statusFiltered, markerDocker, ""),
			cause:     CauseNamespaceRestricted,
			fixHas:    "deploy/seccomp/flynn.json",
			detailHas: "running in a container",
		},
		{
			name:   "Podman's filter blocks the network namespace",
			f:      failure{stage: stageNet, errno: syscall.EPERM, raw: "operation not permitted"},
			proc:   with(healthy, procSelfStatus, statusFiltered, markerPodman, "engine=\"podman\"\n"),
			cause:  CauseNamespaceRestricted,
			fixHas: "--security-opt seccomp=",
		},
		{
			name:   "a container's filter stops the launcher installing its own",
			f:      launcher("install seccomp filter: operation not permitted"),
			proc:   with(healthy, procSelfStatus, statusFiltered, markerDocker, ""),
			cause:  CauseSeccompConflict,
			fixHas: "deploy/seccomp/flynn.json",
		},
		{
			name:   "a container's filter kills the child with SIGSYS",
			f:      failure{stage: stageLauncher, signal: true},
			proc:   with(healthy, procSelfStatus, statusFiltered, markerDocker, ""),
			cause:  CauseSeccompConflict,
			fixHas: "Localhost seccompProfile",
		},
		{
			// Measured on Ubuntu 24.04 with the shipped seccomp profile and docker-default
			// AppArmor: the namespaces start and the launcher's first mount is refused.
			name:   "docker-default AppArmor refuses the launcher's mount",
			f:      launcher("make mounts private: permission denied"),
			proc:   with(healthy, procSelfStatus, statusFiltered, procAppArmor, "1\n", markerDocker, ""),
			cause:  CauseAppArmorUserns,
			also:   []Cause{CauseSeccompConflict},
			fixHas: "--security-opt apparmor=flynn-container",
		},
		{
			name:      "user.max_user_namespaces=0",
			f:         failure{stage: stageUser, errno: syscall.ENOSPC, raw: "no space left on device"},
			proc:      with(healthy, procMaxUserNS, "0\n"),
			cause:     CauseUserNamespacesDisabled,
			fixHas:    "user.max_user_namespaces",
			detailHas: "user.max_user_namespaces=0",
		},
		{
			name:      "kernel.unprivileged_userns_clone=0 (Debian)",
			f:         failure{stage: stageUser, errno: syscall.EPERM, raw: "operation not permitted"},
			proc:      with(healthy, procCloneSysctl, "0\n"),
			cause:     CauseUserNamespacesDisabled,
			fixHas:    "kernel.unprivileged_userns_clone=1",
			detailHas: "kernel.unprivileged_userns_clone=0",
		},
		{
			name:   "the sysctl names the cause even when the errno is not the usual one",
			f:      failure{stage: stageUser, errno: syscall.EINVAL, raw: "invalid argument"},
			proc:   with(healthy, procMaxUserNS, "0\n"),
			cause:  CauseUserNamespacesDisabled,
			fixHas: "user.max_user_namespaces",
		},
		{
			name:   "user namespace limit exhausted with the sysctl non-zero",
			f:      failure{stage: stageUser, errno: syscall.ENOSPC, raw: "no space left on device"},
			proc:   healthy,
			cause:  CauseUserNamespacesDisabled,
			fixHas: "raise user.max_user_namespaces",
		},
		{
			name:      "Ubuntu 24.04 AppArmor restriction denies the network namespace",
			f:         failure{stage: stageNet, errno: syscall.EPERM, raw: "operation not permitted"},
			proc:      with(healthy, procAppArmor, "1\n"),
			cause:     CauseAppArmorUserns,
			fixHas:    "AppArmor profile",
			detailHas: "kernel.apparmor_restrict_unprivileged_userns=1",
		},
		{
			name:   "AppArmor restriction denies the launcher's mount",
			f:      launcher("make mounts private: operation not permitted"),
			proc:   with(healthy, procAppArmor, "1\n"),
			cause:  CauseAppArmorUserns,
			fixHas: "userns",
		},
		{
			name:   "AppArmor with a filter active lists the inherited filter as the other candidate",
			f:      launcher("make mounts private: operation not permitted"),
			proc:   with(healthy, procAppArmor, "1\n", procSelfStatus, statusFiltered),
			cause:  CauseAppArmorUserns,
			also:   []Cause{CauseSeccompConflict},
			fixHas: "userns",
		},
		{
			name:   "the launcher cannot install the sandbox's own filter",
			f:      launcher("install seccomp filter: operation not permitted"),
			proc:   healthy,
			cause:  CauseSeccompConflict,
			fixHas: "SystemCallFilter=mount",
		},
		{
			name:   "the launcher cannot set no-new-privs",
			f:      launcher("set no-new-privs: invalid argument"),
			proc:   healthy,
			cause:  CauseSeccompConflict,
			fixHas: "SystemCallErrorNumber=EPERM",
		},
		{
			name:   "the unit's filter blocks mount",
			f:      launcher("make mounts private: operation not permitted"),
			proc:   with(healthy, procSelfStatus, statusFiltered),
			cause:  CauseSeccompConflict,
			fixHas: "SystemCallFilter=mount",
		},
		{
			name:      "a blocked call that kills the child",
			f:         failure{stage: stageLauncher, signal: true, raw: "signal: bad system call"},
			proc:      healthy,
			cause:     CauseSeccompConflict,
			fixHas:    "SystemCallErrorNumber=EPERM",
			detailHas: "SIGSYS",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.f, tc.proc.read)
			if got.Ready {
				t.Fatalf("classify returned Ready for a failure: %+v", got)
			}
			if got.Cause != tc.cause {
				t.Fatalf("cause = %q, want %q\n%+v", got.Cause, tc.cause, got)
			}
			if len(got.Also) != len(tc.also) {
				t.Fatalf("also = %v, want %v", got.Also, tc.also)
			}
			for i := range tc.also {
				if got.Also[i] != tc.also[i] {
					t.Fatalf("also = %v, want %v", got.Also, tc.also)
				}
			}
			if got.Stage != string(tc.f.stage) {
				t.Errorf("stage = %q, want %q", got.Stage, tc.f.stage)
			}
			if !strings.Contains(got.Fix, tc.fixHas) {
				t.Errorf("fix %q does not contain %q", got.Fix, tc.fixHas)
			}
			if tc.detailHas != "" && !strings.Contains(got.Detail, tc.detailHas) {
				t.Errorf("detail %q does not contain %q", got.Detail, tc.detailHas)
			}
			assertPointsToStrongerTiers(t, got)
		})
	}
}

// assertPointsToStrongerTiers holds every refusal to naming both alternatives, since the
// point of refusing is that the operator has somewhere to go that is not unconfined.
func assertPointsToStrongerTiers(t *testing.T, r Readiness) {
	t.Helper()
	for _, want := range []string{"container", "microVM"} {
		if !strings.Contains(r.Alternative, want) {
			t.Errorf("alternative %q does not name the %s tier", r.Alternative, want)
		}
	}
	if r.Fix == "" || r.Detail == "" {
		t.Errorf("a refusal must carry a fix and its evidence: %+v", r)
	}
}

// TestClassifyUnrecognisedFailureRefuses is the fail-closed guarantee: whatever the
// errno, signal, output or readable /proc, the classifier never returns Ready, and a
// failure it cannot name is CauseUnknown rather than a pass.
func TestClassifyUnrecognisedFailureRefuses(t *testing.T) {
	errnos := []syscall.Errno{0, syscall.ENOENT, syscall.EIO, syscall.ENOMEM, syscall.EAGAIN, syscall.EPERM, syscall.EACCES, syscall.EINVAL, syscall.ENOSPC}
	stages := []stage{stageUser, stageNet, stageMount, stageLauncher, stage("a stage nobody defined")}
	procs := []fakeProc{
		nil,
		{},
		{procSelfStatus: statusFiltered},
		{procMaxUserNS: "0"},
		{procCloneSysctl: "0"},
		{procAppArmor: "1"},
		{procMaxUserNS: "garbage", procCloneSysctl: "", procAppArmor: "\x00", procSelfStatus: "Seccomp:\tx"},
	}
	outputs := []string{"", "sandbox: confinement launcher: something new", "operation not permitted"}
	count := 0
	for _, s := range stages {
		for _, e := range errnos {
			for _, sig := range []bool{false, true} {
				for _, out := range outputs {
					for _, p := range procs {
						f := failure{stage: s, errno: e, signal: sig, output: out}
						got := classify(f, p.read)
						count++
						if got.Ready || got.Cause == "" || got.Fix == "" || got.Alternative == "" {
							t.Fatalf("classify(%+v, %v) = %+v, want a classified refusal", f, p, got)
						}
					}
				}
			}
		}
	}
	if count == 0 {
		t.Fatal("no combinations ran")
	}
}

func TestClassifyUnknownErrnoIsUnknownCause(t *testing.T) {
	for _, e := range []syscall.Errno{0, syscall.ENOENT, syscall.EIO, syscall.ENOMEM} {
		for _, s := range []stage{stageUser, stageNet, stageMount, stageLauncher} {
			f := failure{stage: s, errno: e, raw: "something unexpected"}
			got := classify(f, fakeProc{procSelfStatus: statusFiltered, procAppArmor: "1"}.read)
			if got.Cause != CauseUnknown || got.Ready {
				t.Errorf("stage %s errno %v: got %+v, want a refusal with CauseUnknown", s, e, got)
			}
		}
	}
}

func TestClassifyPermissionDeniedWithNoEvidenceIsUnknown(t *testing.T) {
	// EPERM with nothing readable cannot be pinned on the unit, AppArmor or a sysctl; it
	// must refuse as unknown rather than guess a fix.
	for _, s := range []stage{stageUser, stageNet, stageMount, stageLauncher} {
		got := classify(failure{stage: s, errno: syscall.EPERM, raw: "operation not permitted"}, nil)
		if got.Ready || got.Cause != CauseUnknown {
			t.Errorf("stage %s: got %+v, want a refusal with CauseUnknown", s, got)
		}
	}
}

func TestErrnoFromText(t *testing.T) {
	tests := map[string]syscall.Errno{
		"sandbox: confinement launcher: make mounts private: Operation not permitted": syscall.EPERM,
		"permission denied":       syscall.EACCES,
		"no space left on device": syscall.ENOSPC,
		"invalid argument":        syscall.EINVAL,
		"no such file":            0,
		"":                        0,
	}
	for in, want := range tests {
		if got := errnoFromText(in); got != want {
			t.Errorf("errnoFromText(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStagesFollowNeeds(t *testing.T) {
	tests := []struct {
		n    needs
		want []stage
	}{
		{needs{}, []stage{stageUser}},
		{needs{net: true}, []stage{stageUser, stageNet, stageCanary}},
		{needs{mount: true}, []stage{stageUser, stageMount, stageLauncher, stageCanary}},
		{needs{seccomp: true}, []stage{stageUser, stageLauncher}},
		{needsAll, []stage{stageUser, stageNet, stageMount, stageLauncher, stageCanary}},
	}
	for _, tc := range tests {
		got := tc.n.stages()
		if len(got) != len(tc.want) {
			t.Fatalf("%+v: stages = %v, want %v", tc.n, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%+v: stages = %v, want %v", tc.n, got, tc.want)
			}
		}
	}
}

func TestCheckPassesOnlyWhenEveryStageRuns(t *testing.T) {
	var ran []stage
	p := probe{
		run: func(_ context.Context, s stage, _ needs) *failure {
			ran = append(ran, s)
			return nil
		},
		proc: fakeProc{}.read,
	}
	got := check(context.Background(), needsAll, p)
	if !got.Ready || got.Err() != nil {
		t.Fatalf("every stage passed, got %+v", got)
	}
	if len(ran) != 5 {
		t.Fatalf("ran %v, want all five stages", ran)
	}
}

func TestCheckStopsAtTheFirstFailingStage(t *testing.T) {
	var ran []stage
	p := probe{
		run: func(_ context.Context, s stage, _ needs) *failure {
			ran = append(ran, s)
			if s == stageNet {
				return &failure{stage: s, errno: syscall.EPERM, raw: "operation not permitted"}
			}
			return nil
		},
		proc: fakeProc{procAppArmor: "1"}.read,
	}
	got := check(context.Background(), needsAll, p)
	if got.Ready || got.Cause != CauseAppArmorUserns || got.Stage != string(stageNet) {
		t.Fatalf("got %+v, want an AppArmor refusal at the network namespace", got)
	}
	if len(ran) != 2 {
		t.Fatalf("ran %v, want to stop after the failing stage", ran)
	}
}

func TestCheckRefusesAFailureWithNoErrno(t *testing.T) {
	// A child that failed without leaving an errno or a message is the case a lenient
	// check would wave through.
	p := probe{
		run:  func(_ context.Context, s stage, _ needs) *failure { return &failure{stage: s} },
		proc: fakeProc{}.read,
	}
	got := check(context.Background(), needsAll, p)
	if got.Ready || got.Cause != CauseUnknown {
		t.Fatalf("got %+v, want an unknown refusal", got)
	}
}

// TestCheckRecordsArchitectureAndWarnsWhereRestrictNamespacesIsInert holds the caveat a
// pass carries: on an architecture where systemd ignores RestrictNamespaces=, the result
// says so, and a refusal keeps its warning too.
func TestCheckRecordsArchitectureAndWarnsWhereRestrictNamespacesIsInert(t *testing.T) {
	pass := func(context.Context, stage, needs) *failure { return nil }
	fail := func(_ context.Context, s stage, _ needs) *failure {
		return &failure{stage: s, errno: syscall.EPERM, raw: "operation not permitted"}
	}
	for _, tc := range []struct {
		arch     string
		run      func(context.Context, stage, needs) *failure
		wantWarn bool
	}{
		{"amd64", pass, false},
		{"386", pass, false},
		{"ppc64le", pass, false},
		{"s390x", pass, false},
		{"arm64", pass, true},
		{"riscv64", pass, true},
		{"mips64le", pass, true},
		{"arm64", fail, true},
		{"amd64", fail, false},
	} {
		got := check(context.Background(), needsAll, probe{run: tc.run, proc: fakeProc{}.read, arch: tc.arch})
		if got.Arch != tc.arch {
			t.Errorf("%s: Arch = %q", tc.arch, got.Arch)
		}
		if has := len(got.Warnings) > 0; has != tc.wantWarn {
			t.Errorf("%s ready=%v: warnings = %v, want warning %v", tc.arch, got.Ready, got.Warnings, tc.wantWarn)
		}
		if tc.wantWarn && !strings.Contains(got.Warnings[0], "RestrictNamespaces") {
			t.Errorf("%s: warning %q does not name the directive", tc.arch, got.Warnings[0])
		}
	}
}

// TestAppArmorFixPointsAtTheShippedProfileAndNeverTheGlobalSysctl holds the advice to the
// narrow remedy: the host-wide switch weakens every program, so the text must not tell an
// operator to flip it.
func TestAppArmorFixPointsAtTheShippedProfileAndNeverTheGlobalSysctl(t *testing.T) {
	for _, want := range []string{"deploy/apparmor/flynn", "apparmor_parser -r", "Do not set kernel.apparmor_restrict_unprivileged_userns=0"} {
		if !strings.Contains(fixAppArmor, want) {
			t.Errorf("fixAppArmor does not contain %q", want)
		}
	}
	for _, fix := range []string{fixNamespaces, fixSeccomp, fixContainer, fixUnknown, fixPlatform, (evidence{}).userNSFix()} {
		if strings.Contains(fix, "apparmor_restrict_unprivileged_userns=0") {
			t.Errorf("fix %q recommends the global AppArmor switch", fix)
		}
	}
}

// TestContainerFixNamesAProfileThatShips holds the container fix to a file the repository
// carries, and to warning off the two broad switches an operator reaches for first.
func TestContainerFixNamesAProfileThatShips(t *testing.T) {
	const profile = "deploy/seccomp/flynn.json"
	for _, p := range []string{profile, "deploy/apparmor/flynn-container"} {
		if !strings.Contains(fixContainer, p) {
			t.Errorf("fixContainer does not name %s", p)
		}
		if _, err := os.Stat(filepath.Join("..", filepath.FromSlash(p))); err != nil {
			t.Errorf("fixContainer names %s, which is not in the repository: %v", p, err)
		}
	}
	for _, want := range []string{"Do not use seccomp=unconfined or apparmor=unconfined", "do not add CAP_SYS_ADMIN"} {
		if !strings.Contains(fixContainer, want) {
			t.Errorf("fixContainer does not contain %q", want)
		}
	}
	for _, fix := range []string{fixNamespaces, fixSeccomp} {
		if strings.Contains(fix, profile) {
			t.Errorf("the service unit fix %q names the container profile", fix)
		}
	}
}

func TestReadinessZeroValueRefuses(t *testing.T) {
	var r Readiness
	err := r.Err()
	if err == nil {
		t.Fatal("the zero Readiness must refuse")
	}
	if !errors.Is(err, ErrConfinementRefused) {
		t.Fatalf("err = %v, want it to match ErrConfinementRefused", err)
	}
	if (Readiness{Ready: true}).Err() != nil {
		t.Fatal("a ready result must not produce an error")
	}
}

func TestRefusedErrorReadsAsCauseFixAndAlternative(t *testing.T) {
	r := classify(
		failure{stage: stageUser, errno: syscall.EPERM, raw: "operation not permitted"},
		fakeProc{procSelfStatus: statusFiltered}.read,
	)
	err := r.Err()
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %T, want *RefusedError", err)
	}
	if refused.Readiness.Cause != CauseNamespaceRestricted {
		t.Fatalf("cause = %q", refused.Readiness.Cause)
	}
	msg := err.Error()
	for _, want := range []string{string(CauseNamespaceRestricted), "RestrictNamespaces=user net mnt", "container tier", "microVM tier"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

func TestRequiredListsAreCopies(t *testing.T) {
	ns := RequiredNamespaces()
	ns[0] = "tampered"
	if RequiredNamespaces()[0] != "user" {
		t.Fatal("RequiredNamespaces returned shared storage")
	}
	sc := RequiredSyscalls()
	sc[0] = "tampered"
	if RequiredSyscalls()[0] != "mount" {
		t.Fatal("RequiredSyscalls returned shared storage")
	}
}

func TestCheckConfinementOffLinuxIsUnsupported(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("covered by the Linux host test")
	}
	got := CheckConfinement(context.Background())
	if got.Ready || got.Cause != CauseUnsupportedPlatform {
		t.Fatalf("got %+v, want an unsupported-platform refusal", got)
	}
	assertPointsToStrongerTiers(t, got)
}

// TestReadinessCodeReadsNoEnvironment holds the guarantee that nothing in the
// environment can turn confinement off: the check and its gate read no environment
// variable at all. A new Getenv in these files fails here, so an off switch cannot be
// added without changing this test and showing the change in review.
func TestReadinessCodeReadsNoEnvironment(t *testing.T) {
	files, err := filepath.Glob("readiness*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no readiness files found: %v", err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				return true
			}
			switch sel.Sel.Name {
			case "Getenv", "LookupEnv", "Environ", "ExpandEnv", "Expand":
				t.Errorf("%s: os.%s read in the readiness code; the check must not depend on the environment", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
}

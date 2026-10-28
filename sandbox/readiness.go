package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
)

// Cause names why a host refused to set up the kernel confinement the Linux sandbox
// needs. Each value maps to one fix an operator can apply, so a host reads the cause
// and decides between repairing the host and moving to a stronger tier.
type Cause string

// The causes [CheckConfinement] reports. Every refusal carries exactly one primary
// cause; [CauseUnknown] is the answer for anything the classifier cannot name, and it
// refuses like the rest.
const (
	// CauseNamespaceRestricted means the service unit (RestrictNamespaces=) forbids a
	// namespace type the sandbox creates.
	CauseNamespaceRestricted Cause = "namespace-restricted"
	// CauseUserNamespacesDisabled means the kernel is configured to refuse unprivileged
	// user namespaces (kernel.unprivileged_userns_clone=0 or user.max_user_namespaces=0).
	CauseUserNamespacesDisabled Cause = "user-namespaces-disabled"
	// CauseAppArmorUserns means AppArmor denies capabilities inside an unprivileged
	// user namespace (kernel.apparmor_restrict_unprivileged_userns=1, Ubuntu 24.04 and
	// later).
	CauseAppArmorUserns Cause = "apparmor-userns-restricted"
	// CauseSeccompConflict means a seccomp filter inherited from the service unit or a
	// container runtime blocks a system call the launcher needs, or the sandbox's own
	// filter could not be installed on top of it.
	CauseSeccompConflict Cause = "seccomp-conflict"
	// CauseUnsupportedPlatform means the namespace sandbox does not exist on this
	// operating system.
	CauseUnsupportedPlatform Cause = "unsupported-platform"
	// CauseConfinementIneffective means the sandbox started a child but did not confine
	// it: a command the confinement exists to stop ran to completion, or the check could
	// not tell. The host is refused whatever the configuration says.
	CauseConfinementIneffective Cause = "confinement-ineffective"
	// CauseUnknown means confinement could not be established and the reason matched
	// nothing the classifier knows. It is a refusal, never a pass.
	CauseUnknown Cause = "unknown"
)

// stage is one rung of the ladder [CheckConfinement] climbs. Each rung adds one thing
// the sandbox does, so the first rung to fail names the thing the host refused.
type stage string

const (
	stageUser     stage = "user-namespace"
	stageNet      stage = "network-namespace"
	stageMount    stage = "mount-namespace"
	stageLauncher stage = "launcher"
	stageCanary   stage = "canary"
)

// The launcher prints these on its way out when the step they name fails. They are the
// only evidence a launcher failure leaves (it exits 126 with a message, not a Go error),
// so the launcher builds its messages from the same constants the classifier matches.
const (
	msgNoNewPrivs    = "set no-new-privs"
	msgInstallFilter = "install seccomp filter"
	msgMakePrivate   = "make mounts private"
)

// The files the classifier reads to tell one refusal from another. A file the host hides
// (ProcSubset=pid, a locked-down /proc) is evidence that is absent, never evidence of a
// pass: the classifier falls through to a less specific cause and the result is still a
// refusal.
const (
	procMaxUserNS   = "/proc/sys/user/max_user_namespaces"
	procCloneSysctl = "/proc/sys/kernel/unprivileged_userns_clone"
	procAppArmor    = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
	procSelfStatus  = "/proc/self/status"

	// The files Docker and Podman create at the root of every container they start. A
	// container's refusal needs a different fix from a service unit's: the filter comes
	// from the runtime, so the answer is a runtime profile, not unit settings. Kubernetes
	// over containerd creates neither, so a pod still gets the unit-oriented text.
	markerDocker = "/.dockerenv"
	markerPodman = "/run/.containerenv"
)

// Readiness is the result of [CheckConfinement]: whether this host will run a command
// under the Linux sandbox's kernel confinement, and if not, which refusal it is and what
// to do about it. An embedding host reads it to gate its own readiness reporting.
//
// Ready is the only pass signal. The zero value is not ready, so a Readiness that was
// never computed refuses.
type Readiness struct {
	// Ready is true only when every stage of the check ran a confined no-op child to
	// completion.
	Ready bool `json:"ready"`
	// Cause is the primary reason for a refusal; empty when Ready.
	Cause Cause `json:"cause,omitempty"`
	// Also lists other causes the same evidence is consistent with, when two refusals
	// produce the same error (AppArmor and RestrictNamespaces both return EPERM).
	Also []Cause `json:"also,omitempty"`
	// Stage is the rung that failed: "user-namespace", "network-namespace",
	// "mount-namespace" or "launcher". Empty when Ready.
	Stage string `json:"stage,omitempty"`
	// Detail is the evidence: the stage's error and the sysctl values that decided the
	// classification. It is for a log or an operator, not for matching.
	Detail string `json:"detail,omitempty"`
	// Fix says how to repair the host so confinement works. Empty when Ready.
	Fix string `json:"fix,omitempty"`
	// Alternative names the stronger tiers to use where the host cannot or should not
	// be changed. Empty when Ready.
	Alternative string `json:"alternative,omitempty"`
	// Arch is the architecture the check ran on (a GOARCH value). systemd enforces
	// RestrictNamespaces= on only some architectures, so a unit's namespace list means
	// nothing on the others; the result records where it was measured.
	Arch string `json:"arch,omitempty"`
	// Tier is the confinement tier the probe measured, in the names
	// [Local.ConfinementTier] reports: "namespace-seccomp" when every rung of the check ran
	// a confined child, and "process-jail" when one did not. It records what the host
	// delivered, so a record of a run cannot claim the tier the configuration asked for.
	Tier string `json:"tier,omitempty"`
	// Warnings are facts about the host that do not refuse the launch but change what a
	// pass proves, such as an architecture where the unit's namespace restriction is not
	// enforced.
	Warnings []string `json:"warnings,omitempty"`
}

// ErrConfinementRefused is matched, with [errors.Is], by every error that reports a host
// refusing kernel confinement. A launch that gets it must not retry unconfined.
var ErrConfinementRefused = errors.New("sandbox: kernel confinement refused")

// RefusedError is the error a launch returns when the host refuses confinement. It
// carries the [Readiness] so a caller can read the cause and fix, and it matches
// [ErrConfinementRefused].
type RefusedError struct {
	Readiness Readiness
}

// Error renders the cause, the evidence, the fix and the alternative on one line, so a
// log that records only the error string still says why and what to do.
func (e *RefusedError) Error() string {
	r := e.Readiness
	cause := string(r.Cause)
	if cause == "" {
		cause = string(CauseUnknown)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", ErrConfinementRefused.Error(), cause)
	if r.Detail != "" {
		fmt.Fprintf(&b, " (%s)", r.Detail)
	}
	if r.Fix != "" {
		fmt.Fprintf(&b, ". Fix: %s", r.Fix)
	}
	if r.Alternative != "" {
		fmt.Fprintf(&b, ". Alternative: %s", r.Alternative)
	}
	return b.String()
}

// Is reports whether target is [ErrConfinementRefused].
func (e *RefusedError) Is(target error) bool { return target == ErrConfinementRefused }

// Err returns nil when the host is ready and a [*RefusedError] otherwise, including for
// the zero value.
func (r Readiness) Err() error {
	if r.Ready {
		return nil
	}
	return &RefusedError{Readiness: r}
}

// RequiredNamespaces lists the namespace types the Linux sandbox creates, named as
// systemd's RestrictNamespaces= names them. A service unit that restricts namespaces
// must allow exactly these; a test holds the shipped runner unit to this list and holds
// this list to the clone flags the sandbox sets.
func RequiredNamespaces() []string { return []string{"user", "net", "mnt"} }

// RequiredSyscalls lists the system calls the sandbox makes that a hardened service
// unit's usual syscall filter (@system-service without @privileged) removes, named as
// systemd's SystemCallFilter= names them. The sandbox's launcher mounts inside its new
// mount namespace; nothing else it does falls outside the ordinary service set.
func RequiredSyscalls() []string { return []string{"mount"} }

// needs is the part of the sandbox a Local is configured to use, which decides how far
// up the ladder a host has to get. The launcher stage runs when the command is
// re-executed through the launcher, which is whenever the mount view or the syscall
// filter is requested.
type needs struct {
	net     bool
	mount   bool
	seccomp bool
}

// needsAll is every part of the sandbox the agent runner uses.
var needsAll = needs{net: true, mount: true, seccomp: true}

// stages returns the ladder for these needs, lowest rung first.
func (n needs) stages() []stage {
	out := []stage{stageUser}
	if n.net {
		out = append(out, stageNet)
	}
	if n.mount {
		out = append(out, stageMount)
	}
	if n.mount || n.seccomp {
		out = append(out, stageLauncher)
	}
	if n.net || n.mount {
		out = append(out, stageCanary)
	}
	return out
}

// The tier names a [Readiness] and [Local.ConfinementTier] share.
const (
	tierNamespaceSeccomp = "namespace-seccomp"
	tierFloor            = "process-jail"
)

// failure is what one rung reports when it did not run its confined no-op child to a
// clean exit. A rung that fails always produces one, whatever went wrong; there is no
// "failed but unremarkable" case, which is what makes an unrecognised error a refusal.
type failure struct {
	stage   stage
	errno   syscall.Errno // the start error's errno, zero when the child started
	signal  bool          // the child was killed by SIGSYS
	escaped bool          // a canary command the confinement should stop ran to completion
	output  string        // what the launcher or child printed
	raw     string        // the error text, for the detail
}

// procReader reads one of the fixed files the classifier names (/proc files and the
// container marker files), reporting false when it cannot be read. Injecting it lets
// the classification run against any kernel's sysctls on any OS.
type procReader func(path string) (string, bool)

// probe is everything the check needs from the host: a way to run one rung and a way to
// read /proc. The real one runs children; a test supplies canned failures and files.
type probe struct {
	run  func(ctx context.Context, s stage, n needs) *failure
	proc procReader
	arch string // GOARCH of the host being checked
}

// check climbs the ladder and returns at the first rung that fails, classified. It
// returns Ready only if every rung ran clean.
func check(ctx context.Context, n needs, p probe) Readiness {
	r := climb(ctx, n, p)
	r.Arch = p.arch
	r.Tier = tierFloor
	if r.Ready {
		r.Tier = tierNamespaceSeccomp
	}
	r.Warnings = archWarnings(p.arch)
	return r
}

func climb(ctx context.Context, n needs, p probe) Readiness {
	var passed []string
	for _, s := range n.stages() {
		if f := p.run(ctx, s, n); f != nil {
			return classify(*f, p.proc)
		}
		passed = append(passed, string(s))
	}
	return Readiness{Ready: true, Detail: "confined no-op child ran at: " + strings.Join(passed, ", ")}
}

// restrictNamespacesEnforced lists the architectures (GOARCH values) on which systemd
// documents that it enforces RestrictNamespaces=. Elsewhere, and on MIPS where the
// documentation says only "some", the directive is accepted and does nothing.
var restrictNamespacesEnforced = map[string]bool{"386": true, "amd64": true, "ppc64": true, "ppc64le": true, "s390x": true}

// archWarnings reports what a result on this architecture does not prove.
func archWarnings(arch string) []string {
	if restrictNamespacesEnforced[arch] {
		return nil
	}
	return []string{"systemd enforces RestrictNamespaces= only on x86, x86-64, ppc64, ppc64le and s390x (and some MIPS); on " + arch + " the directive does not restrict namespaces, so a unit is not hardened by it and a pass here does not show the unit's namespace list is right"}
}

// CheckConfinement starts a real confined no-op child at each step of the Linux sandbox
// (user, network and mount namespaces, then the re-exec launcher with its read-only host
// view and syscall filter) and reports whether this host allows it. A refusal names the
// cause and the fix: a namespace type blocked by the unit's RestrictNamespaces=, user
// namespaces disabled by sysctl, the AppArmor restriction on Ubuntu 24.04 and later, or
// a seccomp filter that conflicts with the launcher.
//
// The check fails closed. Anything but a clean run, including an error it cannot
// classify, a timeout or an unreadable /proc file, is a refusal, and no environment
// variable, option or file turns it into a pass. Off Linux it reports
// [CauseUnsupportedPlatform].
func CheckConfinement(ctx context.Context) Readiness {
	return checkHost(ctx, needsAll)
}

// classify turns a failed rung and the readable sysctls into a refusal. The rules run
// from the most specific evidence to the least, and a failure no rule recognises falls
// through to [CauseUnknown]: the function has no path that returns Ready.
func classify(f failure, proc procReader) Readiness {
	ev := readEvidence(proc)
	r := Readiness{
		Stage:       string(f.stage),
		Detail:      f.describe(ev),
		Alternative: alternativeTiers,
	}
	denied := f.permissionDenied()
	switch {
	case f.escaped:
		return refuse(r, CauseConfinementIneffective, fixIneffective)
	case f.signal:
		return refuse(r, CauseSeccompConflict, ev.seccompFix())
	case f.stage == stageUser && (ev.maxUserNS == "0" || ev.cloneSysctl == "0" || f.effectiveErrno() == syscall.ENOSPC):
		return refuse(r, CauseUserNamespacesDisabled, ev.userNSFix())
	case f.sawAppArmorDenial():
		return refuse(r, CauseAppArmorUserns, ev.appArmorFix(), alsoFilter(f, ev)...)
	case f.stage == stageUser && denied && ev.filter:
		return refuse(r, CauseNamespaceRestricted, ev.namespacesFix(), appArmorIf(ev)...)
	case denied && ev.apparmor == "1":
		return refuse(r, CauseAppArmorUserns, ev.appArmorFix(), alsoFilter(f, ev)...)
	case (f.stage == stageNet || f.stage == stageMount) && denied && ev.filter:
		return refuse(r, CauseNamespaceRestricted, ev.namespacesFix())
	case launcherBlockedBySeccomp(f, ev):
		return refuse(r, CauseSeccompConflict, ev.seccompFix())
	}
	return refuse(r, CauseUnknown, fixUnknown)
}

// appArmorDenied is the marker AppArmor puts in a denial record. A record that reaches
// the child's output or the start error names the cause outright, with no need to infer it
// from the sysctl.
const appArmorDenied = `apparmor="DENIED"`

// sawAppArmorDenial reports whether the failure carries an AppArmor denial record.
func (f failure) sawAppArmorDenial() bool {
	return strings.Contains(f.output, appArmorDenied) || strings.Contains(f.raw, appArmorDenied)
}

// launcherBlockedBySeccomp reports whether the launcher's own message says a syscall
// filter stopped it: it could not install its filter or set no-new-privileges, or its
// first mount call was refused while this process runs under a filter.
func launcherBlockedBySeccomp(f failure, ev evidence) bool {
	if f.stage != stageLauncher {
		return false
	}
	if strings.Contains(f.output, msgInstallFilter) || strings.Contains(f.output, msgNoNewPrivs) {
		return true
	}
	return strings.Contains(f.output, msgMakePrivate) && f.permissionDenied() && ev.filter
}

// refuse completes a refusal. It is the only constructor of a classified result, and it
// never sets Ready.
func refuse(r Readiness, c Cause, fix string, also ...Cause) Readiness {
	r.Ready = false
	r.Cause = c
	r.Fix = fix
	r.Also = also
	return r
}

// appArmorIf lists the AppArmor restriction as a second candidate when its sysctl is on,
// because it and RestrictNamespaces both answer EPERM.
func appArmorIf(ev evidence) []Cause {
	if ev.apparmor == "1" {
		return []Cause{CauseAppArmorUserns}
	}
	return nil
}

// alsoFilter lists the inherited seccomp filter as a second candidate for an AppArmor
// refusal, when a filter is active in this process.
func alsoFilter(f failure, ev evidence) []Cause {
	if !ev.filter {
		return nil
	}
	if f.stage == stageLauncher {
		return []Cause{CauseSeccompConflict}
	}
	return []Cause{CauseNamespaceRestricted}
}

// evidence is what the readable /proc files say. An empty string is a file that could
// not be read.
type evidence struct {
	maxUserNS   string // user.max_user_namespaces
	cloneSysctl string // kernel.unprivileged_userns_clone (Debian kernels)
	apparmor    string // kernel.apparmor_restrict_unprivileged_userns (Ubuntu 24.04+)
	filter      bool   // this process runs under a seccomp filter (Seccomp: 2)
	container   bool   // a Docker or Podman marker file is present
}

func readEvidence(proc procReader) evidence {
	if proc == nil {
		return evidence{}
	}
	get := func(path string) string {
		v, ok := proc(path)
		if !ok {
			return ""
		}
		return strings.TrimSpace(v)
	}
	ev := evidence{
		maxUserNS:   get(procMaxUserNS),
		cloneSysctl: get(procCloneSysctl),
		apparmor:    get(procAppArmor),
	}
	for _, marker := range []string{markerDocker, markerPodman} {
		if _, ok := proc(marker); ok {
			ev.container = true
		}
	}
	for _, line := range strings.Split(get(procSelfStatus), "\n") {
		if v, ok := strings.CutPrefix(line, "Seccomp:"); ok && strings.TrimSpace(v) == "2" {
			ev.filter = true
		}
	}
	return ev
}

// userNSFix names the sysctl that is actually off, so the operator is not sent to
// change one the host already has right.
func (ev evidence) userNSFix() string {
	switch {
	case ev.cloneSysctl == "0":
		return "set kernel.unprivileged_userns_clone=1 (sysctl -w kernel.unprivileged_userns_clone=1, and persist it in /etc/sysctl.d/)"
	case ev.maxUserNS == "0":
		return "raise user.max_user_namespaces above 0 (sysctl -w user.max_user_namespaces=15000, and persist it in /etc/sysctl.d/)"
	}
	return "user namespaces are exhausted or unavailable: raise user.max_user_namespaces (sysctl -w user.max_user_namespaces=15000), and check that the kernel was built with CONFIG_USER_NS"
}

// namespacesFix, seccompFix and appArmorFix send a container to the runtime profiles,
// because inside one the filter and the AppArmor profile come from the runtime, and
// neither a unit setting nor a per-binary host profile reaches them.
func (ev evidence) namespacesFix() string {
	if ev.container {
		return fixContainer
	}
	return fixNamespaces
}

func (ev evidence) appArmorFix() string {
	if ev.container {
		return fixContainer
	}
	return fixAppArmor
}

func (ev evidence) seccompFix() string {
	if ev.container {
		return fixContainer
	}
	return fixSeccomp
}

// describe renders the failed rung and the sysctl values the decision rested on.
func (f failure) describe(ev evidence) string {
	var parts []string
	what := f.raw
	switch {
	case f.signal:
		what = "child killed by SIGSYS (bad system call)"
	case what == "" && f.output != "":
		what = strings.TrimSpace(f.output)
	case what == "":
		what = "child did not run to a clean exit"
	}
	parts = append(parts, fmt.Sprintf("%s failed: %s", f.stage, what))
	if f.raw != "" && f.output != "" {
		parts = append(parts, "output: "+strings.TrimSpace(f.output))
	}
	for _, kv := range []struct{ name, val string }{
		{"user.max_user_namespaces", ev.maxUserNS},
		{"kernel.unprivileged_userns_clone", ev.cloneSysctl},
		{"kernel.apparmor_restrict_unprivileged_userns", ev.apparmor},
	} {
		if kv.val != "" {
			parts = append(parts, kv.name+"="+kv.val)
		}
	}
	if ev.filter {
		parts = append(parts, "seccomp filter active in this process")
	}
	if ev.container {
		parts = append(parts, "running in a container")
	}
	return strings.Join(parts, "; ")
}

// effectiveErrno is the failure's errno, recovered from the launcher's message when the
// child started and printed the error rather than failing to start.
func (f failure) effectiveErrno() syscall.Errno {
	if f.errno != 0 {
		return f.errno
	}
	return errnoFromText(f.output)
}

// permissionDenied reports EPERM or EACCES, the two errnos a refused namespace, a
// refused mount and a refused filter all use.
func (f failure) permissionDenied() bool {
	e := f.effectiveErrno()
	return e == syscall.EPERM || e == syscall.EACCES
}

// errnoFromText maps the strerror text of the errnos the classifier acts on back to
// the errno. The strings are fixed here rather than taken from [syscall.Errno] so the
// mapping is the same on every OS the tests run on.
func errnoFromText(s string) syscall.Errno {
	s = strings.ToLower(s)
	for _, m := range []struct {
		text  string
		errno syscall.Errno
	}{
		{"operation not permitted", syscall.EPERM},
		{"permission denied", syscall.EACCES},
		{"no space left on device", syscall.ENOSPC},
		{"invalid argument", syscall.EINVAL},
	} {
		if strings.Contains(s, m.text) {
			return m.errno
		}
	}
	return 0
}

// The fixes and the alternative are fixed text, one per cause, so an operator reads the
// same instruction for the same refusal everywhere.
const (
	fixNamespaces  = "the service unit restricts namespaces, or filters a system call that starting a child needs (both answer EPERM). Allow exactly the types the sandbox creates by setting RestrictNamespaces=user net mnt in the unit (the Flynn runner unit does), and keep the SystemCallFilter= groups of the runner unit, then run systemctl daemon-reload and restart the service. Do not set RestrictNamespaces=no, and do not turn confinement off"
	fixAppArmor    = "AppArmor denies capabilities inside unprivileged user namespaces for programs without a profile that grants them. Install the per-binary AppArmor profile Flynn ships (deploy/apparmor/flynn, which grants userns to the flynn binary and nothing else) with the path in its header set to this binary, then run apparmor_parser -r on it. Do not set kernel.apparmor_restrict_unprivileged_userns=0: that lifts the restriction for every program on the host"
	fixSeccomp     = "a seccomp filter from the service unit or the container runtime blocks a call the launcher needs. In a systemd unit, allow mount after the @privileged exclusion with SystemCallFilter=mount (the Flynn runner unit does) and set SystemCallErrorNumber=EPERM so a refused call fails instead of killing the process"
	fixContainer   = "this process runs in a container whose runtime profiles block the sandbox: Docker's default seccomp profile allows clone with namespace flags, unshare and mount only with CAP_SYS_ADMIN, and on an AppArmor host (Ubuntu, Debian) its docker-default AppArmor profile denies every mount. Run the container with the profiles Flynn publishes with each release, each Docker's own with only that change: --security-opt seccomp=flynn-seccomp.json (deploy/seccomp/flynn.json in the source), and on an AppArmor host load flynn-apparmor-container (deploy/apparmor/flynn-container) with apparmor_parser -r and add --security-opt apparmor=flynn-container. On Kubernetes, install them on each node and set a Localhost seccompProfile and appArmorProfile. Do not use seccomp=unconfined or apparmor=unconfined, and do not add CAP_SYS_ADMIN: each lifts far more than the sandbox needs"
	fixUnknown     = "confinement could not be established and the reason is not one Flynn recognises, so it refuses to run unconfined. Read the detail, run flynn sandbox check --json on the host, and report the output"
	fixIneffective = "the sandbox started a command but did not confine it, so Flynn refuses to run commands. Do not run agent commands on this host; use the container tier or the microVM tier, and report the detail of flynn sandbox check --json"
	fixPlatform    = "the namespace sandbox is Linux-only; on this platform use the platform's own confinement or a stronger tier"

	alternativeTiers = "run the agent in the container tier (SelectContainerDriver, RunContainer) or the microVM tier (SelectDriver, BootMicroVM), which do not need unprivileged namespaces from the host process"
)

package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"syscall"
	"testing"
)

// TestSelfTestNamesDenialClass holds each denial class the check can see to its own cause
// and to a fix that names the class: an EPERM from unshare under a filter, a SIGSYS, an
// AppArmor denial record in the output, and a canary that ran when it should have been
// stopped.
func TestSelfTestNamesDenialClass(t *testing.T) {
	healthy := fakeProc{procMaxUserNS: "15000\n", procCloneSysctl: "1\n", procSelfStatus: statusUnfiltered}
	tests := []struct {
		name   string
		f      failure
		proc   fakeProc
		cause  Cause
		fixHas string
	}{
		{
			name:   "EPERM from unshare under the unit's filter",
			f:      failure{stage: stageUser, errno: syscall.EPERM, raw: "fork/exec /bin/sh: operation not permitted"},
			proc:   fakeProc{procMaxUserNS: "15000", procSelfStatus: statusFiltered},
			cause:  CauseNamespaceRestricted,
			fixHas: "RestrictNamespaces",
		},
		{
			name:   "SIGSYS from the filter",
			f:      failure{stage: stageLauncher, signal: true, raw: "signal: bad system call"},
			proc:   healthy,
			cause:  CauseSeccompConflict,
			fixHas: "SystemCallErrorNumber=EPERM",
		},
		{
			name: "an AppArmor denial record names AppArmor with no sysctl to infer it from",
			f: failure{
				stage:  stageLauncher,
				raw:    "exit status 126",
				output: `audit: type=1400 apparmor="DENIED" operation="capable" class="cap" profile="unprivileged_userns" capname="sys_admin"`,
			},
			proc:   healthy,
			cause:  CauseAppArmorUserns,
			fixHas: "deploy/apparmor/flynn",
		},
		{
			name:   "a canary the confinement did not stop",
			f:      failure{stage: stageCanary, escaped: true, raw: "a command could write outside the working directory"},
			proc:   healthy,
			cause:  CauseConfinementIneffective,
			fixHas: "container tier",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.f, tc.proc.read)
			if got.Ready || got.Cause != tc.cause {
				t.Fatalf("got ready=%v cause=%q, want a refusal with cause %q\n%+v", got.Ready, got.Cause, tc.cause, got)
			}
			if !strings.Contains(got.Fix, tc.fixHas) {
				t.Errorf("fix %q does not contain %q", got.Fix, tc.fixHas)
			}
			assertPointsToStrongerTiers(t, got)
		})
	}
}

// TestRemedyNeverNamesGlobalSysctl holds every remedy to the narrow fix. The one place the
// text may mention the AppArmor sysctl is the warning not to set it; with that sentence
// removed, no remedy refers to it.
func TestRemedyNeverNamesGlobalSysctl(t *testing.T) {
	const warning = "Do not set kernel.apparmor_restrict_unprivileged_userns=0: that lifts the restriction for every program on the host"
	fixes := map[string]string{
		"namespaces": fixNamespaces, "apparmor": fixAppArmor, "seccomp": fixSeccomp,
		"unknown": fixUnknown, "ineffective": fixIneffective, "platform": fixPlatform,
		"userns": (evidence{}).userNSFix(), "userns with the sysctl": (evidence{maxUserNS: "0"}).userNSFix(),
	}
	for name, fix := range fixes {
		if rest := strings.ReplaceAll(fix, warning, ""); strings.Contains(rest, "apparmor_restrict_unprivileged_userns") {
			t.Errorf("%s: the remedy refers to the global AppArmor sysctl outside the warning against it: %q", name, fix)
		}
	}
	if !strings.Contains(fixAppArmor, warning) {
		t.Error("the AppArmor remedy must warn against the global switch")
	}
}

// TestReadinessRecordsArchAndTier holds the record a supervisor reads to the facts it was
// measured on: the architecture, and the tier the probe delivered rather than the one
// requested.
func TestReadinessRecordsArchAndTier(t *testing.T) {
	pass := func(context.Context, stage, needs) *failure { return nil }
	fail := func(_ context.Context, s stage, _ needs) *failure {
		return &failure{stage: s, errno: syscall.EPERM, raw: "operation not permitted"}
	}
	for _, tc := range []struct {
		name     string
		run      func(context.Context, stage, needs) *failure
		wantTier string
	}{
		{"every rung ran", pass, tierNamespaceSeccomp},
		{"a rung failed", fail, tierFloor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := check(context.Background(), needsAll, probe{run: tc.run, proc: fakeProc{}.read, arch: "arm64"})
			if got.Tier != tc.wantTier || got.Arch != "arm64" {
				t.Fatalf("tier=%q arch=%q, want %q and arm64", got.Tier, got.Arch, tc.wantTier)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var rec map[string]any
			if err := json.Unmarshal(b, &rec); err != nil {
				t.Fatal(err)
			}
			if rec["arch"] != "arm64" || rec["tier"] != tc.wantTier {
				t.Fatalf("the JSON record lost the architecture or the tier: %s", b)
			}
		})
	}
}

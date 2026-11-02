//go:build linux

package sandbox_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

// expectTierEnv names the tier a CI leg asserts. Left unset the test only records what the
// host reached, so a developer machine is never failed for being locked down.
const expectTierEnv = "FLYNN_EXPECT_CONFINEMENT_TIER"

// TestHostConfinementTierIsMeasured records which confinement tier this host reaches and
// holds it to an expected value when a CI leg names one. Every other containment test
// either skips or passes without saying which tier it ran against, so a green run could
// be proving the process-jail floor. Hosted Ubuntu 24.04 runners set
// kernel.apparmor_restrict_unprivileged_userns=1, which refuses the user namespace the
// kernel tier needs, so the tier a runner reaches depends on a sysctl the workflow
// controls and an image update can change.
//
// The log line carries the kernel, the AppArmor sysctl and the tier from both the
// readiness check and the sandbox a caller would build, so the CI log is the measurement.
// The two must agree: a record that named one tier while the check measured the other
// would mislabel every governed run that stores it.
func TestHostConfinementTierIsMeasured(t *testing.T) {
	r := sandbox.CheckConfinement(context.Background())
	sb, err := sandbox.NewLocal(t.TempDir(), sandbox.WithDefaultConfinement(), sandbox.WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	t.Logf("confinement tier: check=%q sandbox=%q ready=%v cause=%q stage=%q kernel=%s apparmor_restrict_unprivileged_userns=%s arch=%s",
		r.Tier, sb.ConfinementTier(), r.Ready, r.Cause, r.Stage, kernelRelease(), sysctlValue("kernel/apparmor_restrict_unprivileged_userns"), r.Arch)
	if !r.Ready {
		t.Logf("why not the kernel tier: %s", r.Detail)
	}

	if r.Tier != sb.ConfinementTier() {
		t.Errorf("the readiness check measured tier %q but the sandbox reports %q", r.Tier, sb.ConfinementTier())
	}
	if want := os.Getenv(expectTierEnv); want != "" && r.Tier != want {
		t.Errorf("this host reached the %q tier, the CI leg expects %q (%s): %s", r.Tier, want, r.Cause, r.Detail)
	}
}

// sysctlValue reads one /proc/sys entry, or "absent" where the host does not have it.
func sysctlValue(name string) string {
	b, err := os.ReadFile("/proc/sys/" + name)
	if err != nil {
		return "absent"
	}
	return strings.TrimSpace(string(b))
}

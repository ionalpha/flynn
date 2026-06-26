//go:build linux

package sandbox

import (
	"context"
	"os"
	"testing"
)

// TestKernelConfinementProvenOnCI guards against a hollow green run. The Linux
// confinement tests skip where unprivileged user namespaces are unavailable, which is
// the right behaviour on a locked-down developer box. On CI it is a trap: if the
// runner ever stops allowing unprivileged user namespaces, every confinement test
// skips at once and the suite passes while proving nothing. This canary turns that
// silent skip into a loud failure on CI, so a green containment result always means
// the kernel boundary was actually exercised, never merely skipped.
//
// It reuses the same network-deny path and the same namespaceUnavailable predicate as
// the confinement tests, so the canary and the tests it guards cannot drift apart.
func TestKernelConfinementProvenOnCI(t *testing.T) {
	onCI := os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != ""

	denied, err := NewLocal(t.TempDir(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}
	// A non-loopback connect must fail under network deny; a non-zero exit is the pass.
	res, err := denied.Exec(context.Background(), Command{Line: "timeout 3 bash -c 'exec 3<>/dev/tcp/8.8.8.8/53' 2>&1"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			if onCI {
				t.Fatalf("kernel confinement is not enforceable on this CI runner: unprivileged user namespaces are unavailable, so the confinement tests would all skip and a green run would prove nothing. The runner must allow unprivileged user namespaces. underlying error: %v", err)
			}
			t.Skip("unprivileged user namespaces unavailable on this host (local, not CI)")
		}
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("an outbound connect must fail under network deny, but it succeeded:\n%s", res.Output)
	}
}

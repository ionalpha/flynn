//go:build darwin

package sandbox

import (
	"context"
	"os"
	"testing"
)

// TestOutboundReachableUnconfinedOnCI guards against a hollow green run of the macOS
// network-deny test. That test confirms the denial only by also checking that the same
// command can reach the network unconfined; on a runner with no outbound path it skips
// instead, so a green suite would prove nothing about the deny path. This canary turns
// that condition into a loud failure on CI: it reuses the same unconfined probe and, when
// running on CI, fails if outbound network is not reachable, so a green containment result
// always means the deny test was able to observe a real denial rather than skip. Locally
// it stays lenient, since a developer box may legitimately have no network.
func TestOutboundReachableUnconfinedOnCI(t *testing.T) {
	onCI := os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != ""
	if !onCI {
		t.Skip("not on CI; an unconfined outbound check is only meaningful where the network is guaranteed")
	}

	// The same unconfined probe the network-deny test uses for its distinguishing check.
	open, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := open.Exec(context.Background(), Command{Line: "curl --max-time 5 -sS http://1.1.1.1 >/dev/null 2>&1"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("outbound network is not reachable unconfined on this CI runner, so the network-deny test cannot distinguish a real denial from a runner with no network and would skip, leaving a green run that proves nothing. The runner must have outbound network. probe output:\n%s", res.Output)
	}
}

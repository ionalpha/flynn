package sandbox

import (
	"os/exec"
	"testing"

	"github.com/ionalpha/flynn/procs"
)

// runCounted must not count a spawn whose start fails: a process that never ran is not a
// live child, and the error path must not leave the registry elevated. The child count
// exists to surface leaked children, so a phantom count from a failed exec would be a
// false signal. The registry increment is therefore bracketed around a confirmed start,
// not the attempt.
func TestRunCountedDoesNotCountAFailedStart(t *testing.T) {
	before := procs.Live()
	err := runCounted(exec.Command("flynn-no-such-binary-should-exist"))
	if err == nil {
		t.Fatal("a start that cannot find its binary must return an error")
	}
	if got := procs.Live(); got != before {
		t.Fatalf("a failed start moved the live count: %d -> %d", before, got)
	}
}

// Package sandboxtest holds the helper tests use to run only where the host can confine.
//
// Production code that launches a command with [sandbox.WithDefaultConfinement] refuses on
// a host that cannot confine, so a test that runs a real command through it needs the same
// host. The helper asks the host the way the runner does, with the self-test, and skips
// with the classified reason, so a skipped test says why instead of failing on a refusal.
package sandboxtest

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

// RequireConfinement skips the test when [sandbox.CheckConfinement] refuses the host. It
// never relaxes confinement: the code under test still refuses on a host that cannot
// confine, and this only keeps that refusal out of tests that are not about it. A platform
// the self-test does not cover ([sandbox.CauseUnsupportedPlatform]) is not skipped, since
// its own confinement runs the test as before.
func RequireConfinement(tb testing.TB) {
	tb.Helper()
	r := sandbox.CheckConfinement(context.Background())
	if !r.Ready && r.Cause != sandbox.CauseUnsupportedPlatform {
		tb.Skipf("this host cannot confine commands (%s): %s", r.Cause, r.Detail)
	}
}

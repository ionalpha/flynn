// Package sandboxtest holds the helper tests use to run only where the host can confine.
//
// Production code that launches a command with [sandbox.WithDefaultConfinement] refuses on
// a host that cannot confine, so a test that runs a real command through it needs the same
// host. The helper asks the host the way the runner does, with the self-test, and skips
// with the classified reason, so a skipped test says why instead of failing on a refusal.
package sandboxtest

import (
	"context"
	"os"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

// RequireEnv names the variable that turns [RequireConfinement]'s skip into a failure.
// CI sets it on the Linux runners after lifting the user-namespace restriction, so a
// runner image that stops allowing confinement fails the suite instead of skipping every
// test that needs it and reporting green.
const RequireEnv = "FLYNN_TEST_REQUIRE_CONFINEMENT"

// RequireConfinement skips the test when [sandbox.CheckConfinement] refuses the host, or
// fails it when [RequireEnv] is set. It never relaxes confinement: the code under test
// still refuses on a host that cannot confine, and this only keeps that refusal out of
// tests that are not about it. A platform with no kernel confinement
// ([sandbox.CauseUnsupportedPlatform]) is skipped too, since
// [sandbox.WithDefaultConfinement] refuses every launch there.
func RequireConfinement(tb testing.TB) {
	tb.Helper()
	r := sandbox.CheckConfinement(context.Background())
	if r.Ready {
		return
	}
	if os.Getenv(RequireEnv) != "" {
		tb.Fatalf("%s is set but this host cannot confine commands (%s): %s", RequireEnv, r.Cause, r.Detail)
	}
	tb.Skipf("this host cannot confine commands (%s): %s", r.Cause, r.Detail)
}

// Package sandboxtest skips a test on a host that cannot confine, using the
// runner's self-test, so the skip gives the classified reason.
package sandboxtest

import (
	"context"
	"os"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

// RequireEnv turns [RequireConfinement]'s skip into a failure. CI sets it on Linux,
// so a runner image that stops allowing confinement fails instead of going green.
const RequireEnv = "FLYNN_TEST_REQUIRE_CONFINEMENT"

// RequireConfinement skips when [sandbox.CheckConfinement] refuses the host,
// including [sandbox.CauseUnsupportedPlatform], or fails when [RequireEnv] is set.
// It never relaxes confinement.
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

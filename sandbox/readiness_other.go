//go:build !linux

package sandbox

import (
	"context"
	"runtime"
)

// checkHost reports the namespace sandbox as unavailable: it is built from Linux
// namespaces, so no other platform can pass this check. The platforms that confine
// commands another way do so through their own mechanisms, not this ladder.
func checkHost(_ context.Context, _ needs) Readiness {
	return Readiness{
		Cause:       CauseUnsupportedPlatform,
		Detail:      "the namespace sandbox is not available on " + runtime.GOOS,
		Fix:         fixPlatform,
		Alternative: alternativeTiers,
		Arch:        runtime.GOARCH,
	}
}

// forgetConfinementPass has nothing to forget off Linux: there is no gate to remember a
// pass.
func (*Local) forgetConfinementPass() {}

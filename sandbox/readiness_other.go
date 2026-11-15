//go:build !linux

package sandbox

import (
	"context"
	"runtime"
)

// checkHost reports unavailable, because the namespace sandbox is Linux-only.
func checkHost(_ context.Context, _ needs) Readiness {
	return Readiness{
		Cause:       CauseUnsupportedPlatform,
		Detail:      "the namespace sandbox is not available on " + runtime.GOOS,
		Fix:         fixPlatform,
		Alternative: alternativeTiers,
		Arch:        runtime.GOARCH,
	}
}

// forgetConfinementPass is a no-op, because no gate remembers a pass here.
func (*Local) forgetConfinementPass() {}

package dependency

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// defaultLookPath resolves a program name on PATH. It is the manager's default detector;
// tests inject their own.
func defaultLookPath(name string) (string, error) { return exec.LookPath(name) }

// probeTimeout bounds a version probe so a hung or misbehaving binary cannot stall a
// reconcile. A version print is near-instant; this is a generous backstop.
const probeTimeout = 10 * time.Second

// SystemProber runs a discovered binary to read its version. The invocation is fixed: the
// resolved path plus the spec's literal version arguments, executed directly without a
// shell, so there is no command string for a name or argument to be interpreted from, and
// it is bounded by a timeout. It is the version-check the detect-installed-first policy uses
// on a host binary; running a program to do real work is a separate, sandbox-confined path.
type SystemProber struct{}

// Probe runs path with args and returns its combined output. Many tools print their version
// to stdout, some to stderr, so both are captured. A nonzero exit or a timeout is returned
// as an error, which the manager treats as "not usable" rather than trusting the binary.
func (SystemProber) Probe(ctx context.Context, path string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// #nosec G204 -- path is a binary resolved from PATH (or a Flynn-installed build) and
	// args are the spec's literal version flags; nothing here is shell-interpreted.
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return "", fault.Wrap(fault.Transient, "dependency_probe", err)
	}
	return strings.TrimSpace(string(out)), nil
}

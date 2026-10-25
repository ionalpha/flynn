package externagent

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// cliProbe is the first step of every adapter's Detect: ask the CLI for its version through
// the sandbox spawner.
type cliProbe struct {
	// name is the adapter's name as it reads in a reason ("claude", "codex").
	name string
	// bin is the executable the adapter would run.
	bin string
	// spawner runs the probe. Nil is a refusal: detection has no way to run anything.
	spawner Spawner
	// notFound is the reason given when the CLI is not on disk, which is where each
	// vendor's install instruction differs.
	notFound string
}

// probeVersion runs the version probe and reports either the trimmed version or the
// [Readiness] that ends detection. A nil spawner is a refusal. A failed probe is
// unreadiness and not a Go error, so the caller can onboard instead of crashing; a CLI that
// is on disk but would not run is reported apart from one that is absent, because telling a
// user to install what they already have sends them the wrong way.
func (p cliProbe) probeVersion(ctx context.Context) (version string, stop *Readiness) {
	if p.spawner == nil {
		return "", &Readiness{Refuse: true, Reason: p.name + " detection needs a process spawner"}
	}
	out, err := p.spawner.Probe(ctx, p.bin, "--version")
	if err != nil {
		if _, statErr := os.Stat(p.bin); statErr == nil {
			return "", &Readiness{Reason: fmt.Sprintf("%s is installed at %s but the confined child could not run it: %v", p.name, p.bin, err)}
		}
		return "", &Readiness{Reason: p.notFound}
	}
	return strings.TrimSpace(out), nil
}

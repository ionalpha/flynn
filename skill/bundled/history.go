package bundled

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"
)

// packHistory lists the [digest] of every version of every bundled skill the pack has
// carried, one "slug digest" pair a line. dev/packhistory writes it from git history,
// through the same function that fingerprints a stored skill, so a copy seeded before
// records existed can be matched against it.
//
//go:embed pack_history.txt
var packHistory string

// shippedDigests parses [packHistory] once. The file is part of the build, so a
// malformed line fails every start and the package's tests, not one skill.
var shippedDigests = sync.OnceValues(func() (digestSet, error) { return parseHistory(packHistory) })

func parseHistory(text string) (digestSet, error) {
	out := digestSet{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 || len(f[1]) != 64 {
			return nil, fmt.Errorf("bundled: pack_history.txt line %d: want \"slug digest\", got %q", i+1, line)
		}
		out[f[1]] = struct{}{}
	}
	return out, nil
}

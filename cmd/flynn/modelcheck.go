package main

import (
	"context"
	"fmt"
	"io"
	"os"
	gruntime "runtime"
	"strings"

	"github.com/ionalpha/flynn/inference"
	"github.com/ionalpha/flynn/inference/provision"
	"github.com/ionalpha/flynn/sandbox"
)

// runRuntimeCheck reports, for each local inference runtime Flynn knows, whether it is
// installed and whether its version is exposed to a known model-parser advisory. It is
// the security gate made visible: the same check the run path uses before launching a
// runtime, surfaced as a command. A vulnerable runtime is reported with the advisory
// so it can be updated; an absent one is simply not installed.
func runRuntimeCheck(out io.Writer) error {
	ctx := context.Background()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	sb, err := sandbox.NewLocal(cwd)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintln(out, "local inference runtimes:")
	for _, rt := range inference.Runtimes() {
		ver, ok := detectRuntimeVersion(ctx, sb, rt)
		switch {
		case !ok:
			if _, can := provision.ReleaseFor(rt.Name, gruntime.GOOS, gruntime.GOARCH); can {
				_, _ = fmt.Fprintf(out, "  %-10s not installed (run `flynn models install %s` to fetch a pinned build)\n", rt.Name, rt.Name)
			} else {
				_, _ = fmt.Fprintf(out, "  %-10s not installed\n", rt.Name)
			}
		case inference.SafeToRun(rt.Name, ver) != nil:
			_, _ = fmt.Fprintf(out, "  %-10s %-8s VULNERABLE: %s (update the runtime)\n", rt.Name, ver, concern(rt.Name, ver))
		default:
			_, _ = fmt.Fprintf(out, "  %-10s %-8s ok\n", rt.Name, ver)
		}
	}
	return nil
}

// detectRuntimeVersion finds an installed binary for the runtime and reads its version
// by running it through the sandbox boundary. It returns the parsed version and whether
// a working binary was found.
func detectRuntimeVersion(ctx context.Context, sb sandbox.Sandbox, rt inference.Runtime) (inference.Version, bool) {
	for _, bin := range rt.Binaries {
		line := bin + " " + strings.Join(rt.VersionArgs, " ")
		res, err := sb.Exec(ctx, sandbox.Command{Line: line})
		if err != nil || res.ExitCode != 0 {
			continue
		}
		ver := rt.ParseVersion(res.Output)
		if len(ver) == 0 {
			continue
		}
		return ver, true
	}
	return nil, false
}

// concern explains why a runtime version is unsafe, for the report: the named
// advisories it is exposed to, or, when it is below the floor with no named advisory,
// that it predates the minimum supported version.
func concern(runtime string, v inference.Version) string {
	ex := inference.Exposure(runtime, v, inference.Advisories())
	if len(ex) > 0 {
		ids := make([]string, len(ex))
		for i, a := range ex {
			ids[i] = a.ID
		}
		return strings.Join(ids, ", ")
	}
	if floor, ok := inference.MinSupportedFor(runtime); ok {
		return "older than minimum supported " + floor.String()
	}
	return "unsafe"
}
